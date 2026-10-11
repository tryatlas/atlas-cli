package record

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestOracleChains verifies chains written by the desktop app. Point
// ATLAS_ORACLE_MEMORY at one or more copies of desktop memory.sqlite files
// (colon-separated). Skipped otherwise: the fixtures hold real memories and
// are not committed.
func TestOracleChains(t *testing.T) {
	paths := os.Getenv("ATLAS_ORACLE_MEMORY")
	if paths == "" {
		t.Skip("ATLAS_ORACLE_MEMORY not set")
	}
	for _, p := range strings.Split(paths, ":") {
		db, err := sql.Open("sqlite", "file:"+p+"?mode=ro")
		if err != nil {
			t.Fatal(err)
		}
		rep, err := verifyChain(db)
		_ = db.Close()
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if !rep.OK() || rep.Revisions == 0 {
			t.Errorf("%s: %+v", p, rep)
		} else {
			t.Logf("%s: %d revisions, chain intact", p, rep.Revisions)
		}
	}
}

// TestOracleRoundTrip opens a copy of each desktop record, runs every read,
// and checks that no revision was added and the chain still verifies; then
// writes one memory and checks the CLI's revision extends the desktop chain.
func TestOracleRoundTrip(t *testing.T) {
	paths := os.Getenv("ATLAS_ORACLE_MEMORY")
	if paths == "" {
		t.Skip("ATLAS_ORACLE_MEMORY not set")
	}
	for _, p := range strings.Split(paths, ":") {
		root := t.TempDir()
		dst := Path(root)
		_ = os.MkdirAll(filepath.Dir(dst), 0o755)
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if b, err := os.ReadFile(p + suffix); err == nil {
				_ = os.WriteFile(dst+suffix, b, 0o600)
			}
		}
		s, err := Open(root, false)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		before := count(t, s, `SELECT count(*) FROM revisions`)
		es, _ := s.List(ctx, "", 200)
		var idsList []int64
		for _, e := range es {
			idsList = append(idsList, e.ID)
			_, _ = s.History(ctx, e.ID)
		}
		_, _ = s.Sources(ctx, idsList)
		_, _ = s.Search(ctx, "the project uses", nil, 10)
		_, _ = s.ChangesSince(ctx, 0, "cli-session")
		_ = s.TouchUsed(ctx, idsList, 1)
		if after := count(t, s, `SELECT count(*) FROM revisions`); after != before {
			t.Fatalf("%s: reads added revisions (%d → %d)", p, before, after)
		}
		rep := mustChain(t, s)
		t.Logf("%s: %d entries read, %d revisions, chain intact", p, len(es), rep.Revisions)
		if _, _, err := s.Remember(ctx, agentWrite(KindFact, "", "The CLI can extend a desktop chain", "cli-session", 1)); err != nil {
			t.Fatal(err)
		}
		if rep := mustChain(t, s); rep.Revisions != before+1 {
			t.Fatalf("revisions %d, want %d", rep.Revisions, before+1)
		}
		_ = s.Close()
	}
}
