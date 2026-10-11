package sync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/ingest"
)

// Batch limits (SPEC §6.7).
const (
	MaxArtifacts = 100
	MaxBatch     = 4 << 20
	// Budget bounds one drain pass; the next tick carries on.
	Budget  = 3 * time.Second
	timeout = 30 * time.Second
	page    = 400
)

// Outcome is how a drain pass ended.
type Outcome int

// Outcomes.
const (
	Idle          Outcome = iota // gates closed: not a cloud binding
	Done                         // nothing left, or the budget ran out without errors
	Offline                      // 5xx or no answer
	RateLimited                  // 429
	NotAuthorized                // 403: drain_state = not_authorized
	SignedOut                    // no token, or a 401 after the one refresh
)

func (o Outcome) String() string {
	return [...]string{"idle", "done", "offline", "rate limited", "not authorized", "signed out"}[o]
}

// Report counts one pass.
type Report struct {
	Outcome    Outcome
	Sent       int
	Failed     int
	Retired    int64
	RetryAfter time.Duration // from a 429
}

// Client is the HTTP side of the drain.
type Client struct {
	Base   string
	Tokens ingest.Tokens
	HTTP   *http.Client
}

// NewClient returns a client with the 30 s timeout.
func NewClient(base string, tokens ingest.Tokens) *Client {
	return &Client{Base: strings.TrimRight(base, "/"), Tokens: tokens, HTTP: &http.Client{Timeout: timeout}}
}

// stop ends a pass early with an outcome.
type stop struct {
	outcome Outcome
	after   time.Duration
}

func (s *stop) Error() string { return s.outcome.String() }

// pass is one drain: it refreshes the JWT at most once.
type pass struct {
	c         *Client
	st        *store.Store
	refreshed bool
	rep       *Report
	now       func() time.Time
}

// call sends one request. A 401 invalidates the token and retries once per
// pass. The Authorization header is never logged or echoed.
func (p *pass) call(ctx context.Context, method, path, ctype string, body []byte) (int, http.Header, []byte, error) {
	for {
		jwt, err := p.c.Tokens.Token(ctx)
		if err != nil {
			return 0, nil, nil, &stop{outcome: SignedOut}
		}
		req, err := http.NewRequestWithContext(ctx, method, p.c.Base+path, bytes.NewReader(body))
		if err != nil {
			return 0, nil, nil, err
		}
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Content-Type", ctype)
		resp, err := p.c.HTTP.Do(req)
		if err != nil {
			return 0, nil, nil, &stop{outcome: Offline}
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			if p.refreshed {
				return 0, nil, nil, &stop{outcome: SignedOut}
			}
			p.refreshed = true
			p.c.Tokens.Invalidate()
			continue
		}
		return resp.StatusCode, resp.Header, b, nil
	}
}

// common maps the statuses every call shares to a stop.
func (p *pass) common(ctx context.Context, code int, hdr http.Header) error {
	switch {
	case code == http.StatusForbidden:
		_ = p.st.SetDrainState(ctx, store.DrainNotAuthorized, p.now())
		return &stop{outcome: NotAuthorized}
	case code == http.StatusTooManyRequests:
		return &stop{outcome: RateLimited, after: RetryAfter(hdr)}
	case code >= 500:
		return &stop{outcome: Offline}
	}
	return nil
}

// RetryAfter reads a Retry-After given in seconds.
func RetryAfter(h http.Header) time.Duration {
	if h == nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After")))
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// putBlob uploads one blob: 2xx stored, 409 already present, 413 fails the row.
func (p *pass) putBlob(ctx context.Context, key string, data []byte) (bool, error) {
	code, hdr, _, err := p.call(ctx, http.MethodPut, "/blobs/"+key, "application/octet-stream", data)
	if err != nil {
		return false, err
	}
	if err := p.common(ctx, code, hdr); err != nil {
		return false, err
	}
	switch {
	case code >= 200 && code < 300, code == http.StatusConflict:
		return true, nil
	case code == http.StatusRequestEntityTooLarge:
		return false, nil
	}
	return false, fmt.Errorf("blob upload answered %d", code)
}

type ingestResults struct {
	Results []struct {
		RowID    string `json:"rowId"`
		Accepted bool   `json:"accepted"`
	} `json:"results"`
}

// post sends a batch. Another 4xx bisects the batch to find the poison row,
// which fails on its own.
func (p *pass) post(ctx context.Context, batch []item) error {
	if len(batch) == 0 {
		return nil
	}
	body := encode(batch)
	code, hdr, resp, err := p.call(ctx, http.MethodPost, "/ingest", "application/json", body)
	if err != nil {
		return err
	}
	if err := p.common(ctx, code, hdr); err != nil {
		return err
	}
	if code >= 200 && code < 300 {
		return p.apply(ctx, batch, resp)
	}
	if len(batch) == 1 {
		p.rep.Failed++
		return p.st.MarkFailed(ctx, batch[0].kind, batch[0].id)
	}
	mid := len(batch) / 2
	if err := p.post(ctx, batch[:mid]); err != nil {
		return err
	}
	return p.post(ctx, batch[mid:])
}

// apply records per-row results; an empty body accepts the whole batch, and
// only a row answered "accepted":false counts an attempt.
func (p *pass) apply(ctx context.Context, batch []item, resp []byte) error {
	var res ingestResults
	if len(bytes.TrimSpace(resp)) > 0 {
		_ = json.Unmarshal(resp, &res)
	}
	if len(res.Results) == 0 {
		return p.sent(ctx, batch)
	}
	rejected := map[string]bool{}
	for _, r := range res.Results {
		rejected[r.RowID] = !r.Accepted
	}
	var ok []item
	for _, it := range batch {
		if !rejected[it.id] {
			ok = append(ok, it)
			continue
		}
		if err := p.st.BumpAttempts(ctx, it.kind, it.id); err != nil {
			return err
		}
	}
	return p.sent(ctx, ok)
}

func (p *pass) sent(ctx context.Context, items []item) error {
	gs := make([]store.Guard, len(items))
	for i, it := range items {
		gs[i] = it.guard
	}
	p.rep.Sent += len(items)
	return p.st.MarkSent(ctx, gs)
}

func encode(batch []item) []byte {
	var b bytes.Buffer
	b.WriteString(`{"artifacts":[`)
	for i, it := range batch {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(it.artifact)
	}
	b.WriteString(`]}`)
	return b.Bytes()
}

// envelope is the size of `{"artifacts":[]}`.
const envelope = len(`{"artifacts":[]}`)

// TargetOf applies the gates: a cloud binding with an organisation and a
// remote id or slug, not stopped by a 403.
func TargetOf(b *store.Binding) (Target, bool) {
	if b == nil || b.Mode != store.ModeCloud || b.OrgID == nil || *b.OrgID == "" {
		return Target{}, false
	}
	ws := ""
	switch {
	case b.RemoteWorkspaceID != nil && *b.RemoteWorkspaceID != "":
		ws = *b.RemoteWorkspaceID
	case b.Slug != nil && *b.Slug != "":
		ws = *b.Slug
	default:
		return Target{}, false
	}
	return Target{OrgID: *b.OrgID, WorkspaceID: ws}, true
}

// Drain runs one pass: retire exhausted rows, then upload blobs and post
// batches in drain order until nothing is left or the budget is spent.
func Drain(ctx context.Context, c *Client, st *store.Store, budget time.Duration, now func() time.Time) (Report, error) {
	rep := Report{Outcome: Idle}
	b, err := st.Binding(ctx)
	if err != nil {
		return rep, err
	}
	t, ok := TargetOf(b)
	if !ok {
		return rep, nil
	}
	if b.DrainState == store.DrainNotAuthorized {
		rep.Outcome = NotAuthorized
		return rep, nil
	}
	rep.Outcome = Done
	if rep.Retired, err = st.RetireFailed(ctx); err != nil {
		return rep, err
	}
	deadline := now().Add(budget)
	p := &pass{c: c, st: st, rep: &rep, now: now}
	seen := map[string]bool{}
	uploaded := map[string]bool{}
	for now().Before(deadline) {
		o, err := st.Pending(ctx, page)
		if err != nil {
			return rep, err
		}
		var fresh []item
		for _, it := range t.items(o) {
			if !seen[it.id] {
				seen[it.id] = true
				fresh = append(fresh, it)
			}
		}
		if len(fresh) == 0 {
			break
		}
		if err := p.run(ctx, fresh, uploaded, deadline); err != nil {
			var s *stop
			if errors.As(err, &s) {
				rep.Outcome, rep.RetryAfter = s.outcome, s.after
				return rep, nil
			}
			return rep, err
		}
	}
	return rep, nil
}

// run uploads each row's blobs, then posts it in a batch, flushing at 100
// artifacts or 4 MiB.
func (p *pass) run(ctx context.Context, items []item, uploaded map[string]bool, deadline time.Time) error {
	var batch []item
	size := envelope
	for _, it := range items {
		if !p.now().Before(deadline) {
			break
		}
		ok, err := p.blobs(ctx, it, uploaded)
		if err != nil {
			return err
		}
		if !ok {
			p.rep.Failed++
			if err := p.st.MarkFailed(ctx, it.kind, it.id); err != nil {
				return err
			}
			continue
		}
		add := len(it.artifact) + 1
		if len(batch) == MaxArtifacts || (len(batch) > 0 && size+add > MaxBatch) {
			if err := p.post(ctx, batch); err != nil {
				return err
			}
			batch, size = nil, envelope
		}
		batch = append(batch, it)
		size += add
	}
	return p.post(ctx, batch)
}

// blobs puts every blob a row references; false fails the row (missing
// locally, or 413).
func (p *pass) blobs(ctx context.Context, it item, uploaded map[string]bool) (bool, error) {
	for _, key := range it.blobs {
		if uploaded[key] {
			continue
		}
		data, err := p.st.ReadBlob(key)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		ok, err := p.putBlob(ctx, key, data)
		if err != nil || !ok {
			return ok, err
		}
		uploaded[key] = true
	}
	return true, nil
}
