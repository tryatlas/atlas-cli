package hooks

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// The git post-commit hook: it asks atlas-cli to link captured sessions to the
// new commit (atlas-cli hooks git post-commit). An existing hook is kept as
// <hook>.pre-atlas and chained after Atlas's part.

// GitHookMarker identifies a hook file Atlas wrote.
const GitHookMarker = "# Atlas CLI hook"

// gitHooks are the hooks Atlas installs.
var gitHooks = []string{"post-commit"}

// gitHookScript is the hook body: run atlas-cli if present, then the hook that
// was there before, if any.
func gitHookScript(name string) string {
	return "#!/bin/sh\n" + GitHookMarker + "\n" +
		"if command -v " + Bin + " >/dev/null 2>&1; then " + Prefix + "git " + name + " \"$@\" </dev/null >/dev/null 2>&1 || true; fi\n" +
		"if [ -x \"$(dirname \"$0\")/" + name + ".pre-atlas\" ]; then exec \"$(dirname \"$0\")/" + name + ".pre-atlas\" \"$@\"; fi\n"
}

// hooksDir is the repository's active hooks directory (core.hooksPath
// respected), or "" outside a repository.
func hooksDir(ctx context.Context, root string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "--git-path", "hooks") //nolint:gosec // G204: fixed read-only git subcommand
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	p := strings.TrimSpace(string(out))
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	return p
}

// InstallGitHooks writes Atlas's git hooks, keeping any existing hook as
// <hook>.pre-atlas. It reports whether anything changed; outside a
// repository it does nothing.
func InstallGitHooks(ctx context.Context, root string) (bool, error) {
	dir := hooksDir(ctx, root)
	if dir == "" {
		return false, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return false, err
	}
	changed := false
	for _, name := range gitHooks {
		p := filepath.Join(dir, name)
		want := []byte(gitHookScript(name))
		cur, err := os.ReadFile(p) //nolint:gosec // G304: the repository's own hook
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			return changed, err
		case bytes.Equal(cur, want):
			continue
		case bytes.Contains(cur, []byte(GitHookMarker)):
			// An older Atlas hook: rewrite in place.
		default:
			backup := p + ".pre-atlas"
			if _, err := os.Stat(backup); err == nil && !bytes.Equal(mustRead(backup), cur) {
				backup += "." + time.Now().UTC().Format("20060102T150405Z")
			}
			if err := os.WriteFile(backup, cur, 0o755); err != nil { //nolint:gosec // G306: a hook must be executable
				return changed, err
			}
		}
		if err := os.WriteFile(p, want, 0o755); err != nil { //nolint:gosec // G306: a hook must be executable
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

func mustRead(p string) []byte {
	b, _ := os.ReadFile(p) //nolint:gosec // G304: the repository's own hook backup
	return b
}

// UninstallGitHooks removes Atlas's hooks and restores the .pre-atlas ones.
func UninstallGitHooks(ctx context.Context, root string) error {
	dir := hooksDir(ctx, root)
	if dir == "" {
		return nil
	}
	for _, name := range gitHooks {
		p := filepath.Join(dir, name)
		cur, err := os.ReadFile(p) //nolint:gosec // G304: the repository's own hook
		if err != nil || !bytes.Contains(cur, []byte(GitHookMarker)) {
			continue
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		if _, err := os.Stat(p + ".pre-atlas"); err == nil {
			if err := os.Rename(p+".pre-atlas", p); err != nil {
				return err
			}
		}
	}
	return nil
}

// GitHooksState reports whether Atlas's git hooks are in place.
func GitHooksState(ctx context.Context, root string) InstallState {
	dir := hooksDir(ctx, root)
	if dir == "" {
		return Absent
	}
	for _, name := range gitHooks {
		cur, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: the repository's own hook
		if err != nil {
			return Absent
		}
		if !bytes.Equal(cur, []byte(gitHookScript(name))) {
			if bytes.Contains(cur, []byte(GitHookMarker)) {
				return Outdated
			}
			return Absent
		}
	}
	return Current
}
