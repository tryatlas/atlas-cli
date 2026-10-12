package main

import (
	"bytes"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/tryatlas/cli/internal/auth"
)

// installFakeAuth points every command at a scripted auth host and a temp store.
func installFakeAuth(t *testing.T, routes map[string]string) auth.Store {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		body, ok := routes[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		status := 200
		if strings.HasPrefix(body, "!") { // "!401 body"
			code, rest, _ := strings.Cut(body[1:], " ")
			status = map[string]int{"401": 401, "400": 400, "500": 500}[code]
			body = rest
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	st := auth.Store{Dir: t.TempDir()}
	old := newAuth
	newAuth = func() (*auth.Auth, error) { return auth.NewWith(st, auth.NewClient(srv.URL)), nil }
	t.Cleanup(func() { newAuth = old })
	return st
}

func jwt(claims string) string {
	e := base64.RawURLEncoding.EncodeToString
	return e([]byte(`{}`)) + "." + e([]byte(claims)) + ".s"
}

var identityRoutes = map[string]string{
	"POST /device/code":             `{"device_code":"d","user_code":"ABCD-1234","verification_uri":"https://app.example/device","verification_uri_complete":"https://app.example/device?user_code=ABCD-1234","expires_in":600,"interval":0}`,
	"POST /device/token":            `{"access_token":"session-tok","token_type":"Bearer"}`,
	"GET /get-session":              `{"session":{"activeOrganizationId":null},"user":{"id":"user_1","name":"Ada Lovelace","email":"ada@atlas.example","image":null}}`,
	"GET /organization/list":        `[{"id":"org_1","name":"Atlas"}]`,
	"POST /sign-out":                `{}`,
	"POST /organization/check-slug": `{"available":true}`,
	"POST /organization/create":     `{"id":"org_9","name":"Acme Labs"}`,
}

func routes() map[string]string {
	out := map[string]string{"GET /token": `{"token":"` + jwt(`{"orgs":{"org_1":"admin"}}`) + `"}`}
	for k, v := range identityRoutes {
		out[k] = v
	}
	return out
}

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestLoginPrintsPlainURLAndCode(t *testing.T) {
	st := installFakeAuth(t, routes())
	code, out, errOut := runCmd(t, "login", "--no-browser")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !strings.Contains(out, "https://app.example/device") || !strings.Contains(out, "ABCD-1234") {
		t.Fatalf("output: %s", out)
	}
	if strings.Contains(out, "user_code=") {
		t.Fatal("printed verification_uri_complete")
	}
	if !strings.Contains(out, "Signed in as Ada Lovelace <ada@atlas.example>") || !strings.Contains(out, "* Atlas (org_1)  admin") {
		t.Fatalf("output: %s", out)
	}
	if _, ok := st.Load(); !ok {
		t.Fatal("no session file written")
	}
}

func TestWhoamiSignedOut(t *testing.T) {
	installFakeAuth(t, routes())
	code, _, errOut := runCmd(t, "whoami")
	if code != 1 || !strings.Contains(errOut, "not signed in") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
}

func TestOrgUseAndLogout(t *testing.T) {
	st := installFakeAuth(t, routes())
	if code, _, e := runCmd(t, "login", "--no-browser"); code != 0 {
		t.Fatal(e)
	}
	code, out, _ := runCmd(t, "org", "use", "org_1")
	if code != 0 || !strings.Contains(out, "Active organisation: Atlas (org_1)") {
		t.Fatalf("exit %d: %s", code, out)
	}
	s, _ := st.Load()
	if s.PinnedOrg == nil || *s.PinnedOrg.OrgID != "org_1" {
		t.Fatalf("pin = %+v", s.PinnedOrg)
	}
	code, out, _ = runCmd(t, "logout")
	if code != 0 || !strings.Contains(out, "Signed out.") {
		t.Fatalf("exit %d: %s", code, out)
	}
	if _, err := os.Stat(st.Path()); !os.IsNotExist(err) {
		t.Fatal("session file still present")
	}
}

func TestOrgCreate(t *testing.T) {
	installFakeAuth(t, routes())
	if code, _, e := runCmd(t, "login", "--no-browser"); code != 0 {
		t.Fatal(e)
	}
	code, out, e := runCmd(t, "org", "create", "Acme Labs")
	if code != 0 || !strings.Contains(out, "Created Acme Labs (org_9)") {
		t.Fatalf("exit %d: %s %s", code, out, e)
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{"Acme Labs": "acme-labs", "  Ünïcode & Co!! ": "n-code-co", "---": ""} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}
