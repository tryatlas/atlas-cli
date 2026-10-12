package run

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

func TestHostStaleWhenTheStoreIsReplaced(t *testing.T) {
	root := t.TempDir()
	st, err := store.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	fi, _ := os.Stat(store.Path(root))
	held, _ := os.Open(store.Path(root))
	defer held.Close()
	h := &Host{Root: root, opened: fi, held: held}
	if h.Stale() {
		t.Fatal("same store")
	}
	_ = os.RemoveAll(filepath.Join(root, ".atlas"))
	if !h.Stale() {
		t.Fatal("deleted store")
	}
	st, _ = store.Open(root, true)
	_ = st.Close()
	if !h.Stale() {
		t.Fatal("re-created store")
	}
	if !(&Host{Root: root}).Stale() {
		t.Fatal("a store that appeared after start")
	}
}
