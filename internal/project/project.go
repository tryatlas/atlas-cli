package project

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tryatlas/cli/internal/capture/store"
)

// ErrNotInitialised means no binding was found from the cwd up to the scope root.
var ErrNotInitialised = errors.New("this folder is not an Atlas Project; run atlas-cli init")

// Project is a bound folder as seen from a working directory.
type Project struct {
	Root      string // canonical launch dir holding .atlas/sessions.db (capture's key)
	ScopeRoot string // canonical repository root (shared memory's key)
	Cwd       string // canonical directory the lookup started from
}

// Canonical spells a path the one way the desktop does: absolute, symlinks
// resolved, no trailing slash (/private/var/… rather than /var/… on macOS).
func Canonical(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

// gitTimeout bounds every git call: a hung git must not hang the CLI.
const gitTimeout = 10 * time.Second

// gitOut runs a read-only git command in dir and returns trimmed stdout.
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: fixed read-only git subcommands
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

// ScopeRoot is the repository that owns dir, so every worktree and
// subdirectory launch of one repo shares one memory (SPEC §4.1): the git
// common dir's parent when it is named .git, the common dir of a bare
// repository, else the top level; outside git, dir itself.
func ScopeRoot(ctx context.Context, dir string) string {
	self, err := Canonical(dir)
	if err != nil {
		self = filepath.Clean(dir)
	}
	common, err := gitOut(ctx, self, "rev-parse", "--git-common-dir")
	if err != nil || common == "" {
		return self
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(self, common)
	}
	if c, err := Canonical(common); err == nil {
		common = c
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	if bare, err := gitOut(ctx, self, "rev-parse", "--is-bare-repository"); err == nil && bare == "true" {
		return common
	}
	if top, err := gitOut(ctx, self, "rev-parse", "--show-toplevel"); err == nil && top != "" {
		if c, err := Canonical(top); err == nil {
			return c
		}
	}
	return self
}

// Discover finds the Project for cwd: the nearest directory from cwd upward
// that holds .atlas/sessions.db with a binding row, stopping at the scope
// root (ADR-0007). Nested bindings resolve to the nearest.
func Discover(ctx context.Context, cwd string) (*Project, error) {
	start, err := Canonical(cwd)
	if err != nil {
		return nil, err
	}
	scope := ScopeRoot(ctx, start)
	for dir := start; ; dir = filepath.Dir(dir) {
		if bound(ctx, dir) {
			return &Project{Root: dir, ScopeRoot: scope, Cwd: start}, nil
		}
		if dir == scope || dir == filepath.Dir(dir) || !within(dir, scope) {
			return nil, ErrNotInitialised
		}
	}
}

// within reports whether dir is scope or below it.
func within(dir, scope string) bool {
	rel, err := filepath.Rel(scope, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func bound(ctx context.Context, dir string) bool {
	if !store.Exists(dir) {
		return false
	}
	s, err := store.Open(dir, false)
	if err != nil {
		return false
	}
	defer s.Close()
	b, err := s.Binding(ctx)
	return err == nil && b != nil
}

// ── Git fingerprint ──────────────────────────────────────────────────────────

// GitFingerprint identifies a repository to the server: its root commit and
// normalised origin URL. Absent values stay nil and are never sent as null.
type GitFingerprint struct {
	RootCommitSHA *string
	Shallow       bool
	GitURL        *string
}

// Fingerprint reads the root commit (last line of rev-list --max-parents=0),
// whether the clone is shallow, and the normalised origin URL.
func Fingerprint(ctx context.Context, dir string) GitFingerprint {
	var fp GitFingerprint
	if out, err := gitOut(ctx, dir, "rev-list", "--max-parents=0", "HEAD"); err == nil && out != "" {
		lines := strings.Split(out, "\n")
		sha := strings.TrimSpace(lines[len(lines)-1])
		fp.RootCommitSHA = &sha
	}
	if out, err := gitOut(ctx, dir, "rev-parse", "--is-shallow-repository"); err == nil {
		fp.Shallow = out == "true"
	}
	if out, err := gitOut(ctx, dir, "remote", "get-url", "origin"); err == nil && out != "" {
		u := NormalizeGitURL(out)
		fp.GitURL = &u
	}
	return fp
}

// NormalizeGitURL strips the scheme and userinfo, turns scp-style
// "git@host:path" into "host/path", removes a trailing "/" and ".git", and
// lower-cases.
func NormalizeGitURL(u string) string {
	u = strings.TrimSpace(u)
	scheme := false
	if i := strings.Index(u, "://"); i >= 0 {
		u = u[i+3:]
		scheme = true
	}
	if i := strings.Index(u, "@"); i >= 0 && (strings.IndexByte(u, '/') < 0 || i < strings.IndexByte(u, '/')) {
		u = u[i+1:]
	}
	if !scheme {
		// scp form host:path (a colon before the first slash, not a port)
		if i := strings.IndexByte(u, ':'); i >= 0 && (strings.IndexByte(u, '/') < 0 || i < strings.IndexByte(u, '/')) {
			u = u[:i] + "/" + u[i+1:]
		}
	}
	u = strings.TrimRight(u, "/")
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimRight(u, "/")
	return strings.ToLower(u)
}

// ── Memory sharing gate ──────────────────────────────────────────────────────

const sharingFile = "memory-sharing.json"

// MemorySharing reads <dir>/.atlas/memory-sharing.json. Absent or unparsable
// means on.
func MemorySharing(dir string) bool {
	b, err := os.ReadFile(filepath.Join(dir, store.DirName, sharingFile)) //nolint:gosec // G304: the Project's own gate file
	if err != nil {
		return true
	}
	var v struct {
		Enabled *bool `json:"enabled"`
	}
	if json.Unmarshal(b, &v) != nil || v.Enabled == nil {
		return true
	}
	return *v.Enabled
}

// SetMemorySharing writes the gate pretty-printed through a temp file and rename.
func SetMemorySharing(dir string, enabled bool) error {
	d := filepath.Join(dir, store.DirName)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	body, _ := json.MarshalIndent(map[string]bool{"enabled": enabled}, "", "  ")
	tmp := filepath.Join(d, sharingFile+".tmp")
	if err := os.WriteFile(tmp, body, 0o644); err != nil { //nolint:gosec // G306: a flag, not a secret
		return err
	}
	return os.Rename(tmp, filepath.Join(d, sharingFile))
}

// Worktrees lists the repository's other worktrees (git worktree list), canonical.
func Worktrees(ctx context.Context, dir string) []string {
	out, err := gitOut(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	var list []string
	for _, line := range strings.Split(out, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			if c, err := Canonical(p); err == nil {
				list = append(list, c)
			}
		}
	}
	return list
}
