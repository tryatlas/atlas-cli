package mcpconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func TestDroidWriter(t *testing.T) {
	w, ok := For(droidID)
	if !ok {
		t.Fatal("droid writer not registered")
	}
	root := t.TempDir()
	path := w.Path(root)
	if path != root+"/.factory/mcp.json" {
		t.Fatalf("path: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"mcpServers":{"linear":{"type":"http","url":"https://mcp.linear.app/mcp"}}}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	if w.State(root) != hooks.Absent {
		t.Fatalf("state before: %v", w.State(root))
	}
	changed, err := w.Write(root)
	if err != nil || !changed {
		t.Fatalf("write: %v %v", changed, err)
	}
	obj, err := hooks.LoadJSONObject(path)
	if err != nil {
		t.Fatal(err)
	}
	var servers map[string]map[string]any
	if err := json.Unmarshal(obj["mcpServers"], &servers); err != nil {
		t.Fatal(err)
	}
	if servers["linear"]["url"] != "https://mcp.linear.app/mcp" {
		t.Fatalf("user server lost: %v", servers)
	}
	atlas := servers[ServerName]
	if atlas["type"] != "stdio" || atlas["command"] != "atlas" {
		t.Fatalf("entry: %v", atlas)
	}
	args, _ := json.Marshal(atlas["args"])
	want, _ := json.Marshal(Args(root, droidID))
	if string(args) != string(want) {
		t.Fatalf("args %s, want %s", args, want)
	}
	if w.State(root) != hooks.Current {
		t.Fatalf("state after write: %v", w.State(root))
	}
	if changed, err := w.Write(root); err != nil || changed {
		t.Fatalf("second write: %v %v", changed, err)
	}
	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	obj, _ = hooks.LoadJSONObject(path)
	servers = nil
	_ = json.Unmarshal(obj["mcpServers"], &servers)
	if _, ok := servers[ServerName]; ok || servers["linear"] == nil {
		t.Fatalf("remove: %v", servers)
	}
	if w.State(root) != hooks.Absent {
		t.Fatalf("state after remove: %v", w.State(root))
	}
}

func TestDroidWriterCreatesFile(t *testing.T) {
	root := t.TempDir()
	w, _ := For(droidID)
	if changed, err := w.Write(root); err != nil || !changed {
		t.Fatalf("write: %v %v", changed, err)
	}
	if w.State(root) != hooks.Current {
		t.Fatalf("state: %v", w.State(root))
	}
}
