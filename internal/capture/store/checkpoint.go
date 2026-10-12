package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/tryatlas/cli/internal/timefmt"
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

// LinkedCheckpoint is a checkpoint as reconciliation sees it.
type LinkedCheckpoint struct {
	ID, SessionID, CommitSHA, LinkState string
	PatchID, Branch                     *string
}

// ProjectCheckpoints lists a Project's checkpoints.
func (s *Store) ProjectCheckpoints(ctx context.Context, workspaceID string) ([]LinkedCheckpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.session_id, c.commit_sha, c.link_state, c.patch_id, c.branch
	    FROM checkpoint c JOIN agent_session s ON s.id = c.session_id WHERE s.workspace_id = ? ORDER BY c.created_at`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LinkedCheckpoint
	for rows.Next() {
		var c LinkedCheckpoint
		if err := rows.Scan(&c.ID, &c.SessionID, &c.CommitSHA, &c.LinkState, &c.PatchID, &c.Branch); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetLinkState marks a checkpoint linked or orphaned; a sent row sends again.
func (s *Store) SetLinkState(ctx context.Context, id, state string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE checkpoint SET link_state = ?, `+resync+` WHERE id = ?`, state, id)
	return err
}

// RelinkCheckpoint re-points a checkpoint at the commit that now carries the
// same change. When the session already has a row at that commit, that row
// absorbs this one.
func (s *Store) RelinkCheckpoint(ctx context.Context, cp LinkedCheckpoint, sha string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT id FROM checkpoint WHERE session_id = ? AND commit_sha = ?`, cp.SessionID, sha).Scan(&existing)
	switch {
	case err == nil:
		if _, err := tx.ExecContext(ctx, `DELETE FROM checkpoint WHERE id = ?`, cp.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE checkpoint SET link_state = 'linked', `+resync+` WHERE id = ?`, existing); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `UPDATE checkpoint SET commit_sha = ?, link_state = 'linked', `+resync+` WHERE id = ?`, sha, cp.ID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE file_touch SET consumed_by_commit = ? WHERE session_id = ? AND consumed_by_commit = ?`,
			sha, cp.SessionID, cp.CommitSHA); err != nil {
			return err
		}
	default:
		return err
	}
	return tx.Commit()
}

// SetReconcileNote records {"orphaned","relinked","failed","at"} on the cursor.
func (s *Store) SetReconcileNote(ctx context.Context, workspaceID string, orphaned, relinked, failed int, now time.Time) error {
	note, _ := json.Marshal(struct {
		Orphaned int    `json:"orphaned"`
		Relinked int    `json:"relinked"`
		Failed   int    `json:"failed"`
		At       string `json:"at"`
	}{orphaned, relinked, failed, timefmt.RFC3339(now)})
	_, err := s.db.ExecContext(ctx, `INSERT INTO workspace_cursor (workspace_id, last_seen_commit, recovered, updated_at, reconcile_note)
	    VALUES (?, NULL, 0, ?, ?) ON CONFLICT (workspace_id) DO UPDATE SET reconcile_note = excluded.reconcile_note`,
		workspaceID, timefmt.RFC3339(now), string(note))
	return err
}

// ReconcileNote reads the last note ("" when none).
func (s *Store) ReconcileNote(ctx context.Context, workspaceID string) string {
	var n sql.NullString
	_ = s.db.QueryRowContext(ctx, `SELECT reconcile_note FROM workspace_cursor WHERE workspace_id = ?`, workspaceID).Scan(&n)
	return n.String
}

// Health is the capture-health summary atlas-cli status shows.
type Health struct {
	Sessions, NeedsAttention, OpenTurns, Orphaned int
	Cursor                                        *Cursor
	ReconcileNote                                 string
}

// CaptureHealth reads the summary for a Project.
func (s *Store) CaptureHealth(ctx context.Context, workspaceID string) (Health, error) {
	var h Health
	err := s.db.QueryRowContext(ctx, `SELECT
	    (SELECT COUNT(*) FROM agent_session WHERE workspace_id = ?1),
	    (SELECT COUNT(*) FROM agent_session WHERE workspace_id = ?1 AND needs_attention = 1),
	    (SELECT COUNT(*) FROM turn t JOIN agent_session s ON s.id = t.session_id WHERE s.workspace_id = ?1 AND t.state = 'open'),
	    (SELECT COUNT(*) FROM checkpoint c JOIN agent_session s ON s.id = c.session_id WHERE s.workspace_id = ?1 AND c.link_state = 'orphaned')`,
		workspaceID).Scan(&h.Sessions, &h.NeedsAttention, &h.OpenTurns, &h.Orphaned)
	if err != nil {
		return h, err
	}
	if h.Cursor, err = s.GetCursor(ctx, workspaceID); err != nil {
		return h, err
	}
	h.ReconcileNote = s.ReconcileNote(ctx, workspaceID)
	return h, nil
}
