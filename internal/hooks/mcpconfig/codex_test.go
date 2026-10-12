package mcpconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"

	"github.com/tryatlas/cli/internal/hooks"
)

// parsedServer decodes the file and returns the atlas_memory table.
func parsedServer(t *testing.T, path string) (command string, args []string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Servers map[string]struct {
			Command string   `toml:"command"`
			Args    []string `toml:"args"`
		} `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid TOML: %v\n%s", err, b)
	}
	s, ok := doc.Servers[ServerName]
	if !ok {
		t.Fatalf("no %s table:\n%s", ServerName, b)
	}
	return s.Command, s.Args
}

func TestCodexAddIdempotentRemove(t *testing.T) {
	root := t.TempDir()
	w, ok := For("codex")
	if !ok {
		t.Fatal("codex writer not registered")
	}
	path := w.Path(root)
	if path != filepath.Join(root, ".codex", "config.toml") {
		t.Fatal(path)
	}
	if w.State(root) != hooks.Absent {
		t.Fatal("expected absent")
	}
	changed, err := w.Write(root)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode())
	}
	cmd, args := parsedServer(t, path)
	if cmd != "atlas-cli" || strings.Join(args, " ") != "mcp-bridge --project "+root+" --agent codex" {
		t.Fatalf("%s %v", cmd, args)
	}
	b, _ := os.ReadFile(path)
	if string(b) != codexBlock(root) {
		t.Fatalf("a fresh file is only the block:\n%s", b)
	}
	if w.State(root) != hooks.Current {
		t.Fatalf("state %v", w.State(root))
	}
	if changed, err := w.Write(root); err != nil || changed {
		t.Fatalf("second write changed=%v err=%v", changed, err)
	}
	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("an otherwise empty file should be removed")
	}
	if err := w.Remove(root); err != nil {
		t.Fatal("remove on a missing file should be a no-op")
	}
}

func TestCodexPreservesUserConfig(t *testing.T) {
	root := t.TempDir()
	w, _ := For("codex")
	path := w.Path(root)
	user := "# my codex settings\nmodel = \"gpt-5-codex\"\n\n[mcp_servers.github]\ncommand = \"gh-mcp\" # keep this comment\nargs = [\"serve\"]\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := w.Write(root); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(path)
	if !strings.HasPrefix(string(b), user) || !strings.HasSuffix(string(b), codexBlock(root)) {
		t.Fatalf("user content not preserved:\n%s", b)
	}
	var doc map[string]any
	if err := toml.Unmarshal(b, &doc); err != nil {
		t.Fatalf("invalid TOML: %v\n%s", err, b)
	}
	servers := doc["mcp_servers"].(map[string]any)
	if _, ok := servers["github"]; !ok {
		t.Fatal("user server lost")
	}
	if _, ok := servers[ServerName]; !ok {
		t.Fatal("atlas server missing")
	}
	if doc["model"] != "gpt-5-codex" {
		t.Fatal("top-level key lost")
	}

	// A block for another root is outdated and gets replaced in place.
	other := filepath.Join(root, "elsewhere")
	if _, err := w.Write(other); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(w.Path(other), path); err != nil {
		t.Fatal(err)
	}
	stale := codexBlock(other)
	if err := os.WriteFile(path, []byte(user+"\n"+stale+"\n# trailing user note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w.State(root) != hooks.Outdated {
		t.Fatalf("state %v, want outdated", w.State(root))
	}
	if changed, err := w.Write(root); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != user+"\n"+codexBlock(root)+"\n# trailing user note\n" {
		t.Fatalf("block not replaced in place:\n%s", b)
	}
	if w.State(root) != hooks.Current {
		t.Fatalf("state %v", w.State(root))
	}

	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != user+"\n# trailing user note\n" {
		t.Fatalf("remove left:\n%s", b)
	}
	if w.State(root) != hooks.Absent {
		t.Fatalf("state %v", w.State(root))
	}
}

func TestCodexRefusesUnmanagedEntry(t *testing.T) {
	root := t.TempDir()
	w, _ := For("codex")
	path := w.Path(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "[mcp_servers.atlas_memory]\ncommand = \"something-else\"\n"
	if err := os.WriteFile(path, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(root); err == nil {
		t.Fatal("expected an error for a user-owned atlas_memory table")
	}
	b, _ := os.ReadFile(path)
	if string(b) != user {
		t.Fatal("file was changed")
	}
	if w.State(root) != hooks.Absent {
		t.Fatalf("state %v", w.State(root))
	}
}

func TestCodexTOMLString(t *testing.T) {
	root := `/Users/me/Code/my "quoted" project\with backslash`
	block := codexBlock(root)
	var doc struct {
		Servers map[string]struct {
			Args []string `toml:"args"`
		} `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal([]byte(block), &doc); err != nil {
		t.Fatalf("invalid TOML: %v\n%s", err, block)
	}
	if doc.Servers[ServerName].Args[2] != root {
		t.Fatalf("round trip: %q", doc.Servers[ServerName].Args[2])
	}
}
