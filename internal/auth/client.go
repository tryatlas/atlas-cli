package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ClientID is the OAuth client the auth server knows. The CLI signs in exactly
// as the desktop does.
const ClientID = "atlas-desktop"

const (
	deviceGrantType = "urn:ietf:params:oauth:grant-type:device_code"
	authedTimeout   = 10 * time.Second
	maxBody         = 1 << 20
)

// FailureKind classifies a failed authenticated call (evidence-auth.md §2).
type FailureKind int

const (
	// Indeterminate is a transport error, 429 or 5xx. Nothing was learned; retry on the backoff.
	Indeterminate FailureKind = iota
	// Rejected is a 401. Authoritative; only a 401 from GET /token may clear the credential.
	Rejected
	// Denied is any other non-2xx. The credential is fine, the action was refused; never retry.
	Denied
)

// Failure is a classified error. Reason never carries a URL, a header or a body.
type Failure struct {
	Kind       FailureKind
	Status     int
	RetryAfter time.Duration // 0 when absent
	Reason     string
	Code       string // the server's error code, when its body carried one
}

func (f *Failure) Error() string {
	switch f.Kind {
	case Rejected:
		return "the Atlas session was rejected (401)"
	case Denied:
		if f.Code != "" {
			return fmt.Sprintf("refused by Atlas (%d %s)", f.Status, f.Code)
		}
		return fmt.Sprintf("refused by Atlas (%d)", f.Status)
	default:
		return "could not reach Atlas: " + f.Reason
	}
}

// IsRejected reports a 401.
func IsRejected(err error) bool {
	var f *Failure
	return errors.As(err, &f) && f.Kind == Rejected
}

// Client talks to the auth host.
type Client struct {
	Base string // e.g. https://auth.tryatlas.cc/api/auth, no trailing slash
	HTTP *http.Client
}

// NewClient returns a client for base with a default HTTP client. The device
// endpoints have no timeout; authenticated calls carry a 10 s one.
func NewClient(base string) *Client {
	return &Client{Base: strings.TrimRight(strings.TrimSpace(base), "/"), HTTP: &http.Client{}}
}

func (c *Client) url(path string) string { return c.Base + path }

// request sends one call and returns the status and body. err is a transport
// failure only.
func (c *Client) request(ctx context.Context, method, path, bearer string, body any) (int, http.Header, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), rd)
	if err != nil {
		return 0, nil, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, b, nil
}

// authed sends an authenticated call with the 10 s timeout and classifies the
// outcome. out, when non-nil, receives the decoded 2xx body.
func (c *Client) authed(ctx context.Context, method, path, token string, body, out any) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, authedTimeout)
	defer cancel()
	status, hdr, b, err := c.request(ctx, method, path, token, body)
	if err != nil {
		return nil, &Failure{Kind: Indeterminate, Reason: transportReason(err)}
	}
	if f := classify(status, hdr, b); f != nil {
		return nil, f
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return nil, &Failure{Kind: Indeterminate, Status: status, Reason: "unreadable response"}
		}
	}
	return b, nil
}

// classify maps a status to a Failure; nil means 2xx.
func classify(status int, hdr http.Header, body []byte) *Failure {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status == http.StatusUnauthorized:
		return &Failure{Kind: Rejected, Status: status}
	case status == http.StatusTooManyRequests || status >= 500:
		return &Failure{Kind: Indeterminate, Status: status, RetryAfter: retryAfter(hdr),
			Reason: fmt.Sprintf("server said %d", status)}
	default:
		return &Failure{Kind: Denied, Status: status, Code: errorCode(body)}
	}
}

// retryAfter reads Retry-After as delta-seconds only.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	n, err := strconv.ParseUint(v, 10, 32)
	if err != nil {
		return 0
	}
	return time.Duration(n) * time.Second
}

// errorCode reads {"code":…} or {"error":{"code":…}} or {"error":"…"} from a body.
func errorCode(body []byte) string {
	var v struct {
		Code  string          `json:"code"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	if v.Code != "" {
		return v.Code
	}
	var s string
	if json.Unmarshal(v.Error, &s) == nil {
		return s
	}
	var o struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(v.Error, &o) == nil {
		return o.Code
	}
	return ""
}

// transportReason describes a transport error without the URL it carries.
func transportReason(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	return err.Error()
}

// ── Device grant ─────────────────────────────────────────────────────────────

// DeviceCode is the answer to POST /device/code. DeviceCode itself is a
// secret held in memory only.
type DeviceCode struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"` // parsed, never opened or printed
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

// StartGrant posts /device/code.
func (c *Client) StartGrant(ctx context.Context) (*DeviceCode, error) {
	status, _, b, err := c.request(ctx, http.MethodPost, "/device/code", "", map[string]string{"client_id": ClientID})
	if err != nil {
		return nil, fmt.Errorf("could not reach Atlas: %s", transportReason(err))
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("could not reach Atlas: server said %d", status)
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return nil, errors.New("could not reach Atlas: unreadable response")
	}
	for _, k := range []string{"device_code", "user_code", "verification_uri", "verification_uri_complete", "expires_in", "interval"} {
		if _, ok := raw[k]; !ok {
			return nil, fmt.Errorf("could not reach Atlas: response is missing %s", k)
		}
	}
	var dc DeviceCode
	if err := json.Unmarshal(b, &dc); err != nil {
		return nil, errors.New("could not reach Atlas: unreadable response")
	}
	return &dc, nil
}

// PollOutcome is one answer from POST /device/token.
type PollOutcome int

// Poll outcomes (evidence-auth.md §2.2).
const (
	PollTransient PollOutcome = iota
	PollPending
	PollSlowDown
	PollDenied
	PollExpired
	PollToken
)

// PollToken posts /device/token once. On PollToken the session token is returned.
func (c *Client) PollToken(ctx context.Context, deviceCode string) (PollOutcome, string) {
	status, _, b, err := c.request(ctx, http.MethodPost, "/device/token", "", map[string]string{
		"grant_type": deviceGrantType, "device_code": deviceCode, "client_id": ClientID,
	})
	switch {
	case err != nil, status >= 500:
		return PollTransient, ""
	case status >= 200 && status < 300:
		var ok struct {
			AccessToken string `json:"access_token"`
		}
		if json.Unmarshal(b, &ok) != nil || ok.AccessToken == "" {
			return PollTransient, ""
		}
		return PollToken, ok.AccessToken
	}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) != nil {
		return PollTransient, ""
	}
	switch e.Error {
	case "authorization_pending":
		return PollPending, ""
	case "slow_down":
		return PollSlowDown, ""
	case "access_denied":
		return PollDenied, ""
	case "expired_token":
		return PollExpired, ""
	}
	return PollTransient, ""
}

// ── Authenticated calls ──────────────────────────────────────────────────────

// MintJWT gets a fresh access JWT with the session token (GET /token).
func (c *Client) MintJWT(ctx context.Context, sessionToken string) (string, error) {
	var out struct {
		Token string `json:"token"`
	}
	if _, err := c.authed(ctx, http.MethodGet, "/token", sessionToken, nil, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", &Failure{Kind: Indeterminate, Reason: "unreadable token"}
	}
	return out.Token, nil
}

// Profile is GET /get-session's user and session.
type Profile struct {
	UserID               string
	Name                 string
	Email                string
	Image                *string
	ActiveOrganizationID *string
}

// GetSession fetches the profile. A dead session answers 200 null: that is
// (nil, nil), never a sign-out verdict.
func (c *Client) GetSession(ctx context.Context, sessionToken string) (*Profile, error) {
	b, err := c.authed(ctx, http.MethodGet, "/get-session", sessionToken, nil, nil)
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 || string(bytes.TrimSpace(b)) == "null" {
		return nil, nil
	}
	var out struct {
		Session struct {
			ActiveOrganizationID *string `json:"activeOrganizationId"`
		} `json:"session"`
		User *struct {
			ID    string  `json:"id"`
			Name  string  `json:"name"`
			Email string  `json:"email"`
			Image *string `json:"image"`
		} `json:"user"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, &Failure{Kind: Indeterminate, Reason: "unreadable response"}
	}
	if out.User == nil {
		return nil, nil
	}
	return &Profile{UserID: out.User.ID, Name: out.User.Name, Email: out.User.Email,
		Image: out.User.Image, ActiveOrganizationID: out.Session.ActiveOrganizationID}, nil
}

// OrgListing is one row of GET /organization/list (no roles).
type OrgListing struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ListOrgs fetches GET /organization/list. [] is a real answer.
func (c *Client) ListOrgs(ctx context.Context, sessionToken string) ([]OrgListing, error) {
	var out []OrgListing
	if _, err := c.authed(ctx, http.MethodGet, "/organization/list", sessionToken, nil, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = []OrgListing{}
	}
	return out, nil
}

// CheckSlug asks whether an organisation slug is free. Advisory: create can still race.
func (c *Client) CheckSlug(ctx context.Context, sessionToken, slug string) (bool, error) {
	var out struct {
		Available bool `json:"available"`
	}
	_, err := c.authed(ctx, http.MethodPost, "/organization/check-slug", sessionToken, map[string]string{"slug": slug}, &out)
	return out.Available, err
}

// CreateOrg creates an organisation; the creator becomes admin. A taken slug is 400 (Denied).
func (c *Client) CreateOrg(ctx context.Context, sessionToken, name, slug string) (OrgListing, error) {
	var out OrgListing
	_, err := c.authed(ctx, http.MethodPost, "/organization/create", sessionToken, map[string]string{"name": name, "slug": slug}, &out)
	return out, err
}

// SignOut revokes the session once. 2xx or 401 means revoked.
func (c *Client) SignOut(ctx context.Context, sessionToken string) bool {
	ctx, cancel := context.WithTimeout(ctx, authedTimeout)
	defer cancel()
	status, _, _, err := c.request(ctx, http.MethodPost, "/sign-out", sessionToken, struct{}{})
	if err != nil {
		return false
	}
	return (status >= 200 && status < 300) || status == http.StatusUnauthorized
}
