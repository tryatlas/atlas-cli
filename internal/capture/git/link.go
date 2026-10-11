package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

// The walk that links captured sessions to the commits that carried their
// work (SPEC §6.6, evidence-capture.md §6.2–6.3). This is the watcher-free
// version: it runs when a listener stops and on atlas capture link, after
// reconciling rewrites (reconcile.go).

// scanLimit bounds a scan without a usable cursor.
const scanLimit = 200

// Result counts one walk.
type Result struct {
	Commits     int // commits evaluated
	Checkpoints int // checkpoint rows written
	Rewrites    Reconciled
}

// Link walks first-parent history from the cursor to HEAD.
func Link(ctx context.Context, st *store.Store, root string, now func() time.Time) (Result, error) {
	var res Result
	g := runner{dir: root}
	if _, err := g.out(ctx, "rev-parse", "-q", "--verify", "HEAD"); err != nil {
		return res, nil // not a repository, or unborn
	}
	if g.midOperation(ctx) {
		return res, nil
	}
	rw, err := Reconcile(ctx, st, root, now)
	if err != nil {
		return res, err
	}
	res.Rewrites = rw
	head, err := g.out(ctx, "rev-parse", "HEAD")
	if err != nil {
		return res, err
	}
	cur, err := st.GetCursor(ctx, root)
	if err != nil {
		return res, err
	}
	touches, err := st.LiveTouches(ctx, root)
	if err != nil {
		return res, err
	}
	if cur == nil && len(touches) == 0 {
		return res, st.SetCursor(ctx, root, head, false, now())
	}
	var list string
	recovered := false
	switch {
	case cur != nil && cur.LastSeen != nil && g.isCommit(ctx, *cur.LastSeen):
		list, err = g.out(ctx, "rev-list", "--first-parent", "--reverse", *cur.LastSeen+"..HEAD")
	default:
		recovered = cur != nil // a cursor whose commit is gone
		list, err = g.out(ctx, "rev-list", "--first-parent", "--reverse", "--max-count", strconv.Itoa(scanLimit), "HEAD")
	}
	if err != nil {
		return res, err
	}
	for _, sha := range strings.Fields(list) {
		n, err := evaluate(ctx, g, st, sha, touches, now)
		if err != nil {
			return res, err
		}
		res.Commits++
		res.Checkpoints += n
		if err := st.SetCursor(ctx, root, sha, recovered, now()); err != nil {
			return res, err
		}
		if n > 0 {
			if touches, err = st.LiveTouches(ctx, root); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

type commit struct {
	sha, author, email string
	parents            []string
	time               time.Time
}

// evaluate links one first-parent commit; a merge creates no checkpoint and
// its side commits are evaluated instead.
func evaluate(ctx context.Context, g runner, st *store.Store, sha string, touches []store.Touch, now func() time.Time) (int, error) {
	c, err := g.commit(ctx, sha)
	if err != nil {
		return 0, err
	}
	if len(c.parents) > 1 {
		side, err := g.out(ctx, "rev-list", "--no-merges", "--reverse", "--max-count", strconv.Itoa(scanLimit), c.parents[0]+".."+c.parents[1])
		if err != nil {
			return 0, err
		}
		total := 0
		for _, s := range strings.Fields(side) {
			n, err := evaluate(ctx, g, st, s, touches, now)
			if err != nil {
				return total, err
			}
			total += n
		}
		return total, nil
	}
	changes, err := g.changes(ctx, sha)
	if err != nil || len(changes) == 0 {
		return 0, err
	}
	bySession := map[string][]store.Touch{}
	var order []string
	for _, t := range touches {
		if t.StartedAt.Unix() > c.time.Unix() {
			continue // a session that started after the commit did not make it
		}
		if _, ok := bySession[t.SessionID]; !ok {
			order = append(order, t.SessionID)
		}
		bySession[t.SessionID] = append(bySession[t.SessionID], t)
	}
	written := 0
	var stats *[2]int64
	for _, sid := range order {
		var commitPaths, touchPaths []string
		for _, ch := range changes {
			for _, t := range bySession[sid] {
				if !ch.matches(t.Path) || !links(ctx, g, sha, ch, t) {
					continue
				}
				commitPaths = append(commitPaths, ch.path)
				touchPaths = append(touchPaths, t.Path)
				break
			}
		}
		if len(commitPaths) == 0 {
			continue
		}
		if stats == nil {
			ins, del := g.numstat(ctx, sha)
			stats = &[2]int64{ins, del}
		}
		cp := store.Checkpoint{SessionID: sid, CommitSHA: sha, PatchID: g.patchID(ctx, sha), Branch: g.branch(ctx),
			AuthorName: &c.author, AuthorEmail: &c.email, FilesTouched: commitPaths, Insertions: stats[0], Deletions: stats[1]}
		if err := st.UpsertCheckpoint(ctx, cp, now()); err != nil {
			return written, err
		}
		for _, p := range touchPaths {
			if err := st.ConsumeTouches(ctx, sid, p, sha, c.time); err != nil {
				return written, err
			}
		}
		written++
	}
	return written, nil
}

// change is one entry of `git show --name-status -M`.
type change struct {
	status string // A M D T R…
	path   string // commit-side path
	old    string // rename origin
}

func (ch change) matches(touchPath string) bool {
	return strings.EqualFold(ch.path, touchPath) || (ch.old != "" && strings.EqualFold(ch.old, touchPath))
}

// links is the asymmetric rule (evidence-capture.md §6.3).
func links(ctx context.Context, g runner, sha string, ch change, t store.Touch) bool {
	if ch.status == "D" {
		return t.Deleted
	}
	existedInParent := ch.status != "A" && !strings.HasPrefix(ch.status, "R") && !strings.HasPrefix(ch.status, "C")
	if existedInParent && t.ExistedBefore {
		return true
	}
	if t.SHA256After == nil {
		return false
	}
	if blob, err := g.raw(ctx, "cat-file", "blob", sha+":"+ch.path); err == nil && hexSHA(blob) == *t.SHA256After {
		return true
	}
	if blob, err := g.raw(ctx, "cat-file", "--filters", sha+":"+ch.path); err == nil {
		if hexSHA(blob) == *t.SHA256After {
			return true
		}
		return retainsWork(t.SketchAfter, record.Sketch(blob))
	}
	return false
}

// retainsWork: at least half of the agent's line hashes survive in the
// committed file.
func retainsWork(agent, committed *string) bool {
	if agent == nil || committed == nil {
		return false
	}
	have := map[string]bool{}
	for _, h := range strings.Split(*committed, ",") {
		have[h] = true
	}
	mine := strings.Split(*agent, ",")
	kept := 0
	for _, h := range mine {
		if have[h] {
			kept++
		}
	}
	return len(mine) > 0 && kept*2 >= len(mine)
}

func hexSHA(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ── git, read-only ───────────────────────────────────────────────────────────

type runner struct{ dir string }

func (g runner) raw(ctx context.Context, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.dir}, args...)...) //nolint:gosec // G204: fixed read-only git subcommands
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	return cmd.Output()
}

func (g runner) out(ctx context.Context, args ...string) (string, error) {
	b, err := g.raw(ctx, args...)
	return strings.TrimSpace(string(b)), err
}

func (g runner) isCommit(ctx context.Context, sha string) bool {
	_, err := g.raw(ctx, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// midOperation reports a rebase, cherry-pick or merge in progress.
func (g runner) midOperation(ctx context.Context) bool {
	for _, p := range []string{"rebase-merge", "rebase-apply", "CHERRY_PICK_HEAD", "MERGE_HEAD"} {
		path, err := g.out(ctx, "rev-parse", "--git-path", p)
		if err != nil {
			continue
		}
		if !strings.HasPrefix(path, "/") {
			path = g.dir + "/" + path
		}
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func (g runner) commit(ctx context.Context, sha string) (commit, error) {
	out, err := g.out(ctx, "show", "--no-patch", "--format=%H%x1f%an%x1f%ae%x1f%P%x1f%ct%x1f%s", sha)
	if err != nil {
		return commit{}, err
	}
	f := strings.Split(out, "\x1f")
	if len(f) < 5 {
		return commit{}, fmt.Errorf("unreadable commit %s", sha)
	}
	ct, _ := strconv.ParseInt(f[4], 10, 64)
	return commit{sha: f[0], author: f[1], email: f[2], parents: strings.Fields(f[3]), time: time.Unix(ct, 0).UTC()}, nil
}

func (g runner) changes(ctx context.Context, sha string) ([]change, error) {
	b, err := g.raw(ctx, "show", "--first-parent", "--name-status", "-M", "--format=", "-z", sha)
	if err != nil {
		return nil, err
	}
	f := strings.Split(strings.TrimLeft(string(b), "\n"), "\x00")
	var out []change
	for i := 0; i < len(f); i++ {
		st := strings.TrimSpace(f[i])
		if st == "" {
			continue
		}
		if (strings.HasPrefix(st, "R") || strings.HasPrefix(st, "C")) && i+2 < len(f) {
			out = append(out, change{status: st, old: f[i+1], path: f[i+2]})
			i += 2
			continue
		}
		if i+1 < len(f) {
			out = append(out, change{status: st, path: f[i+1]})
			i++
		}
	}
	return out, nil
}

func (g runner) numstat(ctx context.Context, sha string) (int64, int64) {
	out, err := g.out(ctx, "show", "--first-parent", "--numstat", "--format=", sha)
	if err != nil {
		return 0, 0
	}
	var ins, del int64
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		a, _ := strconv.ParseInt(f[0], 10, 64) // "-" for binary files counts 0
		d, _ := strconv.ParseInt(f[1], 10, 64)
		ins += a
		del += d
	}
	return ins, del
}

// patchID is `git patch-id --stable` over the commit's first-parent diff;
// nil for an empty diff.
func (g runner) patchID(ctx context.Context, sha string) *string {
	diff, err := g.raw(ctx, "diff-tree", "-p", "--first-parent", "--root", sha)
	if err != nil || len(bytes.TrimSpace(diff)) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", g.dir, "patch-id", "--stable") //nolint:gosec // G204: fixed subcommand
	cmd.Stdin = bytes.NewReader(diff)
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	f := strings.Fields(string(out))
	if len(f) == 0 {
		return nil
	}
	return &f[0]
}

func (g runner) branch(ctx context.Context) *string {
	b, err := g.out(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil || b == "" {
		return nil
	}
	return &b
}
