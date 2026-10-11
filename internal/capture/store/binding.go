package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/tryatlas/atlas-cli/internal/timefmt"
)

// Binding modes and drain states (SPEC §6.3).
const (
	ModeLocal = "local"
	ModeCloud = "cloud"

	DrainOK            = "ok"
	DrainNotAuthorized = "not_authorized"
)

// Binding is the singleton binding row (id = 1): how this Project is bound.
type Binding struct {
	WorkspaceID          string // canonical launch dir
	Root                 string // same string
	Mode                 string
	Slug                 *string
	OrgID                *string
	RootCommitSHA        *string
	FingerprintIsShallow bool
	GitURL               *string
	Enabled              bool
	CreatedAt            string
	UpdatedAt            string
	ImportApproved       bool
	DrainState           string
	RemoteWorkspaceID    *string
}

// Binding reads the row; nil when the Project has never been bound.
func (s *Store) Binding(ctx context.Context) (*Binding, error) {
	var b Binding
	err := s.db.QueryRowContext(ctx, `SELECT workspace_id, root, mode, slug, org_id, root_commit_sha,
	        fingerprint_is_shallow, git_url, enabled, created_at, updated_at, import_approved,
	        drain_state, remote_workspace_id FROM binding WHERE id = 1`).Scan(
		&b.WorkspaceID, &b.Root, &b.Mode, &b.Slug, &b.OrgID, &b.RootCommitSHA,
		&b.FingerprintIsShallow, &b.GitURL, &b.Enabled, &b.CreatedAt, &b.UpdatedAt, &b.ImportApproved,
		&b.DrainState, &b.RemoteWorkspaceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// PutBinding writes the row, keeping created_at when it exists. The caller
// must hold the writer lock.
func (s *Store) PutBinding(ctx context.Context, b Binding, now time.Time) error {
	if b.DrainState == "" {
		b.DrainState = DrainOK
	}
	ts := timefmt.RFC3339(now)
	_, err := s.db.ExecContext(ctx, `INSERT INTO binding (id, workspace_id, root, mode, slug, org_id,
	        root_commit_sha, fingerprint_is_shallow, git_url, enabled, created_at, updated_at,
	        import_approved, drain_state, remote_workspace_id)
	    VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	    ON CONFLICT (id) DO UPDATE SET workspace_id = excluded.workspace_id, root = excluded.root,
	        mode = excluded.mode, slug = excluded.slug, org_id = excluded.org_id,
	        root_commit_sha = excluded.root_commit_sha, fingerprint_is_shallow = excluded.fingerprint_is_shallow,
	        git_url = excluded.git_url, enabled = excluded.enabled, updated_at = excluded.updated_at,
	        import_approved = excluded.import_approved, drain_state = excluded.drain_state,
	        remote_workspace_id = excluded.remote_workspace_id`,
		b.WorkspaceID, b.Root, b.Mode, b.Slug, b.OrgID, b.RootCommitSHA, b.FingerprintIsShallow, b.GitURL,
		b.Enabled, ts, ts, b.ImportApproved, b.DrainState, b.RemoteWorkspaceID)
	return err
}

// SetEnabled flips the capture gate (binding.enabled). Pausing stops new
// records; what was recorded still drains.
func (s *Store) SetEnabled(ctx context.Context, enabled bool, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE binding SET enabled = ?, updated_at = ? WHERE id = 1`,
		enabled, timefmt.RFC3339(now))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("this Project is not bound; run atlas init")
	}
	return nil
}

// syncTables are the rows that drain to the ingest server.
var syncTables = []string{"agent_session", "agent_message", "tool_call", "checkpoint"}

// BindCloud writes a cloud binding in one transaction. Moving from local to
// cloud promotes every local row to pending; moving to a different cloud
// Project requeues every row (the desktop's promote_to_cloud and
// switch_cloud_project).
func (s *Store) BindCloud(ctx context.Context, b Binding, now time.Time) error {
	prev, err := s.Binding(ctx)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	ts := timefmt.RFC3339(now)
	b.Mode = ModeCloud
	if b.DrainState == "" {
		b.DrainState = DrainOK
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO binding (id, workspace_id, root, mode, slug, org_id,
	        root_commit_sha, fingerprint_is_shallow, git_url, enabled, created_at, updated_at,
	        import_approved, drain_state, remote_workspace_id)
	    VALUES (1, ?, ?, 'cloud', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	    ON CONFLICT (id) DO UPDATE SET workspace_id = excluded.workspace_id, root = excluded.root,
	        mode = 'cloud', slug = excluded.slug, org_id = excluded.org_id,
	        root_commit_sha = excluded.root_commit_sha, fingerprint_is_shallow = excluded.fingerprint_is_shallow,
	        git_url = excluded.git_url, enabled = excluded.enabled, updated_at = excluded.updated_at,
	        import_approved = excluded.import_approved, drain_state = excluded.drain_state,
	        remote_workspace_id = excluded.remote_workspace_id`,
		b.WorkspaceID, b.Root, b.Slug, b.OrgID, b.RootCommitSHA, b.FingerprintIsShallow, b.GitURL,
		b.Enabled, ts, ts, b.ImportApproved, b.DrainState, b.RemoteWorkspaceID); err != nil {
		return err
	}
	switched := prev != nil && prev.Mode == ModeCloud && !samePtr(prev.RemoteWorkspaceID, b.RemoteWorkspaceID)
	for _, t := range syncTables {
		q := `UPDATE ` + t + ` SET sync_state = 'pending' WHERE sync_state = 'local'` //nolint:gosec // G202: t is from the fixed syncTables list
		if switched {
			q = `UPDATE ` + t + ` SET sync_state = 'pending', sync_attempts = 0`
		}
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
