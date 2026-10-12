package record

import (
	"context"
	"database/sql"
	_ "embed" // schema_v6.sql
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite with FTS5
)

// SchemaVersion is the record version the CLI reads and writes.
const SchemaVersion = 6

// schemaV6 is the desktop's DDL as SQLite stored it after migrations v1–v6,
// so a CLI-created record's sqlite_master matches a desktop-created one.
//
//go:embed schema_v6.sql
var schemaV6 string

// Errors from Open.
var (
	ErrNoRecord     = errors.New("no shared memory here yet")
	ErrSchemaTooNew = errors.New("memory.sqlite was written by a newer Atlas; update the CLI")
	ErrSchemaTooOld = errors.New("memory.sqlite was written by an older Atlas; open the folder in the Atlas app once to upgrade it")
)

// Entry kinds (wire spelling).
const (
	KindPlan         = "plan"
	KindFileChanged  = "file_changed"
	KindDecision     = "decision"
	KindFact         = "fact"
	KindFailure      = "failure"
	KindArchitecture = "architecture"
	KindPreference   = "preference"
)

// Kinds lists every entry kind in the desktop's order.
var Kinds = []string{KindPlan, KindFileChanged, KindDecision, KindFact, KindFailure, KindArchitecture, KindPreference}

// DurableKinds accumulate and are what agents search by default.
var DurableKinds = []string{KindDecision, KindFact, KindFailure, KindArchitecture, KindPreference}

// DisplayCap is each kind's display limit (storage is unbounded).
var DisplayCap = map[string]int{
	KindPlan: 1, KindDecision: 50, KindFileChanged: 50, KindFact: 50,
	KindFailure: 30, KindArchitecture: 30, KindPreference: 30,
}

// IsKind reports a known kind.
func IsKind(k string) bool {
	for _, x := range Kinds {
		if x == k {
			return true
		}
	}
	return false
}

// IsDurable reports a durable kind.
func IsDurable(k string) bool {
	for _, x := range DurableKinds {
		if x == k {
			return true
		}
	}
	return false
}

// States.
const (
	StateActive     = "active"
	StateCandidate  = "candidate"
	StateArchived   = "archived"
	StateTombstoned = "tombstoned"
)

// Confidence thresholds.
const (
	Trusted   = 0.5 // ≥ → active, < → candidate
	Candidate = 0.3 // captured or imported
	Promoted  = 0.7 // vouched for by another session
)

// Store is one open memory.sqlite (one per repository scope).
type Store struct {
	ScopeRoot string
	db        *sql.DB
	opened    os.FileInfo // memory.sqlite as opened
	held      *os.File    // a handle on it: unlinked means .atlas/ went away
}

// Stale reports that memory.sqlite on disk is no longer the file this store
// opened (.atlas/ deleted or re-created): a long-lived holder (the listener,
// an mcp-bridge) must reopen it, or its writes go to a file no one sees.
func (s *Store) Stale() bool {
	now, err := os.Stat(Path(s.ScopeRoot))
	return err != nil || s.opened == nil || unlinked(s.held) || !os.SameFile(s.opened, now)
}

// unlinked: the open file's link count is 0. A re-created file can reuse
// the inode number (ext4 does), so the inode comparison alone misses it.
func unlinked(f *os.File) bool {
	if f == nil {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return true
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && st.Nlink == 0
}

// File names in <scope>/.atlas/memory/, as the desktop names them.
const (
	DBFile       = "memory.sqlite"
	SnapshotFile = "memory.snapshot.sqlite"
	// SnapshotEvery is how old the snapshot may get (health.rs SNAPSHOT_EVERY_MS).
	SnapshotEvery = 24 * time.Hour
)

// Path is <scope root>/.atlas/memory/memory.sqlite.
func Path(scopeRoot string) string {
	return filepath.Join(scopeRoot, ".atlas", "memory", "memory.sqlite")
}

// Open opens the record for a scope root. With create, a missing record is
// created at schema v6. A record below v6 was made by an older desktop and is
// refused rather than overwritten.
func Open(scopeRoot string, create bool) (*Store, error) {
	st, err := openChecked(scopeRoot, create)
	if err == nil || !damaged(err) {
		return st, err
	}
	// A damaged record is quarantined and the last snapshot copied in its
	// place, with a restored.json marker for the app, as the desktop does
	// (crates/atlas-memory/src/record.rs RecordStore::open).
	if qerr := quarantine(scopeRoot, time.Now().UnixMilli()); qerr != nil {
		return nil, fmt.Errorf("%w (and quarantining it failed: %w)", err, qerr)
	}
	return openChecked(scopeRoot, create)
}

// damaged tells a damaged file from a record the CLI declines (wrong schema
// version, not there).
func damaged(err error) bool {
	if errors.Is(err, ErrNoRecord) || errors.Is(err, ErrSchemaTooNew) || errors.Is(err, ErrSchemaTooOld) {
		return false
	}
	m := strings.ToLower(err.Error())
	for _, w := range []string{"malformed", "not a database", "quick_check", "corrupt", "disk image"} {
		if strings.Contains(m, w) {
			return true
		}
	}
	return false
}

func quarantine(scopeRoot string, now int64) error {
	dir := filepath.Dir(Path(scopeRoot))
	for _, suffix := range []string{"", "-wal", "-shm"} {
		from := filepath.Join(dir, DBFile+suffix)
		if _, err := os.Stat(from); err == nil {
			if err := os.Rename(from, filepath.Join(dir, fmt.Sprintf("%s%s.corrupt-%d", DBFile, suffix, now))); err != nil {
				return err
			}
		}
	}
	fromSnapshot := copyFile(filepath.Join(dir, SnapshotFile), filepath.Join(dir, DBFile)) == nil
	b, _ := json.Marshal(map[string]any{"at": now, "from_snapshot": fromSnapshot})
	return os.WriteFile(filepath.Join(dir, "restored.json"), b, 0o644) //nolint:gosec // G306: a marker the app reads
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from) //nolint:gosec // G304: the record's own snapshot
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644) //nolint:gosec // G306: the record, as the app writes it
}

func openChecked(scopeRoot string, create bool) (*Store, error) {
	path := Path(scopeRoot)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, ErrNoRecord
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{ScopeRoot: scopeRoot, db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	var quick string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&quick); err != nil || quick != "ok" {
		_ = db.Close()
		if err == nil {
			err = fmt.Errorf("quick_check: %s", quick)
		}
		return nil, err
	}
	s.opened, _ = os.Stat(path)
	s.held, _ = os.Open(path) //nolint:gosec // G304: the record's own file
	return s, nil
}

func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return err
	}
	switch {
	case v == SchemaVersion:
		return nil
	case v > SchemaVersion:
		return ErrSchemaTooNew
	case v > 0:
		return ErrSchemaTooOld
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return errors.New("memory.sqlite has tables but no schema version; refusing to touch it")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(schemaV6); err != nil {
		return fmt.Errorf("create memory schema: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// Close closes the record.
func (s *Store) Close() error {
	if s.held != nil {
		_ = s.held.Close()
	}
	return s.db.Close()
}

// VerifyChain recomputes the revision hash chain.
func (s *Store) VerifyChain() (ChainReport, error) { return verifyChain(s.db) }

// Entry is one live memory: the current view of its revisions.
type Entry struct {
	ID          int64
	Kind        string
	Key         string
	Content     string
	Status      string
	Source      string
	Agent       string
	Session     string
	Confidence  float64
	CreatedAt   int64
	UpdatedAt   int64
	LastUsedAt  *int64
	Uses        int64
	ContentHash string
	Seq         *int64
	Rev         int64
	State       string
	Scope       string
	Evidence    []Citation
	ChangedBy   string
}

// By names the writer: the agent, else the source.
func (e *Entry) By() string {
	if e.Agent != "" {
		return e.Agent
	}
	return e.Source
}

const entryColumns = `id, kind, key, content, status, source, agent, session, confidence, created_at, updated_at,
	last_used_at, uses, content_hash, seq, rev, state, scope, evidence, changed_by`

type scanner interface{ Scan(dest ...any) error }

func scanEntry(r scanner) (*Entry, error) {
	var e Entry
	var ev string
	if err := r.Scan(&e.ID, &e.Kind, &e.Key, &e.Content, &e.Status, &e.Source, &e.Agent, &e.Session,
		&e.Confidence, &e.CreatedAt, &e.UpdatedAt, &e.LastUsedAt, &e.Uses, &e.ContentHash, &e.Seq,
		&e.Rev, &e.State, &e.Scope, &ev, &e.ChangedBy); err != nil {
		return nil, err
	}
	if ev != "" && ev != "[]" {
		_ = json.Unmarshal([]byte(ev), &e.Evidence)
	}
	return &e, nil
}

// Get reads one entry; nil when there is none.
func (s *Store) Get(ctx context.Context, id int64) (*Entry, error) {
	e, err := scanEntry(s.db.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

func (s *Store) queryEntries(ctx context.Context, q string, args ...any) ([]*Entry, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+entryColumns+` FROM entries `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
