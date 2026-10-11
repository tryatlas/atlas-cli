package mcpconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func TestOpenCodeWriter(t *testing.T) {
	root := t.TempDir()
	w, ok := For(opencodeID)
	if !ok {
		t.Fatal("no opencode writer")
	}
	path := filepath.Join(root, "opencode.json")
	if w.Path(root) != path {
		t.Fatal(w.Path(root))
	}
	if err := os.WriteFile(path, []byte(`{
  "$schema": "https://opencode.ai/config.json",
  "model": "google/gemini-3.7-flash",
  "mcp": {"other": {"type": "remote", "url": "https://x", "enabled": false}}
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if w.State(root) != hooks.Absent {
		t.Fatal("state before write")
	}
	changed, err := w.Write(root)
	if err != nil || !changed {
		t.Fatalf("write: %v %v", changed, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Schema string `json:"$schema"`
		Model  string `json:"model"`
		MCP    map[string]struct {
			Type    string   `json:"type"`
			Command []string `json:"command"`
			Enabled bool     `json:"enabled"`
			URL     string   `json:"url"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Schema != "https://opencode.ai/config.json" || cfg.Model != "google/gemini-3.7-flash" {
		t.Fatalf("other keys lost: %s", b)
	}
	if cfg.MCP["other"].URL != "https://x" {
		t.Fatalf("other server lost: %s", b)
	}
	e := cfg.MCP[ServerName]
	want := []string{"atlas", "mcp-bridge", "--project", root, "--agent", "opencode"}
	if e.Type != "local" || !e.Enabled || len(e.Command) != len(want) {
		t.Fatalf("entry %+v", e)
	}
	for i := range want {
		if e.Command[i] != want[i] {
			t.Fatalf("command %v", e.Command)
		}
	}
	if w.State(root) != hooks.Current {
		t.Fatal("state after write")
	}
	if changed, err := w.Write(root); err != nil || changed {
		t.Fatalf("second write: %v %v", changed, err)
	}

	// Another root's entry is outdated and refreshed.
	stale := []byte(`{"mcp":{"atlas_memory":{"type":"local","command":["atlas","mcp-bridge","--project","/elsewhere","--agent","opencode"],"enabled":true}}}`)
	if err := os.WriteFile(path, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	if w.State(root) != hooks.Outdated {
		t.Fatal("state with another root")
	}
	if changed, _ := w.Write(root); !changed || w.State(root) != hooks.Current {
		t.Fatal("stale entry not refreshed")
	}

	// Remove keeps the other server, and drops the key when it was alone.
	if err := os.WriteFile(path, []byte(`{"mcp":{"other":{"type":"remote","url":"https://x"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(root); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	obj, err := hooks.LoadJSONObject(path)
	if err != nil {
		t.Fatal(err)
	}
	var servers map[string]json.RawMessage
	if json.Unmarshal(obj["mcp"], &servers) != nil || len(servers) != 1 || servers["other"] == nil {
		t.Fatalf("after remove: %s", obj["mcp"])
	}
	if err := os.WriteFile(path, []byte(`{"$schema":"s"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(root); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	obj, _ = hooks.LoadJSONObject(path)
	if _, ok := obj["mcp"]; ok || string(obj["$schema"]) != `"s"` {
		t.Fatalf("after removing the last server: %v", obj)
	}

	// No file: Write creates it; Remove on a missing file is fine.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	if changed, err := w.Write(root); err != nil || !changed || w.State(root) != hooks.Current {
		t.Fatalf("fresh write: %v %v", changed, err)
	}
}
