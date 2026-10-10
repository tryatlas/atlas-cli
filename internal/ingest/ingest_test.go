package ingest

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type fakeTokens struct{ n, invalidated int }

func (f *fakeTokens) Token(context.Context) (string, error) {
	f.n++
	return "jwt-" + string(rune('0'+f.n)), nil
}
func (f *fakeTokens) Invalidate() { f.invalidated++ }

func server(t *testing.T, h http.HandlerFunc) *Client {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL, &fakeTokens{})
}

func TestCreateOmitsAbsentFieldsAndAcceptsIDOrWorkspaceID(t *testing.T) {
	var body string
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = io.WriteString(w, `{"id":"ws_9"}`)
	})
	id, err := c.Create(context.Background(), CreateRequest{OrgID: "org_1", Slug: "backend", Visibility: "org"})
	if err != nil || id != "ws_9" {
		t.Fatalf("%q %v", id, err)
	}
	if body != `{"orgId":"org_1","slug":"backend","visibility":"org"}` {
		t.Fatalf("body = %s", body)
	}
}

func TestConnectBody(t *testing.T) {
	var body string
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		_, _ = io.WriteString(w, `{"status":"connected","workspaceId":"ws_1","reason":"fingerprint"}`)
	})
	sha, u := "abc", "github.com/acme/backend"
	res, err := c.Connect(context.Background(), ConnectRequest{OrgID: "org_1", RootCommitSHA: &sha, GitURL: &u})
	if err != nil || res.Status != "connected" || res.WorkspaceID != "ws_1" || res.Reason != "fingerprint" {
		t.Fatalf("%+v %v", res, err)
	}
	if body != `{"orgId":"org_1","create":false,"rootCommitSha":"abc","gitUrl":"github.com/acme/backend"}` {
		t.Fatalf("body = %s", body)
	}
}

func TestRefreshesOnceOn401(t *testing.T) {
	calls := 0
	var auths []string
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		auths = append(auths, r.Header.Get("Authorization"))
		if calls == 1 {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, `{"workspaces":[{"id":"ws_1","slug":"backend"}]}`)
	})
	ws, err := c.Workspaces(context.Background(), "org 1")
	if err != nil || len(ws) != 1 || ws[0].Label() != "backend" {
		t.Fatalf("%+v %v", ws, err)
	}
	if calls != 2 || c.Tokens.(*fakeTokens).invalidated != 1 || auths[0] == auths[1] {
		t.Fatalf("calls=%d auths=%v", calls, auths)
	}
}

func TestErrorsAndConflicts(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/workspaces":
			w.WriteHeader(409)
		case "/workspaces/slug-available":
			if r.URL.Query().Get("orgId") != "org_1" || r.URL.Query().Get("slug") != "x" {
				t.Errorf("query %s", r.URL.RawQuery)
			}
			w.WriteHeader(409)
		default:
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `{"error":{"code":"forbidden","message":"not a member"}}`)
		}
	})
	if _, err := c.Create(context.Background(), CreateRequest{OrgID: "o", Slug: "s", Visibility: "org"}); !errors.Is(err, ErrSlugTaken) {
		t.Fatalf("create: %v", err)
	}
	if ok, err := c.SlugAvailable(context.Background(), "org_1", "x"); ok || err != nil {
		t.Fatalf("slug: %v %v", ok, err)
	}
	var e *Error
	if _, err := c.Connect(context.Background(), ConnectRequest{OrgID: "o"}); !errors.As(err, &e) || e.Code != "forbidden" || e.Status != 403 {
		t.Fatalf("connect: %v", err)
	}
}
