package mcpconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func TestCopilotWriterIsRegisteredForTheRepositoryFile(t *testing.T) {
	w, ok := For("copilot")
	if !ok {
		t.Fatal("no writer for copilot")
	}
	if got := w.Path("/p"); got != "/p/.github/mcp.json" {
		t.Fatalf("path %q", got)
	}
}

func TestCopilotWriterWritesALocalServerAndKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".github", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"github":{"type":"http","url":"https://api.githubcopilot.com/mcp/"}},"other":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := For("copilot")
	if w.State(root) != hooks.Absent {
		t.Fatal("expected absent")
	}
	changed, err := w.Write(root)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(path)
	var file struct {
		Servers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Tools   []string `json:"tools"`
		} `json:"mcpServers"`
		Other int `json:"other"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	e, ok := file.Servers[ServerName]
	if !ok || e.Type != "local" || e.Command != "atlas-cli" || len(e.Tools) != 1 || e.Tools[0] != "*" {
		t.Fatalf("%s", b)
	}
	if want := []string{"mcp-bridge", "--project", root, "--agent", "copilot"}; len(e.Args) != len(want) || e.Args[2] != root || e.Args[4] != "copilot" {
		t.Fatalf("args %v", e.Args)
	}
	if _, ok := file.Servers["github"]; !ok || file.Other != 1 {
		t.Fatalf("other content lost: %s", b)
	}
	if w.State(root) != hooks.Current {
		t.Fatal("expected current")
	}
	if changed, err := w.Write(root); err != nil || changed {
		t.Fatalf("second write changed=%v err=%v", changed, err)
	}
	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	file.Servers = nil // a decode into a live map would keep the old keys
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	if _, ok := file.Servers[ServerName]; ok || len(file.Servers) != 1 || file.Other != 1 {
		t.Fatalf("after remove: %s", b)
	}
	if w.State(root) != hooks.Absent {
		t.Fatal("expected absent after remove")
	}
}
