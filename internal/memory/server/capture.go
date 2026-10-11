package server

import (
	"context"
	"path/filepath"

	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/project"
	"github.com/tryatlas/atlas-cli/internal/timefmt"
)

// capture reads the Project's sessions.db for memory_why and provenance. It
// never writes.
type capture struct {
	on   bool
	root string
	st   *store.Store
}

type capCommit struct{ sha, branch string }

type capSession struct {
	id, native, agent, title, date string
	commits                        []capCommit
}

func (s *capSession) commitList() []any {
	out := []any{}
	for _, c := range s.commits {
		m := map[string]any{"sha": c.sha}
		if c.branch != "" {
			m["branch"] = c.branch
		}
		out = append(out, m)
	}
	return out
}

func openCapture(ctx context.Context, cwd string) *capture {
	p, err := project.Discover(ctx, cwd)
	if err != nil {
		return &capture{}
	}
	st, err := store.Open(p.Root, false)
	if err != nil {
		return &capture{}
	}
	b, err := st.Binding(ctx)
	if err != nil || b == nil {
		_ = st.Close()
		return &capture{}
	}
	return &capture{on: b.Enabled, root: p.Root, st: st}
}

func (c *capture) close() {
	if c.st != nil {
		_ = c.st.Close()
	}
}

const sessionCols = `s.id, s.native_session_id, COALESCE(s.agent, ''), COALESCE(s.title, ''), s.started_at`

func (c *capture) sessions(q string, args ...any) []*capSession {
	if c.st == nil {
		return nil
	}
	rows, err := c.st.DB().Query(q, args...)
	if err != nil {
		return nil
	}
	var out []*capSession
	for rows.Next() {
		var s capSession
		var started string
		if rows.Scan(&s.id, &s.native, &s.agent, &s.title, &started) == nil {
			s.date = timefmt.Parse(started).Format("2006-01-02")
			out = append(out, &s)
		}
	}
	_ = rows.Close()
	for _, s := range out {
		s.commits = c.commits(s.id)
	}
	return out
}

func (c *capture) commits(sessionID string) []capCommit {
	rows, err := c.st.DB().Query(`SELECT commit_sha, COALESCE(branch, '') FROM checkpoint WHERE session_id = ? ORDER BY created_at`, sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []capCommit
	for rows.Next() {
		var cc capCommit
		if rows.Scan(&cc.sha, &cc.branch) == nil {
			out = append(out, cc)
		}
	}
	return out
}

// session looks a recorded session up by the agent's own session id.
func (c *capture) session(native string) *capSession {
	if native == "" {
		return nil
	}
	list := c.sessions(`SELECT `+sessionCols+` FROM agent_session s WHERE s.native_session_id = ? ORDER BY s.started_at DESC LIMIT 1`, native)
	if len(list) == 0 {
		return nil
	}
	return list[0]
}

// byPath finds the sessions that touched a file, newest first.
func (c *capture) byPath(abs string) []*capSession {
	if c.st == nil {
		return nil
	}
	rel, err := filepath.Rel(c.root, abs)
	if err != nil {
		return nil
	}
	return c.sessions(`SELECT DISTINCT `+sessionCols+` FROM file_touch ft JOIN agent_session s ON s.id = ft.session_id
	    WHERE ft.path = ? ORDER BY s.started_at DESC`, filepath.ToSlash(rel))
}

// byCommit finds the sessions with a checkpoint at a commit (prefix match).
func (c *capture) byCommit(sha string) []*capSession {
	return c.sessions(`SELECT DISTINCT `+sessionCols+` FROM checkpoint cp JOIN agent_session s ON s.id = cp.session_id
	    WHERE cp.commit_sha LIKE ? ORDER BY s.started_at DESC`, sha+"%")
}
