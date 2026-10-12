package hooks

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// gitStatus is the set of paths git status reports under root, renames
// counted on both sides; nil outside a repository.
func gitStatus(ctx context.Context, root string) map[string]bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "status", "--porcelain=v1", "-z", "--untracked-files=all") //nolint:gosec // G204: fixed read-only git subcommand
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	fields := strings.Split(string(b), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		out[f[3:]] = true
		if f[0] == 'R' || f[0] == 'C' {
			if i+1 < len(fields) && fields[i+1] != "" {
				out[fields[i+1]] = true
			}
			i++
		}
	}
	return out
}

// changedSince is what git status reports now that it did not report at the
// turn start, plus paths it reported then that are gone from the tree now (an
// untracked file the turn deleted). Paths dirty both before and after cannot
// be told apart and are left out.
func changedSince(root string, before, after map[string]bool) []string {
	var out []string
	for p := range after {
		if !before[p] {
			out = append(out, p)
		}
	}
	for p := range before {
		if after[p] {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(p))); err != nil {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// relPath makes an agent-reported path Project-relative with "/"; a path
// outside root is returned absolute.
func relPath(root, p string) string {
	if p == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(root, p)
	}
	if rel, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return p
}

// gitHead is HEAD's commit, or "" (unborn, not a repository).
func gitHead(ctx context.Context, root string) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "-q", "--verify", "HEAD") //nolint:gosec // G204: fixed read-only git subcommand
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// committedSince are the paths changed by the commits from..HEAD: files the
// turn committed, which git status no longer shows.
func committedSince(ctx context.Context, root, from string) []string {
	if from == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "diff", "--name-only", "-z", "--no-renames", from, "HEAD") //nolint:gosec // G204: fixed read-only git subcommand
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	b, err := cmd.Output()
	if err != nil {
		return nil
	}
	var out []string
	for _, p := range strings.Split(string(b), "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
