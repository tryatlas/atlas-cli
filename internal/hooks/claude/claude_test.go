package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func TestParse(t *testing.T) {
	a := Agent{}
	payload := `{"session_id":"abc","transcript_path":"/t/abc.jsonl","cwd":"/w","model":"claude-opus-5-5","prompt":"do it <ide_opened_file>x</ide_opened_file>","hook_event_name":"UserPromptSubmit"}`
	ev, err := a.Parse(VerbPromptSubmit, []byte(payload))
	if err != nil || ev.Kind != hooks.TurnStart || ev.Prompt != "do it" || ev.SessionID != "abc" || ev.Transcript != "/t/abc.jsonl" || ev.Cwd != "/w" {
		t.Fatalf("%+v %v", ev, err)
	}
	for verb, kind := range map[string]hooks.Kind{VerbSessionStart: hooks.SessionStart, VerbStop: hooks.TurnEnd, VerbStopFailure: hooks.TurnEnd,
		VerbPreCompact: hooks.Compaction, VerbSessionEnd: hooks.SessionEnd} {
		ev, err := a.Parse(verb, []byte(payload))
		if err != nil || ev.Kind != kind {
			t.Fatalf("%s → %+v %v", verb, ev, err)
		}
		if verb == VerbStopFailure && !ev.TurnEndFailed {
			t.Fatal("stop-failure must mark the turn failed")
		}
	}
	if ev, err := a.Parse("unknown", []byte(payload)); ev != nil || err != nil {
		t.Fatal("unknown verb must be nil, nil")
	}
	if _, err := a.Parse(VerbStop, []byte(`not json`)); err == nil {
		t.Fatal("bad payload must error")
	}
	if out := a.Response(hooks.SessionStart, "Atlas: on"); string(out) != `{"systemMessage":"Atlas: on"}`+"\n" {
		t.Fatalf("response = %s", out)
	}
	if a.Response(hooks.TurnEnd, "x") != nil {
		t.Fatal("only SessionStart answers")
	}
}

func TestInstallWritesTheLocalSettings(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	if a.State(root) != hooks.Absent {
		t.Fatal("state before")
	}
	changed, err := a.Install(root, false)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	b, err := os.ReadFile(SettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type, Command string
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	for _, typ := range []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "PreCompact", "SessionEnd"} {
		g := obj.Hooks[typ]
		if len(g) != 1 || len(g[0].Hooks) != 1 || g[0].Hooks[0].Type != "command" || !hooks.Owned(g[0].Hooks[0].Command) {
			t.Fatalf("%s: %+v", typ, g)
		}
	}
	if !strings.Contains(obj.Hooks["SessionStart"][0].Hooks[0].Command, "systemMessage") {
		t.Fatal("SessionStart must warn when atlas is missing")
	}
	if fi, _ := os.Stat(SettingsPath(root)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if a.State(root) != hooks.Current {
		t.Fatal("state after")
	}
}

const fixture = `{"type":"user","message":{"role":"user","content":"make hello.txt"},"uuid":"u1","isMeta":false}
{"type":"user","isMeta":true,"message":{"role":"user","content":"<system-reminder>ignored</system-reminder>"}}
{"type":"assistant","message":{"id":"msg_1","model":"claude-opus-5-5","role":"assistant","content":[{"type":"thinking","thinking":"plan it"}],"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100}}}
{"type":"assistant","message":{"id":"msg_1","model":"claude-opus-5-5","role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Write","input":{"file_path":"/w/hello.txt","content":"hi\n"}}],"usage":{"input_tokens":10,"output_tokens":40,"cache_read_input_tokens":100}}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"File created successfully"}]}}
{"type":"assistant","message":{"id":"msg_2","model":"claude-opus-5-5","role":"assistant","content":[{"type":"text","text":"Created "},{"type":"text","text":"hello.txt."}],"usage":{"input_tokens":20,"output_tokens":7}}}
{"type":"system","subtype":"hook_progress","content":"Running hooks claude-code stop"}
`

func TestReadBuildsTheTurn(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(p, []byte(fixture), 0o600)
	r := reader{}
	pos, err := r.Position(p)
	if err != nil || pos != 7 {
		t.Fatalf("position = %d %v", pos, err)
	}
	turn, end, err := r.Read(p, 0)
	if err != nil || end != 7 {
		t.Fatalf("%v %d", err, end)
	}
	if len(turn.Prompts) != 1 || turn.Prompts[0] != "make hello.txt" {
		t.Fatalf("prompts = %q", turn.Prompts)
	}
	if turn.Model != "claude-opus-5-5" {
		t.Fatalf("model = %q", turn.Model)
	}
	if len(turn.Items) != 3 {
		t.Fatalf("items = %d: %+v", len(turn.Items), turn.Items)
	}
	if turn.Items[0].Mode != "thinking" || turn.Items[0].Body != "plan it" {
		t.Fatalf("item 0 = %+v", turn.Items[0])
	}
	tool := turn.Items[1].Tool
	if tool == nil || tool.NativeID != "toolu_01" || tool.Name != "Write" || tool.Result == nil || *tool.Result != "File created successfully" || tool.Status != "completed" {
		t.Fatalf("item 1 = %+v", turn.Items[1])
	}
	if turn.Items[2].Mode != "text" || turn.Items[2].Body != "Created hello.txt." {
		t.Fatalf("item 2 = %+v", turn.Items[2])
	}
	// Usage: msg_1 keeps its highest output row (40), msg_2 adds 7.
	if !turn.HasUsage || turn.Usage.Input != 30 || turn.Usage.Output != 47 || turn.Usage.CacheRead != 100 {
		t.Fatalf("usage = %+v", turn.Usage)
	}
	// From the middle: only what follows.
	turn, _, _ = r.Read(p, 5)
	if len(turn.Prompts) != 0 || len(turn.Items) != 1 || turn.Usage.Output != 7 {
		t.Fatalf("partial read = %+v", turn)
	}
	r.Settle(context.Background(), p) // the hook_progress line is there: returns at once
}
