package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAuth is a scripted auth host. Each handler may be replaced per test.
type fakeAuth struct {
	t        *testing.T
	mu       sync.Mutex
	polls    []func(w http.ResponseWriter) // consumed in order by /device/token
	calls    []string
	bodies   map[string]string
	bearers  map[string]string
	handlers map[string]http.HandlerFunc
}

func newFakeAuth(t *testing.T) (*fakeAuth, *httptest.Server) {
	f := &fakeAuth{t: t, bodies: map[string]string{}, bearers: map[string]string{}, handlers: map[string]http.HandlerFunc{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + strings.TrimPrefix(r.URL.Path, "/api/auth")
		f.mu.Lock()
		f.calls = append(f.calls, key)
		f.bodies[key] = string(b)
		f.bearers[key] = r.Header.Get("Authorization")
		h := f.handlers[key]
		var poll func(http.ResponseWriter)
		if key == "POST /device/token" && len(f.polls) > 0 {
			poll, f.polls = f.polls[0], f.polls[1:]
		}
		f.mu.Unlock()
		switch {
		case poll != nil:
			poll(w)
		case h != nil:
			h(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeAuth) on(key string, status int, body string) {
	f.handlers[key] = func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func jsonResp(status int, body string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

func (f *fakeAuth) count(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == key {
			n++
		}
	}
	return n
}

// fakeClock advances only when the loop sleeps.
type fakeClock struct {
	mu     sync.Mutex
	t      time.Time
	slept  []time.Duration
	onWait func()
}

func (c *fakeClock) clock() clock {
	return clock{
		now: func() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t },
		sleep: func(ctx context.Context, d time.Duration) error {
			c.mu.Lock()
			c.slept = append(c.slept, d)
			c.t = c.t.Add(d)
			fn := c.onWait
			c.mu.Unlock()
			if fn != nil {
				fn()
			}
			return ctx.Err()
		},
	}
}

func jwtWith(claims string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"EdDSA"}`)) + "." + enc([]byte(claims)) + ".sig"
}

const deviceCodeBody = `{"device_code":"dev-secret","user_code":"ABCD-1234",
 "verification_uri":"https://app.example/device",
 "verification_uri_complete":"https://app.example/device?user_code=ABCD-1234",
 "expires_in":600,"interval":0}`

func newTestAuth(t *testing.T) (*Auth, *fakeAuth, *fakeClock) {
	f, srv := newFakeAuth(t)
	clk := &fakeClock{t: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)}
	a := &Auth{Store: Store{Dir: t.TempDir()}, Client: NewClient(srv.URL + "/api/auth/"), clk: clk.clock()}
	return a, f, clk
}

func TestStartGrantSendsClientIDAndRequiresEveryField(t *testing.T) {
	a, f, _ := newTestAuth(t)
	f.on("POST /device/code", 200, deviceCodeBody)
	dc, err := a.Client.StartGrant(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.bodies["POST /device/code"]; got != `{"client_id":"atlas-desktop"}` {
		t.Fatalf("body = %s", got)
	}
	if f.bearers["POST /device/code"] != "" {
		t.Fatal("device endpoints are unauthenticated")
	}
	if dc.UserCode != "ABCD-1234" || dc.VerificationURI != "https://app.example/device" {
		t.Fatalf("%+v", dc)
	}
	f.on("POST /device/code", 200, `{"device_code":"d","user_code":"u","verification_uri":"v","expires_in":1,"interval":1}`)
	if _, err := a.Client.StartGrant(context.Background()); err == nil {
		t.Fatal("a missing verification_uri_complete must fail like the desktop's strict struct")
	}
	f.on("POST /device/code", 503, ``)
	if _, err := a.Client.StartGrant(context.Background()); err == nil || !strings.Contains(err.Error(), "server said 503") {
		t.Fatalf("err = %v", err)
	}
}

func TestPollTokenClassification(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   PollOutcome
	}{
		{200, `{"access_token":"session-tok","token_type":"Bearer"}`, PollToken},
		{200, `{"access_token":"","token_type":"Bearer"}`, PollTransient},
		{200, `nope`, PollTransient},
		{400, `{"error":"authorization_pending"}`, PollPending},
		{400, `{"error":"slow_down"}`, PollSlowDown},
		{400, `{"error":"access_denied"}`, PollDenied},
		{400, `{"error":"expired_token"}`, PollExpired},
		{400, `{"error":"something_else"}`, PollTransient},
		{400, `garbage`, PollTransient},
		{502, `{"error":"access_denied"}`, PollTransient},
	}
	for _, c := range cases {
		a, f, _ := newTestAuth(t)
		f.polls = []func(http.ResponseWriter){jsonResp(c.status, c.body)}
		got, tok := a.Client.PollToken(context.Background(), "dev-secret")
		if got != c.want {
			t.Errorf("%d %s: got %v, want %v", c.status, c.body, got, c.want)
		}
		if c.want == PollToken && tok != "session-tok" {
			t.Errorf("token = %q", tok)
		}
		var body map[string]string
		_ = json.Unmarshal([]byte(f.bodies["POST /device/token"]), &body)
		if body["grant_type"] != "urn:ietf:params:oauth:grant-type:device_code" || body["device_code"] != "dev-secret" || body["client_id"] != "atlas-desktop" || len(body) != 3 {
			t.Errorf("poll body = %s", f.bodies["POST /device/token"])
		}
	}
}

func TestGrantLoopIntervalsAndToken(t *testing.T) {
	a, f, clk := newTestAuth(t)
	f.on("POST /device/code", 200, deviceCodeBody)
	f.polls = []func(http.ResponseWriter){
		jsonResp(400, `{"error":"authorization_pending"}`),
		jsonResp(500, ``),
		jsonResp(400, `{"error":"slow_down"}`),
		jsonResp(400, `{"error":"slow_down"}`),
		jsonResp(200, `{"access_token":"session-tok","token_type":"Bearer"}`),
	}
	var shown Prompt
	tok, err := runGrant(context.Background(), a.Client, clk.clock(), func(p Prompt) { shown = p })
	if err != nil || tok != "session-tok" {
		t.Fatalf("tok=%q err=%v", tok, err)
	}
	if shown.UserCode != "ABCD-1234" || shown.VerificationURI != "https://app.example/device" {
		t.Fatalf("prompt = %+v", shown)
	}
	// interval 0 → 200 ms floor; slow_down adds 5 s cumulatively.
	want := []time.Duration{200 * time.Millisecond, 200 * time.Millisecond, 200 * time.Millisecond,
		5200 * time.Millisecond, 10200 * time.Millisecond}
	if len(clk.slept) != len(want) {
		t.Fatalf("slept %v", clk.slept)
	}
	for i := range want {
		if clk.slept[i] != want[i] {
			t.Fatalf("slept %v, want %v", clk.slept, want)
		}
	}
}

func TestGrantTerminalOutcomes(t *testing.T) {
	for _, c := range []struct {
		body string
		want error
	}{
		{`{"error":"access_denied"}`, ErrGrantDenied},
		{`{"error":"expired_token"}`, ErrGrantExpired},
	} {
		a, f, clk := newTestAuth(t)
		f.on("POST /device/code", 200, deviceCodeBody)
		f.polls = []func(http.ResponseWriter){jsonResp(400, c.body)}
		if _, err := runGrant(context.Background(), a.Client, clk.clock(), func(Prompt) {}); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v", c.body, err)
		}
	}
}

func TestGrantLocalDeadline(t *testing.T) {
	a, f, clk := newTestAuth(t)
	f.on("POST /device/code", 200, `{"device_code":"d","user_code":"u","verification_uri":"v","verification_uri_complete":"v?c","expires_in":1,"interval":1}`)
	for i := 0; i < 5; i++ {
		f.polls = append(f.polls, jsonResp(400, `{"error":"authorization_pending"}`))
	}
	if _, err := runGrant(context.Background(), a.Client, clk.clock(), func(Prompt) {}); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("err = %v", err)
	}
	if n := f.count("POST /device/token"); n != 1 {
		t.Fatalf("polled %d times after a 1 s deadline", n)
	}
}

func TestGrantCancelSendsNothing(t *testing.T) {
	a, f, clk := newTestAuth(t)
	f.on("POST /device/code", 200, deviceCodeBody)
	ctx, cancel := context.WithCancel(context.Background())
	clk.onWait = cancel
	if _, err := runGrant(ctx, a.Client, clk.clock(), func(Prompt) {}); !errors.Is(err, ErrGrantCancelled) {
		t.Fatalf("err = %v", err)
	}
	if n := f.count("POST /device/token"); n != 0 {
		t.Fatalf("cancel sent %d polls", n)
	}
}

func serveIdentity(f *fakeAuth) {
	f.on("GET /get-session", 200, `{"session":{"id":"sess_1","activeOrganizationId":null},
	  "user":{"id":"user_1","name":"Ada Lovelace","email":"ada@atlas.example","image":null}}`)
	f.on("GET /organization/list", 200, `[{"id":"org_1","name":"Atlas","slug":"org_1","logo":null},{"id":"org_2","name":"Side","slug":"side","logo":null}]`)
	f.on("GET /token", 200, `{"token":"`+jwtWith(`{"sub":"user_1","orgs":{"org_1":"admin","org_2":"chief"}}`)+`"}`)
}

func TestLoginWritesTheDesktopFile(t *testing.T) {
	a, f, _ := newTestAuth(t)
	f.on("POST /device/code", 200, deviceCodeBody)
	f.polls = []func(http.ResponseWriter){jsonResp(200, `{"access_token":"session-tok","token_type":"Bearer"}`)}
	serveIdentity(f)
	if _, err := a.Login(context.Background(), func(Prompt) {}); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"GET /get-session", "GET /organization/list", "GET /token"} {
		if f.bearers[k] != "Bearer session-tok" {
			t.Errorf("%s bearer = %q", k, f.bearers[k])
		}
	}
	got, _ := os.ReadFile(a.Store.Path())
	want := `{
  "sessionToken": "session-tok",
  "savedAt": "2026-10-10T12:00:00.2+00:00",
  "identity": {
    "id": "user_1",
    "name": "Ada Lovelace",
    "email": "ada@atlas.example",
    "avatarUrl": null,
    "avatarPath": null,
    "orgs": [
      {
        "id": "org_1",
        "name": "Atlas",
        "role": "admin"
      },
      {
        "id": "org_2",
        "name": "Side",
        "role": null
      }
    ],
    "activeOrgId": null
  },
  "pinnedOrg": null
}`
	want = strings.Replace(want, "12:00:00.2+00:00", "12:00:00.200+00:00", 1)
	if string(got) != want {
		t.Fatalf("file:\n%s\nwant:\n%s", got, want)
	}
	s, _ := a.Session()
	if s.ActiveOrgID() != "org_1" {
		t.Fatalf("active org = %q", s.ActiveOrgID())
	}
}

func TestRefreshKeepsWhatFailedToLoad(t *testing.T) {
	a, f, _ := newTestAuth(t)
	serveIdentity(f)
	if err := a.Store.Save(freshSession("session-tok", "s")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Dead profile (200 null), a 401 on the list, an unreadable JWT: keep everything.
	f.on("GET /get-session", 200, `null`)
	f.on("GET /organization/list", 401, ``)
	f.on("GET /token", 200, `{"token":"not-a-jwt"}`)
	s, _ := a.Refresh(context.Background())
	if s == nil || s.Identity == nil || s.Identity.Name != "Ada Lovelace" {
		t.Fatalf("identity lost: %+v", s)
	}
	orgs := *s.Identity.Orgs
	if len(orgs) != 2 || orgs[0].Role == nil || *orgs[0].Role != RoleAdmin {
		t.Fatalf("orgs/roles lost: %+v", orgs)
	}
	if _, ok := a.Store.Load(); !ok {
		t.Fatal("a 401 from /organization/list must not clear the session")
	}
}

func TestOnlyA401FromTokenClearsTheSession(t *testing.T) {
	a, f, _ := newTestAuth(t)
	serveIdentity(f)
	_ = a.Store.Save(freshSession("session-tok", "s"))
	f.on("GET /token", 403, `{"code":"nope"}`)
	if _, err := a.Refresh(context.Background()); err == nil {
		t.Fatal("want the 403 reported")
	}
	if _, ok := a.Store.Load(); !ok {
		t.Fatal("a 403 must keep the credential")
	}
	f.on("GET /token", 401, ``)
	if _, err := a.Refresh(context.Background()); !errors.Is(err, ErrSignedOut) {
		t.Fatalf("err = %v", err)
	}
	if _, ok := a.Store.Load(); ok {
		t.Fatal("a 401 from /token must clear the session")
	}
}

func TestRefreshDoesNotResurrectASignedOutFile(t *testing.T) {
	a, f, _ := newTestAuth(t)
	_ = a.Store.Save(freshSession("session-tok", "s"))
	serveIdentity(f)
	f.handlers["GET /token"] = func(w http.ResponseWriter, _ *http.Request) {
		_ = a.Store.Clear() // the desktop signs out while we are on the network
		_, _ = io.WriteString(w, `{"token":"`+jwtWith(`{"orgs":{}}`)+`"}`)
	}
	_, _ = a.Refresh(context.Background())
	if _, ok := a.Store.Load(); ok {
		t.Fatal("refresh wrote back a file deleted during its network calls")
	}
}

func TestPinSurvivesRefreshAndSeedsActiveOrg(t *testing.T) {
	a, f, _ := newTestAuth(t)
	serveIdentity(f)
	_ = a.Store.Save(freshSession("session-tok", "s"))
	if _, err := a.SetActiveOrg("org_2"); err != nil {
		t.Fatal(err)
	}
	s, _ := a.Refresh(context.Background())
	if s.PinnedOrg == nil || s.PinnedOrg.OrgID == nil || *s.PinnedOrg.OrgID != "org_2" || s.ActiveOrgID() != "org_2" {
		t.Fatalf("pin lost: %+v / %q", s.PinnedOrg, s.ActiveOrgID())
	}
	s, _ = a.SetActiveOrg("")
	if s.PinnedOrg == nil || s.PinnedOrg.OrgID != nil {
		t.Fatalf("explicit none not stored: %+v", s.PinnedOrg)
	}
}

func TestLogoutDeletesFirstThenRevokesOnce(t *testing.T) {
	for _, c := range []struct {
		status int
		want   bool
	}{{200, true}, {401, true}, {500, false}} {
		a, f, _ := newTestAuth(t)
		_ = a.Store.Save(freshSession("session-tok", "s"))
		f.handlers["POST /sign-out"] = func(w http.ResponseWriter, _ *http.Request) {
			if _, ok := a.Store.Load(); ok {
				t.Error("file still present when /sign-out was sent")
			}
			w.WriteHeader(c.status)
		}
		got, err := a.Logout(context.Background())
		if err != nil || got != c.want {
			t.Errorf("%d: got %v, %v", c.status, got, err)
		}
		if f.bodies["POST /sign-out"] != `{}` || f.bearers["POST /sign-out"] != "Bearer session-tok" {
			t.Errorf("sign-out request: %q %q", f.bodies["POST /sign-out"], f.bearers["POST /sign-out"])
		}
		if n := f.count("POST /sign-out"); n != 1 {
			t.Errorf("sign-out sent %d times", n)
		}
	}
}

func TestClassify(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "7")
	cases := []struct {
		status int
		hdr    http.Header
		body   string
		kind   FailureKind
		after  time.Duration
		code   string
	}{
		{401, nil, ``, Rejected, 0, ""},
		{403, nil, `{"code":"owner_cannot_leave","message":"…"}`, Denied, 0, "owner_cannot_leave"},
		{400, nil, `{"error":{"code":"org_not_covered"}}`, Denied, 0, "org_not_covered"},
		{404, nil, ``, Denied, 0, ""},
		{429, h, ``, Indeterminate, 7 * time.Second, ""},
		{503, nil, ``, Indeterminate, 0, ""},
	}
	for _, c := range cases {
		f := classify(c.status, c.hdr, []byte(c.body))
		if f == nil || f.Kind != c.kind || f.RetryAfter != c.after || f.Code != c.code {
			t.Errorf("%d: %+v", c.status, f)
		}
	}
	if classify(204, nil, nil) != nil {
		t.Error("2xx is not a failure")
	}
	h.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	if f := classify(503, h, nil); f.RetryAfter != 0 {
		t.Error("Retry-After is delta-seconds only")
	}
}

func TestTokenSourceCachesUntilExpMinus60(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	mints := 0
	exp := now.Add(10 * time.Minute).Unix()
	ts := NewTokenSource(func(context.Context) (string, error) {
		mints++
		return jwtWith(`{"exp":` + jsonInt(exp) + `}`), nil
	})
	ts.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, err := ts.Token(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if mints != 1 {
		t.Fatalf("minted %d times", mints)
	}
	now = now.Add(9*time.Minute + 1*time.Second) // past exp − 60 s
	_, _ = ts.Token(context.Background())
	if mints != 2 {
		t.Fatalf("did not re-mint at exp − 60 s (%d)", mints)
	}
	ts.Invalidate()
	_, _ = ts.Token(context.Background())
	if mints != 3 {
		t.Fatalf("invalidate did not force a mint (%d)", mints)
	}
}

func TestTokenSourceFallbackTTL(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	mints := 0
	ts := NewTokenSource(func(context.Context) (string, error) { mints++; return "opaque", nil })
	ts.now = func() time.Time { return now }
	_, _ = ts.Token(context.Background())
	now = now.Add(599 * time.Second)
	_, _ = ts.Token(context.Background())
	now = now.Add(2 * time.Second)
	_, _ = ts.Token(context.Background())
	if mints != 2 {
		t.Fatalf("minted %d times, want 2 (600 s fallback)", mints)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestBackoff(t *testing.T) {
	b := Backoff{rand: func() float64 { return 1 }}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 64, 128, 256, 300, 300}
	for i, w := range want {
		if got := b.Next(0); got != w*time.Second {
			t.Fatalf("attempt %d: %v, want %v", i, got, w*time.Second)
		}
	}
	low := Backoff{rand: func() float64 { return 0 }}
	low.attempt = 3
	if got := low.Next(0); got != 4*time.Second {
		t.Fatalf("lower bound = %v, want cap/2", got)
	}
	ra := Backoff{}
	if got := ra.Next(900 * time.Second); got != 300*time.Second {
		t.Fatalf("Retry-After not clamped: %v", got)
	}
	if got := ra.Next(time.Millisecond); got != time.Second {
		t.Fatalf("Retry-After floor: %v", got)
	}
}

func TestParseClaims(t *testing.T) {
	c, ok := ParseClaims(jwtWith(`{"sub":"user_1","orgs":{"org_1":"admin"}}`))
	if !ok || c.Sub != "user_1" || c.Orgs["org_1"] != "admin" {
		t.Fatalf("%+v %v", c, ok)
	}
	if c, ok := ParseClaims(jwtWith(`{"sub":"u"}`)); !ok || c.Orgs == nil || len(c.Orgs) != 0 {
		t.Fatalf("missing orgs should read as {}: %+v", c)
	}
	if _, ok := ParseClaims("nope"); ok {
		t.Fatal("garbage parsed")
	}
}
