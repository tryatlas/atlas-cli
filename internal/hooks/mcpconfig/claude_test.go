package mcpconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/cli/internal/hooks"
)

func TestClaudeWriterAddsAndRemoves(t *testing.T) {
	root := t.TempDir()
	w, ok := For("claude-code")
	if !ok {
		t.Fatal("no claude writer")
	}
	_ = os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"mcpServers":{"other":{"command":"x"}}}`), 0o644)
	if w.State(root) != hooks.Absent {
		t.Fatal("state before")
	}
	changed, err := w.Write(root)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	b, _ := os.ReadFile(filepath.Join(root, ".mcp.json"))
	var obj struct {
		Servers map[string]struct {
			Type, Command string
			Args          []string
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	e := obj.Servers[ServerName]
	if e.Type != "stdio" || e.Command != "atlas-cli" || strings.Join(e.Args, " ") != "mcp-bridge --project "+root+" --agent claude-code" || obj.Servers["other"].Command != "x" {
		t.Fatalf("%+v", obj)
	}
	local, _ := os.ReadFile(filepath.Join(root, ".claude", "settings.local.json"))
	if !strings.Contains(string(local), `"atlas_memory"`) {
		t.Fatalf("not approved: %s", local)
	}
	if w.State(root) != hooks.Current {
		t.Fatal("state after")
	}
	if changed, _ := w.Write(root); changed {
		t.Fatal("second write changed something")
	}
	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(root, ".mcp.json"))
	if strings.Contains(string(b), ServerName) || !strings.Contains(string(b), `"other"`) {
		t.Fatalf("after remove: %s", b)
	}
	local, _ = os.ReadFile(filepath.Join(root, ".claude", "settings.local.json"))
	if strings.Contains(string(local), ServerName) {
		t.Fatalf("approval left: %s", local)
	}
}
