package cursor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/cli/internal/hooks"
)

// TestPerEventToolDelivery drives the dispatcher through the hooks Cursor's
// TUI fires for one turn: the prompt opens the turn, afterFileEdit and
// afterShellExecution deliver their calls as they complete, and stop closes
// the turn from the transcript without recording those calls a second time.
// The transcript's tool_use blocks carry no id, so the match is by written
// path (Write, StrReplace) and by command text (Shell, whose transcript
// arguments carry extra fields).
func TestPerEventToolDelivery(t *testing.T) {
	root := t.TempDir()
	transcript := filepath.Join(t.TempDir(), "c.jsonl")
	const id = "e00128c9-9da5-4ab7-8ad9-2743f63d3734"
	common := `"conversation_id":"` + id + `","generation_id":"c2eb53ba","model":"default","workspace_roots":["` + root + `"],"transcript_path":"` + transcript + `"`
	file := filepath.Join(root, "probe.txt")

	var deliveries []hooks.Delivery
	d := &hooks.Dispatcher{Root: root, Version: "test", Now: time.Now, Deliver: func(_ context.Context, dl hooks.Delivery) error {
		deliveries = append(deliveries, dl)
		return nil
	}}
	a := Agent{}
	run := func(verb, body string) {
		t.Helper()
		ev, err := a.Parse(verb, []byte(`{`+common+`,`+body+`}`))
		if err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
		if _, err := d.Run(context.Background(), a, ev); err != nil {
			t.Fatalf("%s: %v", verb, err)
		}
	}

	if err := os.WriteFile(transcript, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	run(VerbSessionStart, `"hook_event_name":"sessionStart"`)
	run(VerbBeforePrompt, `"prompt":"make probe.txt"`)
	// The agent works: Cursor's Write, then its StrReplace, then a Shell.
	_ = os.WriteFile(file, []byte("alpha\n"), 0o644)
	run(VerbAfterFileEdit, `"file_path":"`+file+`","edits":[{"old_string":"","new_string":"alpha\n"}]`)
	_ = os.WriteFile(file, []byte("beta\n"), 0o644)
	run(VerbAfterFileEdit, `"file_path":"`+file+`","edits":[{"old_string":"alpha","new_string":"beta"}]`)
	run(VerbAfterShell, `"command":"cat probe.txt","output":"beta\n","duration":12,"sandbox":false`)
	// A command the hooks did not report stays with the transcript.
	lines := []string{
		`{"role":"user","message":{"content":[{"type":"text","text":"<user_query>make probe.txt</user_query>"}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"path":"` + file + `","contents":"alpha\n"}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"path":"` + file + `","old_string":"alpha","new_string":"beta"}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"cat probe.txt","working_directory":"` + root + `","description":"Show it"}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"tool_use","name":"Shell","input":{"command":"echo unreported"}}]}}`,
		`{"role":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`,
	}
	if err := os.WriteFile(transcript, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(VerbStop, `"status":"completed","loop_count":0,"input_tokens":1000,"output_tokens":20,"cache_read_tokens":600,"cache_write_tokens":0`)

	var kinds []string
	var tools []string
	for _, dl := range deliveries {
		switch {
		case dl.Start:
			kinds = append(kinds, "start")
		case dl.Open != nil:
			kinds = append(kinds, "open:"+dl.Open.Prompt)
		case dl.Tool != nil:
			kinds = append(kinds, "tool:"+dl.Tool.Name)
			tools = append(tools, dl.Tool.NativeID)
		case dl.Turn != nil:
			kinds = append(kinds, "turn")
		}
	}
	want := "start|open:make probe.txt|tool:Write|tool:Edit|tool:Shell|turn"
	if got := strings.Join(kinds, "|"); got != want {
		t.Fatalf("deliveries %s, want %s", got, want)
	}
	for i, id := range tools {
		if !strings.HasPrefix(id, "c2eb53ba-") {
			t.Fatalf("tool %d id %q lacks the generation", i, id)
		}
		for _, other := range tools[i+1:] {
			if other == id {
				t.Fatalf("two calls share id %q", id)
			}
		}
	}
	turn := deliveries[len(deliveries)-1].Turn
	if !turn.Open || turn.Prompt != "make probe.txt" || turn.Usage == nil || turn.Usage.Output != 20 || turn.Usage.CacheRead != 600 {
		t.Fatalf("turn %+v", turn)
	}
	var left []string
	for _, it := range turn.Items {
		if it.Tool != nil {
			left = append(left, it.Tool.Name+":"+string(it.Tool.Arguments))
		} else {
			left = append(left, it.Mode+":"+it.Body)
		}
	}
	if len(left) != 2 || left[0] != `Shell:{"command":"echo unreported"}` || left[1] != "text:done" {
		t.Fatalf("transcript items left after dedupe: %q", left)
	}
	if len(turn.Changed) != 0 {
		t.Fatalf("the delivered Write explains probe.txt; changed = %q", turn.Changed)
	}
}

func TestEditAndShellToolShapes(t *testing.T) {
	p := payload{GenerationID: "g1", FilePath: "/w/a.txt", Edits: []edit{{OldString: "", NewString: "x\n"}}}
	w := p.editTool()
	if w.Name != "Write" || string(w.Arguments) != `{"contents":"x\n","path":"/w/a.txt"}` || w.Status != "completed" {
		t.Fatalf("write %+v %s", w, w.Arguments)
	}
	p.Edits = []edit{{OldString: "x", NewString: "y"}}
	e := p.editTool()
	if e.Name != "Edit" || !strings.Contains(string(e.Arguments), `"old_string":"x"`) || !strings.Contains(string(e.Arguments), `"edits":[{"old_string":"x","new_string":"y"}]`) {
		t.Fatalf("edit %+v %s", e, e.Arguments)
	}
	if e.NativeID == w.NativeID || !strings.HasPrefix(e.NativeID, "g1-edit-") {
		t.Fatalf("ids %q %q", w.NativeID, e.NativeID)
	}
	p.Edits = []edit{{OldString: "a", NewString: "b"}, {OldString: "c", NewString: "d"}}
	if m := p.editTool(); m.Name != "Edit" || strings.Count(string(m.Arguments), `"old_string"`) != 2 {
		t.Fatalf("multi-edit flattened: %s", m.Arguments)
	}
	if (payload{}).editTool() != nil || (payload{Command: "  "}).shellTool() != nil {
		t.Fatal("empty payloads should give no tool")
	}
	out := "ok\n"
	s := payload{GenerationID: "g1", Command: "ls", Output: &out}.shellTool()
	if s.Name != "Shell" || string(s.Arguments) != `{"command":"ls"}` || s.Result == nil || *s.Result != "ok\n" || !strings.HasPrefix(s.NativeID, "g1-shell-") {
		t.Fatalf("shell %+v", s)
	}
	if n := (payload{Command: "ls"}).shellTool(); n.Result != nil || !strings.HasPrefix(n.NativeID, "shell-") {
		t.Fatalf("no output, no generation: %+v", n)
	}
}
