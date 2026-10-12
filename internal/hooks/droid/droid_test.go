package droid

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func loadHooks(t *testing.T, root string) (map[string]json.RawMessage, map[string][]hooks.Matcher) {
	t.Helper()
	obj, err := hooks.LoadJSONObject(SettingsPath(root))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]hooks.Matcher{}
	if raw, ok := obj["hooks"]; ok {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
	}
	return obj, out
}

func commands(groups []hooks.Matcher) []string {
	var out []string
	for _, g := range groups {
		for _, h := range g.Hooks {
			out = append(out, h.Command)
		}
	}
	return out
}

func TestInstallUninstall(t *testing.T) {
	root := t.TempDir()
	path := SettingsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := hooks.Wrap(hooks.Command(ID, "stop-failure"))
	seed := `{"model":"claude-sonnet-4-5","hooks":{` +
		`"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"echo hi"}]}],` +
		`"Stop":[{"matcher":"","hooks":[{"type":"command","command":` + string(hooks.Raw(stale)) + `}]}]}}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	a := Agent{}
	if a.State(root) != hooks.Outdated {
		t.Fatalf("a stale Atlas entry alone should be outdated, got %v", a.State(root))
	}

	changed, err := a.Install(root, false)
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	obj, hk := loadHooks(t, root)
	if string(obj["model"]) != `"claude-sonnet-4-5"` {
		t.Fatalf("user key lost: %s", obj["model"])
	}
	start := hk["SessionStart"]
	if len(start) != 1 || *start[0].Matcher != "" {
		t.Fatalf("SessionStart groups: %+v", start)
	}
	want := []string{
		"echo hi",
		hooks.WrapWithMessage(hooks.Command(ID, VerbSessionStart), MissingMessage),
		hooks.Wrap(hooks.Command(ID, VerbPromptSubmit)),
	}
	if got := commands(start); strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("SessionStart commands:\n got %q\nwant %q", got, want)
	}
	if !strings.Contains(want[1], `printf "%s\n" "Atlas is enabled here but atlas-cli is not installed or not on PATH."`) {
		t.Fatalf("plain-text missing message: %s", want[1])
	}
	if got := commands(hk["Stop"]); len(got) != 1 || got[0] != hooks.Wrap(hooks.Command(ID, VerbStop)) {
		t.Fatalf("Stop should hold only the current hook (stale pruned): %q", got)
	}
	for typ, verb := range map[string]string{"UserPromptSubmit": VerbPromptSubmit, "PostToolUse": VerbPostToolUse, "PreCompact": VerbPreCompact, "SessionEnd": VerbSessionEnd} {
		if got := commands(hk[typ]); len(got) != 1 || got[0] != hooks.Wrap(hooks.Command(ID, verb)) {
			t.Fatalf("%s: %q", typ, got)
		}
	}
	if post := hk["PostToolUse"]; len(post) != 1 || post[0].Matcher == nil || *post[0].Matcher != "" {
		t.Fatalf("PostToolUse should match every tool with matcher \"\": %+v", post)
	}
	if a.State(root) != hooks.Current {
		t.Fatalf("state after install: %v", a.State(root))
	}
	if changed, err := a.Install(root, false); err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}
	fi, _ := os.Stat(path)
	if fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v, want 0644 for a project file", fi.Mode().Perm())
	}

	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	obj, hk = loadHooks(t, root)
	if string(obj["model"]) != `"claude-sonnet-4-5"` {
		t.Fatalf("user key lost on uninstall: %s", obj["model"])
	}
	if got := commands(hk["SessionStart"]); len(got) != 1 || got[0] != "echo hi" {
		t.Fatalf("user hook should survive: %q", got)
	}
	if len(hk) != 1 {
		t.Fatalf("Atlas-only types should be gone: %v", hk)
	}
	if a.State(root) != hooks.Absent {
		t.Fatalf("state after uninstall: %v", a.State(root))
	}
}

func TestStateAbsentWithoutFile(t *testing.T) {
	root := t.TempDir()
	if (Agent{}).State(root) != hooks.Absent {
		t.Fatal("no file should be absent")
	}
	if (Agent{}).Detect(root) && !hasDroid() {
		t.Fatal("no .factory and no droid on PATH should not detect")
	}
	if err := os.MkdirAll(filepath.Join(root, ".factory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !(Agent{}).Detect(root) {
		t.Fatal(".factory should detect")
	}
}

func hasDroid() bool {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(filepath.Join(dir, "droid")); err == nil {
			return true
		}
	}
	return false
}

func TestParse(t *testing.T) {
	a := Agent{}
	base := `"session_id":"s-1","transcript_path":"/h/.factory/sessions/repo/s-1.jsonl","cwd":"/w"`
	cases := []struct {
		verb    string
		payload string
		kind    hooks.Kind
		prompt  string
		model   string
	}{
		{VerbSessionStart, `{` + base + `}`, hooks.SessionStart, "", ""},
		{VerbPromptSubmit, `{` + base + `,"prompt":"  fix it \n","model":"claude-opus-4"}`, hooks.TurnStart, "fix it", "claude-opus-4"},
		{VerbPromptSubmit, `{` + base + `}`, hooks.TurnStart, "", ""}, // fired from SessionStart: no prompt
		{VerbStop, `{` + base + `,"model":"custom:Gemini-2.5-Pro-0"}`, hooks.TurnEnd, "", "Gemini-2.5-Pro-0"},
		{VerbStop, `{` + base + `}`, hooks.TurnEnd, "", ""}, // no settings file beside this path
		{VerbPreCompact, `{` + base + `}`, hooks.Compaction, "", ""},
		{VerbSessionEnd, `{` + base + `}`, hooks.SessionEnd, "", ""},
	}
	for _, c := range cases {
		ev, err := a.Parse(c.verb, []byte(c.payload))
		if err != nil || ev == nil {
			t.Fatalf("%s: ev=%v err=%v", c.verb, ev, err)
		}
		if ev.Kind != c.kind || ev.SessionID != "s-1" || ev.Transcript != "/h/.factory/sessions/repo/s-1.jsonl" || ev.Cwd != "/w" {
			t.Fatalf("%s: %+v", c.verb, ev)
		}
		if ev.Prompt != c.prompt || ev.Model != c.model {
			t.Fatalf("%s: prompt %q model %q, want %q %q", c.verb, ev.Prompt, ev.Model, c.prompt, c.model)
		}
	}
	if ev, err := a.Parse("notification", []byte(`{`+base+`}`)); err != nil || ev != nil {
		t.Fatalf("unknown verb: %v %v", ev, err)
	}
	if !slices.Contains(a.Verbs(), VerbPostToolUse) {
		t.Fatalf("verbs should include post-tool-use: %q", a.Verbs())
	}
	if _, err := a.Parse(VerbStop, []byte(`nope`)); err == nil {
		t.Fatal("bad JSON should error")
	}
}

func TestParsePostToolUse(t *testing.T) {
	a := Agent{}
	base := `"session_id":"s-1","transcript_path":"/h/.factory/sessions/repo/s-1.jsonl","cwd":"/w","hook_event_name":"PostToolUse"`
	// An object response is kept as its JSON text.
	post := `{` + base + `,"tool_name":"Edit","tool_use_id":"toolu_01","tool_input":{"file_path":"/w/main.go","old_string":"a","new_string":"b"},"tool_response":{"filePath":"/w/main.go","success":true}}`
	ev, err := a.Parse(VerbPostToolUse, []byte(post))
	if err != nil || ev == nil || ev.Kind != hooks.ToolUse || ev.SessionID != "s-1" || ev.Transcript != "/h/.factory/sessions/repo/s-1.jsonl" || ev.Cwd != "/w" {
		t.Fatalf("post-tool-use: %+v %v", ev, err)
	}
	tool := ev.Tool
	if tool == nil || tool.NativeID != "toolu_01" || tool.Name != "Edit" || tool.Status != "completed" ||
		string(tool.Arguments) != `{"file_path":"/w/main.go","old_string":"a","new_string":"b"}` ||
		tool.Result == nil || *tool.Result != `{"filePath":"/w/main.go","success":true}` {
		t.Fatalf("tool: %+v", tool)
	}
	// A string response is kept verbatim.
	ev, err = a.Parse(VerbPostToolUse, []byte(`{`+base+`,"tool_name":"Execute","tool_use_id":"toolu_02","tool_input":{"command":"go test ./..."},"tool_response":"ok\n"}`))
	if err != nil || ev == nil || ev.Tool == nil || ev.Tool.Result == nil || *ev.Tool.Result != "ok\n" || ev.Tool.NativeID != "toolu_02" {
		t.Fatalf("string response: %+v %v", ev, err)
	}
	// No response at all, or a null one, leaves Result nil.
	for _, tail := range []string{``, `,"tool_response":null`} {
		ev, err = a.Parse(VerbPostToolUse, []byte(`{`+base+`,"tool_name":"Read","tool_use_id":"toolu_03","tool_input":{"file_path":"/w/x"}`+tail+`}`))
		if err != nil || ev == nil || ev.Tool == nil || ev.Tool.Result != nil {
			t.Fatalf("no response (%q): %+v %v", tail, ev, err)
		}
	}
	// Without a tool id (Droid has been seen to omit it) a stable id is
	// derived from the call, so it still lands per event; the dispatcher
	// matches the transcript's call by arguments or written path.
	noID := `{` + base + `,"tool_name":"Edit","tool_input":{"file_path":"/w/x"},"tool_response":"ok"}`
	ev, err = a.Parse(VerbPostToolUse, []byte(noID))
	if err != nil || ev == nil || ev.Tool == nil || !strings.HasPrefix(ev.Tool.NativeID, "droid-") || len(ev.Tool.NativeID) != 22 {
		t.Fatalf("payload without an id: %+v %v", ev, err)
	}
	again, _ := a.Parse(VerbPostToolUse, []byte(noID))
	if again.Tool.NativeID != ev.Tool.NativeID {
		t.Fatal("the derived id must be stable")
	}
	// Without a name there is nothing to record: nil, and the transcript
	// supplies the call at Stop.
	for _, raw := range []string{
		`{` + base + `,"tool_use_id":"toolu_04","tool_input":{}}`,
		`{` + base + `}`,
	} {
		if ev, err := a.Parse(VerbPostToolUse, []byte(raw)); err != nil || ev != nil {
			t.Fatalf("payload without a call should be nil: %s → %+v %v", raw, ev, err)
		}
	}
}

func TestStopModelFromSettings(t *testing.T) {
	dir := t.TempDir()
	transcript := filepath.Join(dir, "abc.jsonl")
	if SettingsFor(transcript) != filepath.Join(dir, "abc.settings.json") {
		t.Fatalf("settings path: %s", SettingsFor(transcript))
	}
	if err := os.WriteFile(SettingsFor(transcript), []byte(`{"model":"custom:Gemini-2.5-Pro-0","other":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, err := Agent{}.Parse(VerbStop, []byte(`{"session_id":"abc","transcript_path":"`+transcript+`"}`))
	if err != nil || ev.Model != "Gemini-2.5-Pro-0" {
		t.Fatalf("model from settings: %+v %v", ev, err)
	}
	// A payload model wins over the file.
	ev, _ = Agent{}.Parse(VerbStop, []byte(`{"session_id":"abc","transcript_path":"`+transcript+`","model":"claude-sonnet-4-5"}`))
	if ev.Model != "claude-sonnet-4-5" {
		t.Fatalf("payload model should win: %q", ev.Model)
	}
	// A corrupt file yields no model.
	_ = os.WriteFile(SettingsFor(transcript), []byte(`{`), 0o644)
	if m := ModelFromSettings(transcript); m != "" {
		t.Fatalf("corrupt settings: %q", m)
	}
}

func TestResponseAndResume(t *testing.T) {
	a := Agent{}
	if string(a.Response(hooks.SessionStart, "Atlas: capturing")) != "Atlas: capturing\n" {
		t.Fatalf("response: %q", a.Response(hooks.SessionStart, "Atlas: capturing"))
	}
	if a.Response(hooks.TurnEnd, "x") != nil || a.Response(hooks.SessionStart, "") != nil {
		t.Fatal("only a SessionStart message prints")
	}
	if a.ResumeCommand("s-1") != "droid --session-id s-1" {
		t.Fatalf("resume: %q", a.ResumeCommand("s-1"))
	}
	if a.ID() != "droid" || a.Name() != "Factory Droid" || a.Binary() != "droid" {
		t.Fatal("identity")
	}
	if got, ok := hooks.Get(ID); !ok || got.ID() != ID {
		t.Fatal("init should register the agent")
	}
}

const fixture = `{"type":"session_start","id":"s-1","cwd":"/w","model":"claude-sonnet-4-5"}
{"type":"message","id":"e1","message":{"role":"user","content":[{"type":"text","text":"Create hello.go and fix main.go"}]}}
{"type":"message","id":"e2","message":{"role":"assistant","id":"m1","model":"claude-sonnet-4-5","content":[{"type":"thinking","thinking":"Plan it"},{"type":"text","text":"Creating "}],"usage":{"input_tokens":100,"output_tokens":5,"cache_creation_input_tokens":10,"cache_read_input_tokens":20}}}
{"type":"message","id":"e3","message":{"role":"assistant","id":"m1","model":"claude-sonnet-4-5","content":[{"type":"text","text":"the file."},{"type":"tool_use","id":"toolu_1","name":"Create","input":{"file_path":"hello.go","content":"package main\n"}}],"usage":{"input_tokens":100,"output_tokens":12,"cache_creation_input_tokens":10,"cache_read_input_tokens":20}}}
{"type":"message","id":"e4","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}}

{"type":"message","id":"e5","message":{"role":"assistant","id":"m2","content":[{"type":"tool_use","id":"toolu_2","name":"Edit","input":{"file_path":"main.go","old_string":"a","new_string":"b"}}],"usage":{"input_tokens":200,"output_tokens":7,"cache_creation_input_tokens":0,"cache_read_input_tokens":30}}}
{"type":"message","id":"e6","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_2","is_error":true,"content":[{"type":"text","text":"no such file"}]}]}}
{"type":"message","id":"e7","message":{"role":"assistant","content":"Done.","usage":{"input_tokens":300,"output_tokens":3}}}
`

func TestReadTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s-1.jsonl")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	r := reader{}
	pos, err := r.Position(path)
	if err != nil || pos != 9 {
		t.Fatalf("position %d %v", pos, err)
	}
	turn, end, err := r.Read(path, 0)
	if err != nil || end != 9 {
		t.Fatalf("read: end=%d err=%v", end, err)
	}
	if len(turn.Prompts) != 1 || turn.Prompts[0] != "Create hello.go and fix main.go" {
		t.Fatalf("prompts: %q", turn.Prompts)
	}
	if turn.Model != "claude-sonnet-4-5" {
		t.Fatalf("model: %q", turn.Model)
	}
	items := turn.Items
	if len(items) != 5 {
		t.Fatalf("items: %d %+v", len(items), items)
	}
	if items[0].Mode != "thinking" || items[0].Body != "Plan it" || items[0].ID != "m1-0" {
		t.Fatalf("thinking run: %+v", items[0])
	}
	if items[1].Mode != "text" || items[1].Body != "Creating the file." {
		t.Fatalf("text run should merge across streamed rows of m1: %+v", items[1])
	}
	create := items[2].Tool
	if create == nil || create.NativeID != "toolu_1" || create.Name != "Create" || create.Status != "completed" ||
		create.Result == nil || *create.Result != "ok" || !strings.Contains(string(create.Arguments), `"file_path":"hello.go"`) {
		t.Fatalf("Create: %+v", create)
	}
	edit := items[3].Tool
	if edit == nil || edit.Name != "Edit" || edit.Status != "failed" || edit.Result == nil || *edit.Result != "no such file" ||
		!strings.Contains(string(edit.Arguments), `"file_path":"main.go"`) {
		t.Fatalf("Edit: %+v", edit)
	}
	if items[4].Tool != nil || items[4].Body != "Done." || items[4].ID != "e7-0" {
		t.Fatalf("string content run keyed by envelope id: %+v", items[4])
	}
	// m1 counted once at its highest output row; m2 and e7 once each.
	if !turn.HasUsage || turn.Usage.Input != 600 || turn.Usage.Output != 22 || turn.Usage.CacheCreation != 10 || turn.Usage.CacheRead != 50 {
		t.Fatalf("usage: %+v has=%v", turn.Usage, turn.HasUsage)
	}

	// From an offset: only the tail.
	tail, end, err := r.Read(path, 7)
	if err != nil || end != 9 || len(tail.Prompts) != 0 || len(tail.Items) != 1 || tail.Items[0].Body != "Done." {
		t.Fatalf("tail: %+v end=%d err=%v", tail, end, err)
	}
	if !tail.HasUsage || tail.Usage.Input != 300 || tail.Usage.Output != 3 {
		t.Fatalf("tail usage: %+v", tail.Usage)
	}
	if tail.Model != "" {
		t.Fatalf("no model in the tail: %q", tail.Model)
	}
}

func TestReadEmptyAndMalformed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.jsonl")
	if err := os.WriteFile(path, []byte("not json\n{\"type\":\"message\",\"id\":\"e\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"  \"}]}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	turn, end, err := (reader{}).Read(path, 0)
	if err != nil || end != 2 || len(turn.Items) != 0 || turn.HasUsage {
		t.Fatalf("%+v %d %v", turn, end, err)
	}
	if _, _, err := (reader{}).Read(filepath.Join(t.TempDir(), "missing.jsonl"), 0); err == nil {
		t.Fatal("missing file should error")
	}
}
