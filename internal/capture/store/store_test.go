package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// The oracle is sqlite_master of a sessions.db written by the desktop app
// (schema v10, captured 2026-10-10). A store the CLI creates must be
// indistinguishable from it.
func TestSchemaMatchesDesktopOracle(t *testing.T) {
	raw, err := os.ReadFile("testdata/desktop_sqlite_master_v10.json")
	if err != nil {
		t.Fatal(err)
	}
	var want [][4]*string
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	s, err := Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, err := s.db.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][4]*string
	for rows.Next() {
		var typ, name, tbl string
		var sqlText sql.NullString
		if err := rows.Scan(&typ, &name, &tbl, &sqlText); err != nil {
			t.Fatal(err)
		}
		r := [4]*string{&typ, &name, &tbl, nil}
		if sqlText.Valid {
			r[3] = &sqlText.String
		}
		got = append(got, r)
	}
	if len(got) != len(want) {
		t.Fatalf("%d objects, want %d", len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("object %d differs:\n got %s\nwant %s", i, show(got[i]), show(want[i]))
		}
	}
	var v int
	_ = s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	var mode string
	_ = s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	var seq int
	_ = s.db.QueryRow(`SELECT value FROM counter WHERE name = 'seq'`).Scan(&seq)
	if v != 10 || mode != "wal" || seq != 0 {
		t.Fatalf("user_version=%d journal_mode=%s seq=%d", v, mode, seq)
	}
}

func show(r [4]*string) string {
	b, _ := json.Marshal(r)
	return string(b)
}

// The columns SPEC §6.2 / evidence-capture.md §3.3 list, in table order: the
// oracle and the evidence must agree.
func TestColumnsMatchEvidence(t *testing.T) {
	s, err := Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := map[string][]string{
		"agent_session": {"id", "workspace_id", "source", "native_session_id", "title", "agent", "model", "cwd",
			"token_totals", "summary", "started_at", "updated_at", "needs_attention", "attention_reason",
			"redaction_counts", "sync_state", "sync_attempts", "branch", "last_activity_at"},
		"binding": {"id", "workspace_id", "root", "mode", "slug", "org_id", "root_commit_sha", "fingerprint_is_shallow",
			"git_url", "enabled", "created_at", "updated_at", "import_approved", "drain_state", "remote_workspace_id"},
		"file_touch": {"id", "tool_call_id", "session_id", "turn_seq", "seq", "path", "sha256_after", "existed_before",
			"deleted", "out_of_repo", "created_at", "consumed_by_commit", "sketch_after"},
		"usage_delta": {"session_id", "turn_seq", "model", "recorded_at", "input_tokens", "output_tokens",
			"cache_creation_tokens", "cache_read_tokens", "reasoning_tokens"},
	}
	for table, cols := range want {
		rows, err := s.db.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			got = append(got, n)
		}
		rows.Close()
		if !reflect.DeepEqual(got, cols) {
			t.Errorf("%s columns\n got %v\nwant %v", table, got, cols)
		}
	}
}

func TestOpenWritesGitignore(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	b, err := os.ReadFile(filepath.Join(root, ".atlas", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# Atlas per-project state. Never committed: it holds the local session\n" +
		"# store (plus its SQLite -wal/-shm sidecars) and is machine-specific.\n*\n"
	if string(b) != want {
		t.Fatalf("got %q", b)
	}
	// An existing .gitignore is left alone.
	_ = os.WriteFile(filepath.Join(root, ".atlas", ".gitignore"), []byte("custom\n"), 0o644)
	s, _ = Open(root, true)
	s.Close()
	if b, _ := os.ReadFile(filepath.Join(root, ".atlas", ".gitignore")); string(b) != "custom\n" {
		t.Fatalf("overwrote: %q", b)
	}
}

func TestOpenWithoutCreate(t *testing.T) {
	if _, err := Open(t.TempDir(), false); !errors.Is(err, ErrNoStore) {
		t.Fatalf("err = %v", err)
	}
}

func TestVersionGuards(t *testing.T) {
	for _, tc := range []struct {
		version int
		want    error
	}{{11, ErrSchemaTooNew}, {7, ErrSchemaTooOld}} {
		root := t.TempDir()
		s, err := Open(root, true)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`PRAGMA user_version = ` + itoa(tc.version)); err != nil {
			t.Fatal(err)
		}
		s.Close()
		if _, err := Open(root, true); !errors.Is(err, tc.want) {
			t.Errorf("user_version %d: err = %v", tc.version, err)
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestWriterLockIsExclusive(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root, true)
	s.Close()
	l1, err := Lock(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(root); !errors.Is(err, ErrLocked) {
		t.Fatalf("second lock: %v", err)
	}
	if err := l1.Release(); err != nil {
		t.Fatal(err)
	}
	l2, err := Lock(root)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = l2.Release()
}

func TestBindingUpsert(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root, true)
	defer s.Close()
	ctx := context.Background()
	if b, err := s.Binding(ctx); err != nil || b != nil {
		t.Fatalf("empty store: %+v %v", b, err)
	}
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	sha := "abc123"
	in := Binding{WorkspaceID: "/w", Root: "/w", Mode: ModeLocal, RootCommitSHA: &sha, Enabled: true, ImportApproved: true, DrainState: DrainOK}
	if err := s.PutBinding(ctx, in, t0); err != nil {
		t.Fatal(err)
	}
	org, slug, remote := "org_1", "backend", "ws_1"
	in.Mode, in.OrgID, in.Slug, in.RemoteWorkspaceID, in.ImportApproved = ModeCloud, &org, &slug, &remote, false
	if err := s.PutBinding(ctx, in, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.Binding(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != ModeCloud || *got.OrgID != "org_1" || *got.RemoteWorkspaceID != "ws_1" || got.ImportApproved {
		t.Fatalf("%+v", got)
	}
	if got.CreatedAt != "2026-10-10T12:00:00+00:00" || got.UpdatedAt != "2026-10-10T13:00:00+00:00" {
		t.Fatalf("timestamps %s / %s", got.CreatedAt, got.UpdatedAt)
	}
	if err := s.SetEnabled(ctx, false, t0); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Binding(ctx); got.Enabled {
		t.Fatal("still enabled")
	}
}

func TestBindCloudPromotesAndRequeues(t *testing.T) {
	root := t.TempDir()
	s, _ := Open(root, true)
	defer s.Close()
	ctx := context.Background()
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	_ = s.PutBinding(ctx, Binding{WorkspaceID: "/w", Root: "/w", Mode: ModeLocal, Enabled: true, ImportApproved: true}, now)
	for i, st := range []string{"local", "sent", "failed"} {
		id := "as-" + itoa(i)
		if _, err := s.db.Exec(`INSERT INTO agent_session (id, workspace_id, source, native_session_id, started_at, updated_at, sync_state, sync_attempts)
		    VALUES (?, '/w', 'acp', ?, 'x', 'x', ?, 3)`, id, id, st); err != nil {
			t.Fatal(err)
		}
	}
	states := func() []string {
		rows, _ := s.db.Query(`SELECT sync_state || ':' || sync_attempts FROM agent_session ORDER BY id`)
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			_ = rows.Scan(&v)
			out = append(out, v)
		}
		return out
	}
	org, ws1, ws2 := "org_1", "ws_1", "ws_2"
	if err := s.BindCloud(ctx, Binding{WorkspaceID: "/w", Root: "/w", OrgID: &org, RemoteWorkspaceID: &ws1, Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	if got := states(); !reflect.DeepEqual(got, []string{"pending:3", "sent:3", "failed:3"}) {
		t.Fatalf("promote: %v", got)
	}
	b, _ := s.Binding(ctx)
	if b.Mode != ModeCloud || b.ImportApproved || b.CreatedAt != "2026-10-10T12:00:00+00:00" {
		t.Fatalf("%+v", b)
	}
	if err := s.BindCloud(ctx, Binding{WorkspaceID: "/w", Root: "/w", OrgID: &org, RemoteWorkspaceID: &ws2, Enabled: true}, now); err != nil {
		t.Fatal(err)
	}
	if got := states(); !reflect.DeepEqual(got, []string{"pending:0", "pending:0", "pending:0"}) {
		t.Fatalf("switch: %v", got)
	}
}
