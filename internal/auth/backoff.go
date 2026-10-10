package auth

import (
	"math/rand/v2"
	"time"
)

// Backoff is the retry policy for 429, 5xx and transport failures
// (evidence-auth.md §8): exponential from 1 s to a 300 s ceiling, the delay
// drawn from [cap/2, cap], Retry-After honoured but clamped to [1 s, 300 s],
// unlimited attempts. At the ceiling an idle client stays far under the auth
// host's 100 requests per minute.
type Backoff struct {
	attempt int
	rand    func() float64
}

const (
	backoffBase = time.Second
	backoffCap  = 300 * time.Second
)

// Next returns the delay before the next attempt and advances the counter.
// retryAfter is the server's Retry-After (0 when absent).
func (b *Backoff) Next(retryAfter time.Duration) time.Duration {
	ceiling := backoffBase << min(b.attempt, 16)
	if ceiling > backoffCap || ceiling <= 0 {
		ceiling = backoffCap
	}
	b.attempt++
	if retryAfter > 0 {
		return min(max(retryAfter, time.Second), backoffCap)
	}
	r := rand.Float64
	if b.rand != nil {
		r = b.rand
	}
	half := ceiling / 2
	return half + time.Duration(r()*float64(ceiling-half))
}

// Reset starts the sequence again after a success.
func (b *Backoff) Reset() { b.attempt = 0 }
