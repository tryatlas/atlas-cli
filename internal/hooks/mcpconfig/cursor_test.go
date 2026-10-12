package mcpconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func TestCursorWriter(t *testing.T) {
	root := t.TempDir()
	w, ok := For("cursor")
	if !ok {
		t.Fatal("no cursor writer")
	}
	if w.Path(root) != root+"/.cursor/mcp.json" {
		t.Fatal(w.Path(root))
	}
	if err := os.MkdirAll(filepath.Join(root, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.Path(root), []byte(`{"mcpServers":{"other":{"url":"http://x"}},"extra":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if w.State(root) != hooks.Absent {
		t.Fatal("state before write")
	}
	changed, err := w.Write(root)
	if err != nil || !changed {
		t.Fatalf("write %v %v", changed, err)
	}
	b, _ := os.ReadFile(w.Path(root))
	var obj struct {
		Extra   int `json:"extra"`
		Servers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
			URL     string   `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	e := obj.Servers[ServerName]
	if e.Command != "atlas-cli" || e.Type != "" || len(e.Args) != 5 || e.Args[0] != "mcp-bridge" || e.Args[2] != root || e.Args[4] != "cursor" {
		t.Fatalf("entry %+v", e)
	}
	if obj.Servers["other"].URL != "http://x" || obj.Extra != 1 {
		t.Fatalf("other content lost: %s", b)
	}
	if w.State(root) != hooks.Current {
		t.Fatal("state after write")
	}
	if changed, err := w.Write(root); err != nil || changed {
		t.Fatalf("second write %v %v", changed, err)
	}
	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(w.Path(root))
	var after map[string]json.RawMessage
	_ = json.Unmarshal(b, &after)
	if string(after["mcpServers"]) == "" || string(after["extra"]) != "1" {
		t.Fatalf("after remove %s", b)
	}
	var servers map[string]json.RawMessage
	_ = json.Unmarshal(after["mcpServers"], &servers)
	if _, ok := servers[ServerName]; ok || len(servers) != 1 {
		t.Fatalf("after remove %s", b)
	}
	if w.State(root) != hooks.Absent {
		t.Fatal("state after remove")
	}
}
