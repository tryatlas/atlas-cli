package opencode

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestLiveExport runs the real `opencode export` over the most recent
// session on this machine (or $ATLAS_LIVE_OPENCODE_SESSION) and parses it.
// Guarded by ATLAS_LIVE=1: it needs opencode and a session.
func TestLiveExport(t *testing.T) {
	if os.Getenv("ATLAS_LIVE") != "1" {
		t.Skip("ATLAS_LIVE=1 runs opencode")
	}
	if _, err := exec.LookPath("opencode"); err != nil {
		t.Skip("opencode not on PATH")
	}
	id := os.Getenv("ATLAS_LIVE_OPENCODE_SESSION")
	if id == "" {
		out, err := exec.Command("opencode", "session", "list", "--format", "json", "-n", "1").Output()
		if err != nil {
			t.Fatalf("session list: %v", err)
		}
		var list []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(out, &list); err != nil || len(list) == 0 {
			t.Fatalf("session list: %v %s", err, out)
		}
		id = list[0].ID
	}
	root := t.TempDir()
	path := CachePath(root, id)
	s, err := fetch(id, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) == 0 {
		t.Fatalf("session %s exported no messages", id)
	}
	turn := turnOf(s.Messages)
	t.Logf("session %s: %d messages, %d prompts, %d items, model %q, usage %+v", id, len(s.Messages), len(turn.Prompts), len(turn.Items), turn.Model, turn.Usage)
	for _, it := range turn.Items {
		if it.Tool != nil {
			t.Logf("  tool %s %s %s %v", it.Tool.Name, it.Tool.Status, it.Tool.NativeID, it.Tool.Locations)
		} else {
			t.Logf("  %s %q", it.Mode, firstLine(it.Body))
		}
	}
	if len(turn.Prompts) == 0 || !turn.HasUsage || turn.Model == "" {
		t.Fatalf("a real session should have prompts, usage and a model: %+v", turn)
	}
	if pos, err := rd.Position(path); err != nil || pos != len(s.Messages) {
		t.Fatalf("position from cache: %d %v", pos, err)
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}
