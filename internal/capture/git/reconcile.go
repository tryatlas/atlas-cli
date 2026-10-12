package git

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/tryatlas/cli/internal/capture/store"
)

// Rewrites (evidence-capture.md §6.4): a checkpoint whose commit is no
// longer reachable is relinked to the one commit carrying the same patch-id,
// preferring its recorded branch when several do, else orphaned (the row is
// kept). An orphan that becomes reachable again is recovered.

// MassOrphan is the number of orphans in one pass that health surfaces.
const MassOrphan = 5

// Reconciled counts one pass.
type Reconciled struct {
	Orphaned, Relinked, Recovered, Failed int
}

// reach is a commit's reachability.
type reach int

const (
	reachable reach = iota
	unreachable
	probeFailed
)

func (g runner) reach(ctx context.Context, sha string) reach {
	if _, err := g.raw(ctx, "merge-base", "--is-ancestor", sha, "HEAD"); err == nil {
		return reachable
	}
	if out, err := g.out(ctx, "for-each-ref", "--format=%(refname)", "--contains", sha); err == nil && out != "" {
		return reachable
	}
	_, err := g.raw(ctx, "cat-file", "-e", sha+"^{commit}")
	var ee *exec.ExitError
	switch {
	case err == nil, errors.As(err, &ee):
		return unreachable // present but on no ref, or gone
	}
	return probeFailed
}

// patchIDs maps patch-id → commits over `rev-list --all --max-count 200`.
func (g runner) patchIDs(ctx context.Context) map[string][]string {
	out, err := g.out(ctx, "rev-list", "--all", "--max-count", strconv.Itoa(scanLimit))
	m := map[string][]string{}
	if err != nil {
		return m
	}
	for _, sha := range strings.Fields(out) {
		if p := g.patchID(ctx, sha); p != nil {
			m[*p] = append(m[*p], sha)
		}
	}
	return m
}

func (g runner) onBranch(ctx context.Context, sha, branch string) bool {
	out, err := g.out(ctx, "branch", "--format=%(refname:short)", "--contains", sha)
	if err != nil {
		return false
	}
	for _, b := range strings.Split(out, "\n") {
		if strings.TrimSpace(b) == branch {
			return true
		}
	}
	return false
}

// Reconcile checks every checkpoint of the Project against the history.
func Reconcile(ctx context.Context, st *store.Store, root string, now func() time.Time) (Reconciled, error) {
	var r Reconciled
	g := runner{dir: root}
	if _, err := g.out(ctx, "rev-parse", "-q", "--verify", "HEAD"); err != nil || g.midOperation(ctx) {
		return r, nil
	}
	cps, err := st.ProjectCheckpoints(ctx, root)
	if err != nil || len(cps) == 0 {
		return r, err
	}
	var ids map[string][]string
	for _, cp := range cps {
		switch g.reach(ctx, cp.CommitSHA) {
		case probeFailed:
			r.Failed++
			continue
		case reachable:
			if cp.LinkState == "orphaned" {
				if err := st.SetLinkState(ctx, cp.ID, "linked"); err != nil {
					return r, err
				}
				r.Recovered++
			}
			continue
		}
		if cp.LinkState == "orphaned" {
			continue
		}
		target := ""
		if cp.PatchID != nil {
			if ids == nil {
				ids = g.patchIDs(ctx)
			}
			var cands []string
			for _, c := range ids[*cp.PatchID] {
				if c != cp.CommitSHA {
					cands = append(cands, c)
				}
			}
			switch {
			case len(cands) == 1:
				target = cands[0]
			case len(cands) > 1 && cp.Branch != nil:
				var on []string
				for _, c := range cands {
					if g.onBranch(ctx, c, *cp.Branch) {
						on = append(on, c)
					}
				}
				if len(on) == 1 {
					target = on[0]
				}
			}
		}
		if target != "" {
			if err := st.RelinkCheckpoint(ctx, cp, target); err != nil {
				return r, err
			}
			r.Relinked++
			continue
		}
		if err := st.SetLinkState(ctx, cp.ID, "orphaned"); err != nil {
			return r, err
		}
		r.Orphaned++
	}
	if r.Orphaned+r.Relinked+r.Failed > 0 {
		if err := st.SetReconcileNote(ctx, root, r.Orphaned, r.Relinked, r.Failed, now()); err != nil {
			return r, err
		}
	}
	return r, nil
}
