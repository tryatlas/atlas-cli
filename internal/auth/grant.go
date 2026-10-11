package auth

import (
	"context"
	"errors"
	"time"
)

const (
	minPollInterval = 200 * time.Millisecond
	slowDownStep    = 5 * time.Second
)

// Grant errors, with the desktop's user-facing wording.
var (
	ErrGrantDenied    = errors.New("connection denied in the browser")
	ErrGrantExpired   = errors.New("that code expired; run atlas login again to get a new one")
	ErrGrantCancelled = errors.New("sign-in cancelled")
)

// Prompt is what the user needs to approve the grant: the code and the plain
// verification URL. verification_uri_complete is never shown: a link that
// carries the code is the remote-phishing attack.
type Prompt struct {
	UserCode        string
	VerificationURI string
	ExpiresAt       time.Time
}

// clock lets tests drive the poll loop.
type clock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

var realClock = clock{
	now: time.Now,
	sleep: func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	},
}

// runGrant runs the device grant to a session token (evidence-auth.md §3).
// Cancelling ctx stops polling; nothing is sent to the server.
func runGrant(ctx context.Context, c *Client, clk clock, show func(Prompt)) (string, error) {
	dc, err := c.StartGrant(ctx)
	if err != nil {
		return "", err
	}
	expiresIn := max(dc.ExpiresIn, 1)
	expiresAt := clk.now().Add(time.Duration(expiresIn) * time.Second)
	interval := max(time.Duration(dc.Interval)*time.Second, minPollInterval)
	show(Prompt{UserCode: dc.UserCode, VerificationURI: dc.VerificationURI, ExpiresAt: expiresAt})

	for {
		if ctx.Err() != nil {
			return "", ErrGrantCancelled
		}
		if !clk.now().Before(expiresAt) {
			return "", ErrGrantExpired
		}
		if err := clk.sleep(ctx, interval); err != nil {
			return "", ErrGrantCancelled
		}
		if ctx.Err() != nil {
			return "", ErrGrantCancelled
		}
		switch outcome, token := c.PollToken(ctx, dc.DeviceCode); outcome {
		case PollToken:
			return token, nil
		case PollSlowDown:
			interval += slowDownStep // cumulative, never reset
		case PollDenied:
			return "", ErrGrantDenied
		case PollExpired:
			return "", ErrGrantExpired
		case PollPending, PollTransient:
			// Same interval. A 5xx or a dropped connection does not end the grant.
		}
	}
}
