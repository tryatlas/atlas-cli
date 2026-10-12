package mcpconfig

import (
	"os"
	"strings"
	"testing"

	"github.com/tryatlas/cli/internal/hooks"
)

func TestPiWriter(t *testing.T) {
	root := t.TempDir()
	w, ok := For("pi")
	if !ok {
		t.Fatal("no writer for pi")
	}
	if w.Path(root) != root+"/.pi/mcp.json" {
		t.Fatalf("path = %q", w.Path(root))
	}
	// A user's own server survives.
	if err := os.MkdirAll(root+"/.pi", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(w.Path(root), []byte(`{"mcpServers":{"mine":{"url":"https://example.com/mcp"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := w.State(root); got != hooks.Absent {
		t.Fatalf("state before = %v", got)
	}
	changed, err := w.Write(root)
	if err != nil || !changed {
		t.Fatalf("write: changed=%v err=%v", changed, err)
	}
	b, _ := os.ReadFile(w.Path(root))
	s := string(b)
	for _, want := range []string{`"atlas_memory"`, `"command": "atlas-cli"`, `"mcp-bridge"`, `"--agent"`, `"pi"`, `"mine"`} {
		if !strings.Contains(s, want) {
			t.Errorf("file lacks %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, `"type"`) {
		t.Errorf("entry carries a type; command implies stdio:\n%s", s)
	}
	if got := w.State(root); got != hooks.Current {
		t.Fatalf("state after = %v", got)
	}
	if changed, err := w.Write(root); err != nil || changed {
		t.Fatalf("second write: changed=%v err=%v", changed, err)
	}
	if err := w.Remove(root); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(w.Path(root))
	if strings.Contains(string(b), "atlas_memory") || !strings.Contains(string(b), `"mine"`) {
		t.Fatalf("after remove:\n%s", b)
	}
	if got := w.State(root); got != hooks.Absent {
		t.Fatalf("state after remove = %v", got)
	}
}
