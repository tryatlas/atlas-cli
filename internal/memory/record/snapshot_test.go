package record

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSnapshotAndRestore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, _, err := st.Remember(ctx, Write{Kind: "decision", Content: "Keep the record restorable", Source: "agent", Agent: "pi", Session: "s1", Confidence: 0.9, At: now}); err != nil {
		t.Fatal(err)
	}
	if did, err := st.SnapshotIfDue(now); err != nil || !did {
		t.Fatalf("first snapshot: %v %v", did, err)
	}
	if did, _ := st.SnapshotIfDue(now + time.Hour.Milliseconds()); did {
		t.Fatal("a snapshot is due once a day")
	}
	if at, ok := snapshotAt(filepath.Join(root, ".atlas", "memory", SnapshotFile)); !ok || at != now {
		t.Fatalf("stamp = %d %v", at, ok)
	}
	_ = st.Close()

	// Damage the record: the next open quarantines it and restores the snapshot.
	dir := filepath.Join(root, ".atlas", "memory")
	for _, f := range []string{"-wal", "-shm"} {
		_ = os.Remove(filepath.Join(dir, DBFile+f))
	}
	if err := os.WriteFile(filepath.Join(dir, DBFile), []byte("this is not a database, not at all, definitely not one"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = Open(root, true)
	if err != nil {
		t.Fatalf("open after damage: %v", err)
	}
	defer st.Close()
	es, _ := st.Active(ctx, "decision")
	if len(es) != 1 || es[0].Content != "Keep the record restorable" {
		t.Fatalf("restored = %+v", es)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "restored.json")); err != nil || string(b) == "" {
		t.Fatal("no restored.json marker")
	}
	if m, _ := filepath.Glob(filepath.Join(dir, DBFile+".corrupt-*")); len(m) != 1 {
		t.Fatalf("quarantined = %v", m)
	}
}

func TestAnOldSchemaIsNotQuarantined(t *testing.T) {
	if damaged(ErrSchemaTooOld) || damaged(ErrNoRecord) {
		t.Fatal("a declined record is not damaged")
	}
}
