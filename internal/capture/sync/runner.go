package sync

import (
	"context"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

// Cadence (SPEC §6.7): a 30 s tick; offline and 429 back off from 30 s,
// doubling to 15 min. A Retry-After can lengthen the wait, never shorten it.
const (
	Tick       = 30 * time.Second
	MinBackoff = 30 * time.Second
	MaxBackoff = 15 * time.Minute
)

// Backoff is the doubling backoff state.
type Backoff struct{ cur time.Duration }

// Next returns the delay after a failed pass: max(Retry-After, doubled backoff).
func (b *Backoff) Next(retryAfter time.Duration) time.Duration {
	if b.cur == 0 {
		b.cur = MinBackoff
	} else {
		b.cur *= 2
	}
	if b.cur > MaxBackoff {
		b.cur = MaxBackoff
	}
	return max(retryAfter, b.cur)
}

// Reset clears the backoff after a good pass.
func (b *Backoff) Reset() { b.cur = 0 }

// Runner drains one Project's store in the background of atlas-cli run and
// atlas-cli serve.
type Runner struct {
	Client *Client
	Store  *store.Store
	Now    func() time.Time
	// OnPass, when set, sees every pass that did something.
	OnPass func(Report, error)

	force chan struct{}
}

// NewRunner returns a runner; call Run in a goroutine.
func NewRunner(c *Client, st *store.Store) *Runner {
	return &Runner{Client: c, Store: st, Now: time.Now, force: make(chan struct{}, 1)}
}

// Force asks for a drain now, ignoring the backoff (a human action).
func (r *Runner) Force() {
	select {
	case r.force <- struct{}{}:
	default:
	}
}

// RetryFailed requeues failed rows and forces a drain (atlas-cli capture retry).
func (r *Runner) RetryFailed(ctx context.Context) (int64, error) {
	n, err := r.Store.RetryFailed(ctx)
	if err == nil {
		r.Force()
	}
	return n, err
}

// Run drains at start, on every tick once the backoff allows, and on Force,
// until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	var bo Backoff
	next := time.Time{}
	tick := time.NewTicker(Tick)
	defer tick.Stop()
	pass := func(forced bool) {
		if !forced && r.Now().Before(next) {
			return
		}
		rep, err := Drain(ctx, r.Client, r.Store, Budget, r.Now)
		switch {
		case err != nil, rep.Outcome == Offline, rep.Outcome == RateLimited, rep.Outcome == SignedOut:
			next = r.Now().Add(bo.Next(rep.RetryAfter))
		default:
			bo.Reset()
			next = time.Time{}
		}
		if r.OnPass != nil && (err != nil || rep.Sent > 0 || rep.Failed > 0 || rep.Outcome > Done) {
			r.OnPass(rep, err)
		}
	}
	pass(false)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			pass(false)
		case <-r.force:
			pass(true)
		}
	}
}
