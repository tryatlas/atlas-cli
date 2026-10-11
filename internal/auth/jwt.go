package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Claims are the parts of the access JWT the client reads. The signature is
// not verified client-side.
type Claims struct {
	Sub  string            `json:"sub"`
	Orgs map[string]string `json:"orgs"` // {orgId: role}; {} when in none
	Exp  int64             `json:"exp"`
}

// ParseClaims decodes the payload segment (base64url, no padding). ok is false
// when the token cannot be read.
func ParseClaims(jwt string) (Claims, bool) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return Claims{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return Claims{}, false
	}
	var c Claims
	if json.Unmarshal(raw, &c) != nil {
		return Claims{}, false
	}
	if c.Orgs == nil {
		c.Orgs = map[string]string{}
	}
	return c, true
}

const (
	remintBefore = 60 * time.Second  // re-mint at exp − 60 s
	fallbackTTL  = 600 * time.Second // when exp cannot be read
)

// TokenSource mints access JWTs for hosts other than auth and caches one in
// memory until exp − 60 s. The JWT is never written to disk.
type TokenSource struct {
	mint func(ctx context.Context) (string, error)
	now  func() time.Time

	mu      sync.Mutex
	jwt     string
	expires time.Time
}

// NewTokenSource wraps a mint function.
func NewTokenSource(mint func(ctx context.Context) (string, error)) *TokenSource {
	return &TokenSource{mint: mint, now: time.Now}
}

// Token returns a cached JWT or mints a new one.
func (ts *TokenSource) Token(ctx context.Context) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.jwt != "" && ts.now().Before(ts.expires) {
		return ts.jwt, nil
	}
	jwt, err := ts.mint(ctx)
	if err != nil {
		return "", err
	}
	ts.jwt = jwt
	ts.expires = ts.now().Add(fallbackTTL)
	if c, ok := ParseClaims(jwt); ok && c.Exp > 0 {
		ts.expires = time.Unix(c.Exp, 0).Add(-remintBefore)
	}
	return jwt, nil
}

// Invalidate drops the cached JWT, for a 401 from another host: the next
// Token call mints again (once per failure).
func (ts *TokenSource) Invalidate() {
	ts.mu.Lock()
	ts.jwt = ""
	ts.mu.Unlock()
}
