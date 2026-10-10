package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/tryatlas/atlas-cli/internal/timefmt"
)

// Cursor is workspace_cursor: the last commit the walk evaluated.
type Cursor struct {
	LastSeen  *string
	Recovered bool
}

// GetCursor reads the Project's cursor; nil when there is no row.
func (s *Store) GetCursor(ctx context.Context, workspaceID string) (*Cursor, error) {
	var c Cursor
	err := s.db.QueryRowContext(ctx, `SELECT last_seen_commit, recovered FROM workspace_cursor WHERE workspace_id = ?`,
		workspaceID).Scan(&c.LastSeen, &c.Recovered)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// SetCursor advances the cursor.
func (s *Store) SetCursor(ctx context.Context, workspaceID, sha string, recovered bool, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspace_cursor (workspace_id, last_seen_commit, recovered, updated_at)
	    VALUES (?, ?, ?, ?) ON CONFLICT (workspace_id) DO UPDATE SET last_seen_commit = excluded.last_seen_commit,
	        recovered = excluded.recovered, updated_at = excluded.updated_at`,
		workspaceID, sha, recovered, timefmt.RFC3339(now))
	return err
}

// Touch is an unconsumed file_touch of a live session.
type Touch struct {
	SessionID     string
	StartedAt     time.Time
	Path          string
	SHA256After   *string
	SketchAfter   *string
	ExistedBefore bool
	Deleted       bool
}

// LiveTouches returns the unconsumed touches of live sessions (acp, native,
// proxy) of a Project, oldest session first.
func (s *Store) LiveTouches(ctx context.Context, workspaceID string) ([]Touch, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.session_id, s.started_at, f.path, f.sha256_after, f.sketch_after,
	        f.existed_before, f.deleted
	    FROM file_touch f JOIN agent_session s ON s.id = f.session_id
	    WHERE f.consumed_by_commit IS NULL AND f.out_of_repo = 0 AND s.workspace_id = ?
	        AND s.source IN ('acp', 'native', 'proxy')
	    ORDER BY s.started_at, f.seq`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Touch
	for rows.Next() {
		var t Touch
		var started string
		if err := rows.Scan(&t.SessionID, &started, &t.Path, &t.SHA256After, &t.SketchAfter, &t.ExistedBefore, &t.Deleted); err != nil {
			return nil, err
		}
		t.StartedAt = timefmt.Parse(started)
		out = append(out, t)
	}
	return out, rows.Err()
}

// Checkpoint is the slice of one session that landed in one commit.
type Checkpoint struct {
	SessionID    string
	CommitSHA    string
	PatchID      *string
	Branch       *string
	AuthorName   *string
	AuthorEmail  *string
	FilesTouched []string // commit-side paths
	Insertions   int64
	Deletions    int64
}

// UpsertCheckpoint writes a checkpoint keyed (session_id, commit_sha).
func (s *Store) UpsertCheckpoint(ctx context.Context, cp Checkpoint, now time.Time) error {
	files := append([]string(nil), cp.FilesTouched...)
	sort.Strings(files)
	files = dedup(files)
	fj, _ := json.Marshal(files)
	if files == nil {
		fj = []byte("[]")
	}
	syncState := s.initialSync(ctx)
	_, err := s.db.ExecContext(ctx, `INSERT INTO checkpoint (id, session_id, commit_sha, patch_id, link_state, branch,
	        git_author_name, git_author_email, files_touched, insertions, deletions, created_at, sync_state)
	    VALUES (?, ?, ?, ?, 'linked', ?, ?, ?, ?, ?, ?, ?, ?)
	    ON CONFLICT (session_id, commit_sha) DO UPDATE SET patch_id = excluded.patch_id, link_state = 'linked',
	        branch = COALESCE(excluded.branch, branch), git_author_name = excluded.git_author_name,
	        git_author_email = excluded.git_author_email, files_touched = excluded.files_touched,
	        insertions = excluded.insertions, deletions = excluded.deletions, `+resync,
		NewID("cp-"), cp.SessionID, cp.CommitSHA, cp.PatchID, cp.Branch, cp.AuthorName, cp.AuthorEmail, string(fj),
		cp.Insertions, cp.Deletions, timefmt.RFC3339(now), syncState)
	return err
}

func dedup(s []string) []string {
	var out []string
	for i, x := range s {
		if i == 0 || x != s[i-1] {
			out = append(out, x)
		}
	}
	return out
}

// ConsumeTouches marks a session's touches of a path as carried by a commit;
// only touches made up to one second after the commit count.
func (s *Store) ConsumeTouches(ctx context.Context, sessionID, path, sha string, commitTime time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE file_touch SET consumed_by_commit = ?
	    WHERE session_id = ? AND path = ? AND consumed_by_commit IS NULL AND created_at <= ?`,
		sha, sessionID, path, timefmt.RFC3339(commitTime.Add(time.Second)))
	return err
}
