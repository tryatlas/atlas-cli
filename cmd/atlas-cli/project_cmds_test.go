package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/project"
)

// fakeIngest records requests and answers from a route table.
type fakeIngest struct {
	mu     sync.Mutex
	bodies map[string]string
	routes map[string]string
}

func installFakeIngest(t *testing.T, routes map[string]string) *fakeIngest {
	t.Helper()
	f := &fakeIngest{bodies: map[string]string{}, routes: routes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		key := r.Method + " " + r.URL.Path
		f.mu.Lock()
		f.bodies[key] = string(b)
		body, ok := f.routes[key]
		f.mu.Unlock()
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("ATLAS_INGEST_URL", srv.URL)
	return f
}

// gitRepo makes a repository with one commit and an origin, and chdirs into it.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "backend")
	if err := os.MkdirAll(filepath.Join(dir, "svc"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@e.test", "-c", "commit.gpgsign=false"}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "a"), []byte("a"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "first")
	run("remote", "add", "origin", "https://github.com/Acme/Backend.git")
	t.Chdir(dir)
	c, _ := project.Canonical(dir)
	return c
}

func binding(t *testing.T, root string) *store.Binding {
	t.Helper()
	s, err := store.Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b, err := s.Binding(context.Background())
	if err != nil || b == nil {
		t.Fatalf("binding: %+v %v", b, err)
	}
	return b
}

func TestInitLocal(t *testing.T) {
	root := gitRepo(t)
	code, out, e := runCmd(t, "init", "--local")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	b := binding(t, root)
	if b.Mode != "local" || !b.ImportApproved || !b.Enabled || b.WorkspaceID != root || b.Root != root || b.DrainState != "ok" {
		t.Fatalf("%+v", b)
	}
	if b.RootCommitSHA == nil || *b.GitURL != "github.com/acme/backend" {
		t.Fatalf("fingerprint: %+v", b)
	}
	if !strings.Contains(out, "locally") {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(root, ".atlas", ".gitignore")); err != nil {
		t.Fatal(err)
	}
}

func TestInitCloudMatchedByFingerprint(t *testing.T) {
	root := gitRepo(t)
	installFakeAuth(t, routes())
	f := installFakeIngest(t, map[string]string{
		"POST /workspaces/connect": `{"status":"connected","workspaceId":"ws_1","slug":"backend","reason":"fingerprint"}`,
	})
	if code, _, e := runCmd(t, "login", "--no-browser"); code != 0 {
		t.Fatal(e)
	}
	code, out, e := runCmd(t, "init")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	if !strings.Contains(out, "Bound "+root+" to Atlas / backend (matched by fingerprint)") {
		t.Fatalf("output: %s", out)
	}
	var req map[string]any
	_ = json.Unmarshal([]byte(f.bodies["POST /workspaces/connect"]), &req)
	if req["orgId"] != "org_1" || req["create"] != false || req["gitUrl"] != "github.com/acme/backend" || req["rootCommitSha"] == nil {
		t.Fatalf("connect body: %s", f.bodies["POST /workspaces/connect"])
	}
	if _, ok := req["workspaceId"]; ok {
		t.Fatal("absent fields must be omitted, not null")
	}
	b := binding(t, root)
	if b.Mode != "cloud" || *b.OrgID != "org_1" || *b.RemoteWorkspaceID != "ws_1" || *b.Slug != "backend" || b.ImportApproved {
		t.Fatalf("%+v", b)
	}
}

func TestInitCreate(t *testing.T) {
	gitRepo(t)
	installFakeAuth(t, routes())
	f := installFakeIngest(t, map[string]string{
		"GET /workspaces/slug-available": `{"available":true}`,
		"POST /workspaces":               `{"workspaceId":"ws_new"}`,
	})
	_, _, _ = runCmd(t, "login", "--no-browser")
	code, out, e := runCmd(t, "init", "--create", "--visibility", "restricted")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	var req map[string]any
	_ = json.Unmarshal([]byte(f.bodies["POST /workspaces"]), &req)
	if req["slug"] != "backend" || req["name"] != "backend" || req["visibility"] != "restricted" || req["orgId"] != "org_1" {
		t.Fatalf("create body: %s", f.bodies["POST /workspaces"])
	}
	if !strings.Contains(out, "(created)") {
		t.Fatal(out)
	}
}

func TestInitWithoutMatchNeedsAFlagWhenNotInteractive(t *testing.T) {
	gitRepo(t)
	installFakeAuth(t, routes())
	installFakeIngest(t, map[string]string{
		"POST /workspaces/connect": `{"status":"none"}`,
		"GET /workspaces":          `{"workspaces":[{"id":"ws_1","slug":"other"}]}`,
	})
	_, _, _ = runCmd(t, "login", "--no-browser")
	code, _, e := runCmd(t, "init")
	if code != 1 || !strings.Contains(e, "pass --project <id> (other (ws_1)) or --create") {
		t.Fatalf("exit %d: %s", code, e)
	}
}

func TestInitInSubfolderNeedsYes(t *testing.T) {
	root := gitRepo(t)
	t.Chdir(filepath.Join(root, "svc"))
	code, _, e := runCmd(t, "init", "--local")
	if code != 1 || !strings.Contains(e, "Capture is recorded for this folder only") || !strings.Contains(e, "--yes") {
		t.Fatalf("exit %d: %s", code, e)
	}
	if code, _, e := runCmd(t, "init", "--local", "--yes"); code != 0 {
		t.Fatal(e)
	}
	b := binding(t, filepath.Join(root, "svc"))
	if b.WorkspaceID != filepath.Join(root, "svc") {
		t.Fatalf("%+v", b)
	}
}

func TestInitRefusesWhileAnotherWriterHoldsTheLock(t *testing.T) {
	root := gitRepo(t)
	l, err := store.Lock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	code, _, e := runCmd(t, "init", "--local")
	if code != 1 || !strings.Contains(e, "another Atlas window is recording this Project") {
		t.Fatalf("exit %d: %s", code, e)
	}
}

func TestGatesAndStatus(t *testing.T) {
	root := gitRepo(t)
	installFakeAuth(t, routes())
	if code, _, e := runCmd(t, "init", "--local"); code != 0 {
		t.Fatal(e)
	}
	t.Chdir(filepath.Join(root, "svc")) // gates resolve the Project by walking up
	if code, _, e := runCmd(t, "memory", "disable"); code != 0 {
		t.Fatal(e)
	}
	if project.MemorySharing(root) {
		t.Fatal("memory still on")
	}
	if code, _, e := runCmd(t, "capture", "disable"); code != 0 {
		t.Fatal(e)
	}
	if binding(t, root).Enabled {
		t.Fatal("capture still on")
	}
	code, out, e := runCmd(t, "status")
	if code != 0 {
		t.Fatal(e)
	}
	for _, want := range []string{"Account:   not signed in", "Project:   " + root, "Binding:   local, not synced",
		"Memory:    off", "Capture:   paused", "Recorder:  idle"} {
		if !strings.Contains(out, want) {
			t.Errorf("status lacks %q:\n%s", want, out)
		}
	}
}
