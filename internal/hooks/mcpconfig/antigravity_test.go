package mcpconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tryatlas/cli/internal/hooks"
)

func TestAntigravityWriter(t *testing.T) {
	w, ok := For(antigravityID)
	if !ok {
		t.Fatal("no writer registered for antigravity")
	}
	root := t.TempDir()
	path := filepath.Join(root, ".agents", "mcp_config.json")
	if got := w.Path(root); got != path {
		t.Fatalf("Path = %q, want %q", got, path)
	}
	if w.State(root) != hooks.Absent {
		t.Fatalf("State before write = %v, want absent", w.State(root))
	}

	// A user's own server survives and ours lands beside it, with no type.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"arm":{"command":"docker","args":["run"],"env":{}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := w.Write(root)
	if err != nil || !changed {
		t.Fatalf("Write = %v, %v; want changed", changed, err)
	}
	var file struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	b, _ := os.ReadFile(path)
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	if _, ok := file.Servers["arm"]; !ok {
		t.Fatalf("user's server lost: %s", b)
	}
	ours := file.Servers[ServerName]
	if ours["command"] != "atlas-cli" || ours["type"] != nil {
		t.Fatalf("entry = %v", ours)
	}
	args, _ := json.Marshal(ours["args"])
	want, _ := json.Marshal(Args(root, antigravityID))
	if string(args) != string(want) {
		t.Fatalf("args = %s, want %s", args, want)
	}
	if w.State(root) != hooks.Current {
		t.Fatalf("State after write = %v, want current", w.State(root))
	}
	if changed, err := w.Write(root); err != nil || changed {
		t.Fatalf("second Write = %v, %v; want unchanged", changed, err)
	}

	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	var after struct {
		Servers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &after); err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Servers[ServerName]; ok {
		t.Fatalf("entry not removed: %s", b)
	}
	if _, ok := after.Servers["arm"]; !ok {
		t.Fatalf("user's server lost on remove: %s", b)
	}
	if w.State(root) != hooks.Absent {
		t.Fatalf("State after remove = %v, want absent", w.State(root))
	}
}
