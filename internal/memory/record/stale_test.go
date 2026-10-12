package record

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStaleAfterTheRecordIsRecreated(t *testing.T) {
	root := t.TempDir()
	st, err := Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.Stale() {
		t.Fatal("fresh store is not stale")
	}
	if err := os.RemoveAll(filepath.Join(root, ".atlas")); err != nil {
		t.Fatal(err)
	}
	if !st.Stale() {
		t.Fatal("a deleted record must be stale")
	}
	again, err := Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if !st.Stale() || again.Stale() {
		t.Fatal("the old handle is stale, the new one is not")
	}
}
