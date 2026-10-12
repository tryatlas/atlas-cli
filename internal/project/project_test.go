package project

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tryatlas/cli/internal/capture/store"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@example.test",
		"-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// canon is the expected spelling: symlinks resolved (/private/var on macOS).
func canon(t *testing.T, p string) string {
	t.Helper()
	c, err := Canonical(p)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(dir, "sub", "deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "first")
	return dir
}

func TestCanonicalResolvesSymlinksAndTrailingSlash(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	_ = os.Mkdir(target, 0o755)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	got, err := Canonical(link + "/")
	if err != nil {
		t.Fatal(err)
	}
	if got != canon(t, target) || got[len(got)-1] == '/' {
		t.Fatalf("got %q", got)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("not absolute: %q", got)
	}
}

func TestScopeRoot(t *testing.T) {
	repo := newRepo(t)
	root := canon(t, repo)
	ctx := context.Background()
	if got := ScopeRoot(ctx, filepath.Join(repo, "sub", "deeper")); got != root {
		t.Errorf("subdirectory: %q, want %q", got, root)
	}
	// A linked worktree shares the main worktree's memory.
	wt := filepath.Join(filepath.Dir(repo), "wt")
	git(t, repo, "worktree", "add", "-q", wt, "-b", "feature")
	if got := ScopeRoot(ctx, wt); got != root {
		t.Errorf("worktree: %q, want %q", got, root)
	}
	// A bare repository is its own scope.
	bare := filepath.Join(t.TempDir(), "bare.git")
	git(t, filepath.Dir(bare), "init", "-q", "--bare", bare)
	if got := ScopeRoot(ctx, bare); got != canon(t, bare) {
		t.Errorf("bare: %q", got)
	}
	// Outside git: the directory itself.
	plain := t.TempDir()
	if got := ScopeRoot(ctx, plain); got != canon(t, plain) {
		t.Errorf("outside git: %q", got)
	}
}

func TestFingerprint(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	git(t, repo, "remote", "add", "origin", "git@GitHub.com:Acme/Backend.git")
	fp := Fingerprint(ctx, repo)
	want := git(t, repo, "rev-list", "--max-parents=0", "HEAD")
	if fp.RootCommitSHA == nil || *fp.RootCommitSHA+"\n" != want {
		t.Fatalf("root commit %v, want %q", fp.RootCommitSHA, want)
	}
	if fp.GitURL == nil || *fp.GitURL != "github.com/acme/backend" || fp.Shallow {
		t.Fatalf("%+v", fp)
	}
	empty := t.TempDir()
	git(t, empty, "init", "-q")
	if fp := Fingerprint(ctx, empty); fp.RootCommitSHA != nil || fp.GitURL != nil {
		t.Fatalf("empty repo: %+v", fp)
	}
}

func TestNormalizeGitURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://github.com/Acme/Backend.git":       "github.com/acme/backend",
		"https://user:tok@github.com/acme/backend/": "github.com/acme/backend",
		"git@github.com:Acme/Backend.git":           "github.com/acme/backend",
		"ssh://git@gitlab.example.com/a/b.git":      "gitlab.example.com/a/b",
		"github.com/acme/backend":                   "github.com/acme/backend",
	} {
		if got := NormalizeGitURL(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

func bind(t *testing.T, dir string) {
	t.Helper()
	s, err := store.Open(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c := canon(t, dir)
	if err := s.PutBinding(context.Background(), store.Binding{WorkspaceID: c, Root: c, Mode: store.ModeLocal, Enabled: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverWalksUpToTheNearestBinding(t *testing.T) {
	repo := newRepo(t)
	ctx := context.Background()
	deeper := filepath.Join(repo, "sub", "deeper")

	if p, err := Discover(ctx, deeper); !errors.Is(err, ErrNotInitialised) || p != nil {
		t.Fatalf("unbound: %+v %v", p, err)
	}
	bind(t, repo)
	p, err := Discover(ctx, deeper)
	if err != nil {
		t.Fatal(err)
	}
	if p.Root != canon(t, repo) || p.ScopeRoot != canon(t, repo) || p.Cwd != canon(t, deeper) {
		t.Fatalf("%+v", p)
	}
	// A nested binding wins.
	bind(t, filepath.Join(repo, "sub"))
	p, _ = Discover(ctx, deeper)
	if p.Root != canon(t, filepath.Join(repo, "sub")) {
		t.Fatalf("nested: %+v", p)
	}
	// A store without a binding row does not count.
	other := newRepo(t)
	s, _ := store.Open(other, true)
	s.Close()
	if _, err := Discover(ctx, other); !errors.Is(err, ErrNotInitialised) {
		t.Fatalf("store without binding: %v", err)
	}
}

func TestDiscoverStopsAtTheScopeRoot(t *testing.T) {
	outer := t.TempDir()
	bind(t, outer) // a binding above the repository must not be found
	repo := filepath.Join(outer, "repo")
	_ = os.Mkdir(repo, 0o755)
	git(t, repo, "init", "-q")
	if _, err := Discover(context.Background(), repo); !errors.Is(err, ErrNotInitialised) {
		t.Fatalf("crossed the scope root: %v", err)
	}
}

func TestMemorySharingGate(t *testing.T) {
	dir := t.TempDir()
	if !MemorySharing(dir) {
		t.Fatal("absent file must mean on")
	}
	if err := SetMemorySharing(dir, false); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, ".atlas", "memory-sharing.json"))
	if string(b) != "{\n  \"enabled\": false\n}" {
		t.Fatalf("file = %q", b)
	}
	if MemorySharing(dir) {
		t.Fatal("still on")
	}
	_ = os.WriteFile(filepath.Join(dir, ".atlas", "memory-sharing.json"), []byte("{garbage"), 0o644)
	if !MemorySharing(dir) {
		t.Fatal("unparsable file must mean on")
	}
}
