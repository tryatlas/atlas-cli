package record

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// SnapshotIfDue refreshes memory.snapshot.sqlite when the last one is a day
// old or missing, as the desktop's health pass does (crates/atlas-memory/
// src/health.rs snapshot_if_due): VACUUM INTO a temp file, stamp it with a
// snapshot_meta 'at' row (the age is read from there, not the mtime), then
// rename it into place. A damaged record is restored from it on open.
func (s *Store) SnapshotIfDue(now int64) (bool, error) {
	dir := filepath.Dir(Path(s.ScopeRoot))
	path := filepath.Join(dir, SnapshotFile)
	if at, ok := snapshotAt(path); ok && now-at < SnapshotEvery.Milliseconds() {
		return false, nil
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if _, err := s.db.Exec(`VACUUM INTO ?`, tmp); err != nil {
		return false, err
	}
	snap, err := sql.Open("sqlite", "file:"+tmp)
	if err != nil {
		return false, err
	}
	_, err = snap.Exec(`CREATE TABLE IF NOT EXISTS snapshot_meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`)
	if err == nil {
		_, err = snap.Exec(`INSERT OR REPLACE INTO snapshot_meta (k, v) VALUES ('at', ?)`, strconv.FormatInt(now, 10))
	}
	if cerr := snap.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return false, fmt.Errorf("stamp snapshot: %w", err)
	}
	return true, os.Rename(tmp, path)
}

// snapshotAt reads the snapshot's 'at' stamp.
func snapshotAt(path string) (int64, bool) {
	if _, err := os.Stat(path); err != nil {
		return 0, false
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return 0, false
	}
	defer db.Close()
	var v string
	if db.QueryRow(`SELECT v FROM snapshot_meta WHERE k = 'at'`).Scan(&v) != nil {
		return 0, false
	}
	at, err := strconv.ParseInt(v, 10, 64)
	return at, err == nil
}
