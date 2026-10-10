package store

import (
	"context"
	"database/sql"
	_ "embed" // schema_v10.sql
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pure-Go SQLite driver "sqlite"
)

// SchemaVersion is the capture store version the CLI writes and reads.
const SchemaVersion = 10

// DirName is the per-project dot-dir of the shipped app (identifier dev.atlas.ide).
const DirName = ".atlas"

// gitignore is written into a new .atlas/ so the directory ignores itself
// (the desktop's exact text, store.rs:2861-2872).
const gitignore = "# Atlas per-project state. Never committed: it holds the local session\n" +
	"# store (plus its SQLite -wal/-shm sidecars) and is machine-specific.\n*\n"

// schemaV10 is the desktop's DDL exactly as SQLite stored it after migrations
// V1–V10 (comments and ALTER-added columns included), so sqlite_master of a
// CLI-created store matches a desktop-created one byte for byte.
//
//go:embed schema_v10.sql
var schemaV10 string

// Errors from Open and Lock.
var (
	ErrNoStore      = errors.New("no capture store here (.atlas/sessions.db)")
	ErrSchemaTooNew = errors.New("sessions.db was written by a newer Atlas; update the CLI")
	ErrSchemaTooOld = errors.New("sessions.db was written by an older Atlas; open the folder in the Atlas app once to upgrade it")
	ErrLocked       = errors.New("another Atlas window is recording this Project")
)

// Store is an open sessions.db.
type Store struct {
	Root string // the canonical launch dir
	db   *sql.DB
}

// Path is <root>/.atlas/sessions.db.
func Path(root string) string { return filepath.Join(root, DirName, "sessions.db") }

// Exists reports whether a capture store has ever been created in root: the
// desktop's "has capture ever been enabled here" test.
func Exists(root string) bool {
	_, err := os.Stat(Path(root))
	return err == nil
}

// dsn builds a modernc URI with the desktop's connection settings.
func dsn(path string, busyMS int, extra ...string) string {
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(path)
	q := []string{fmt.Sprintf("_pragma=busy_timeout(%d)", busyMS)}
	return "file:" + esc + "?" + strings.Join(append(q, extra...), "&")
}

// Open opens <root>/.atlas/sessions.db. With create, a missing store is
// created (schema v10) along with .atlas/.gitignore.
func Open(root string, create bool) (*Store, error) {
	path := Path(root)
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, ErrNoStore
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
	}
	if err := ensureGitignore(filepath.Dir(path)); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(path, 5000,
		"_pragma=journal_mode(WAL)", "_pragma=synchronous(NORMAL)", "_pragma=foreign_keys(1)", "_txlock=immediate"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{Root: root, db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate creates a fresh store or checks an existing one's version.
func (s *Store) migrate() error {
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
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
		return fmt.Errorf("sessions.db has tables but no schema version; refusing to touch it")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(schemaV10); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO counter (name, value) VALUES ('seq', 0)`); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

func ensureGitignore(dir string) error {
	p := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	return os.WriteFile(p, []byte(gitignore), 0o644) //nolint:gosec // G306: a .gitignore, not a secret
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle to the recorder in this module.
func (s *Store) DB() *sql.DB { return s.db }

// ── Writer lock ──────────────────────────────────────────────────────────────

// WriterLock is the desktop's writer lock: .atlas/sessions.lock, a SQLite
// file held in BEGIN EXCLUSIVE for the writer's life. While the CLI holds it a
// desktop window on the folder attaches read-only.
type WriterLock struct {
	db   *sql.DB
	conn *sql.Conn
}

// Lock takes the writer lock without waiting. ErrLocked means another writer
// (usually the desktop) has it.
func Lock(root string) (*WriterLock, error) {
	dir := filepath.Join(root, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, "sessions.lock"), 0))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN EXCLUSIVE`); err != nil {
		_ = conn.Close()
		_ = db.Close()
		if isBusy(err) {
			return nil, ErrLocked
		}
		return nil, err
	}
	return &WriterLock{db: db, conn: conn}, nil
}

// Release ends the transaction and closes the lock file.
func (l *WriterLock) Release() error {
	_, _ = l.conn.ExecContext(context.Background(), `ROLLBACK`)
	_ = l.conn.Close()
	return l.db.Close()
}

// isBusy reports SQLITE_BUSY or SQLITE_LOCKED (any extended code).
func isBusy(err error) bool {
	var coder interface{ Code() int }
	if errors.As(err, &coder) {
		c := coder.Code() & 0xff
		return c == 5 || c == 6
	}
	return strings.Contains(err.Error(), "database is locked") || strings.Contains(err.Error(), "SQLITE_BUSY")
}
