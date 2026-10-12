package antigravity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func readHooks(t *testing.T, root string) map[string]json.RawMessage {
	t.Helper()
	b, err := os.ReadFile(HooksPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return m
}

func writeHooks(t *testing.T, root, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(HooksPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HooksPath(root), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRegistered(t *testing.T) {
	a, ok := hooks.Get(ID)
	if !ok {
		t.Fatal("not registered")
	}
	if a.Name() != "Antigravity" || a.Binary() != "agy" {
		t.Fatalf("Name/Binary = %q/%q", a.Name(), a.Binary())
	}
	if got := a.ResumeCommand("c1"); got != "agy --conversation c1" {
		t.Fatalf("ResumeCommand = %q", got)
	}
	if a.Response(hooks.SessionStart, "hi") != nil {
		t.Fatal("Response should print nothing")
	}
}

func TestInstallIntoEmpty(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	if a.State(root) != hooks.Absent {
		t.Fatalf("State = %v, want absent", a.State(root))
	}
	changed, err := a.Install(root, false)
	if err != nil || !changed {
		t.Fatalf("Install = %v, %v; want changed", changed, err)
	}
	file := readHooks(t, root)
	if len(file) != 1 {
		t.Fatalf("file has %d keys: %v", len(file), file)
	}
	type group struct {
		Matcher string              `json:"matcher"`
		Hooks   []hooks.CommandHook `json:"hooks"`
	}
	var e struct {
		Enabled       *bool               `json:"enabled"`
		PreToolUse    []group             `json:"PreToolUse"`
		PostToolUse   []group             `json:"PostToolUse"`
		PreInvocation []hooks.CommandHook `json:"PreInvocation"`
		Stop          []hooks.CommandHook `json:"Stop"`
	}
	if err := json.Unmarshal(file[Key], &e); err != nil {
		t.Fatal(err)
	}
	if e.Enabled != nil {
		t.Fatal("enabled should be absent on a fresh install")
	}
	if len(e.PreToolUse) != 1 || e.PreToolUse[0].Matcher != "*" || len(e.PreToolUse[0].Hooks) != 1 ||
		e.PreToolUse[0].Hooks[0].Command != hooks.Wrap("atlas-cli hooks antigravity pre-tool-use") ||
		e.PreToolUse[0].Hooks[0].Type != "command" {
		t.Fatalf("PreToolUse = %+v", e.PreToolUse)
	}
	if len(e.PostToolUse) != 1 || e.PostToolUse[0].Matcher != "*" || len(e.PostToolUse[0].Hooks) != 1 ||
		e.PostToolUse[0].Hooks[0].Command != hooks.Wrap("atlas-cli hooks antigravity post-tool-use") ||
		e.PostToolUse[0].Hooks[0].Type != "command" {
		t.Fatalf("PostToolUse = %+v", e.PostToolUse)
	}
	if len(e.PreInvocation) != 1 || e.PreInvocation[0].Command != hooks.Wrap("atlas-cli hooks antigravity pre-invocation") {
		t.Fatalf("PreInvocation = %+v", e.PreInvocation)
	}
	if len(e.Stop) != 1 || e.Stop[0].Command != hooks.Wrap("atlas-cli hooks antigravity stop") || e.Stop[0].Timeout != 300 {
		t.Fatalf("Stop = %+v", e.Stop)
	}
	for _, h := range []string{e.PreToolUse[0].Hooks[0].Command, e.PostToolUse[0].Hooks[0].Command, e.PreInvocation[0].Command, e.Stop[0].Command} {
		if !hooks.Owned(h) {
			t.Fatalf("not owned: %q", h)
		}
	}
	if a.State(root) != hooks.Current {
		t.Fatalf("State = %v, want current", a.State(root))
	}
	if changed, err := a.Install(root, false); err != nil || changed {
		t.Fatalf("second Install = %v, %v; want unchanged", changed, err)
	}
	if changed, err := a.Install(root, true); err != nil || !changed {
		t.Fatalf("forced Install = %v, %v; want changed", changed, err)
	}
}

func TestInstallKeepsOtherEntriesAndDisabled(t *testing.T) {
	root := t.TempDir()
	writeHooks(t, root, `{
  "my-linter": {"PostToolUse": [{"matcher": "write_to_file", "hooks": [{"type": "command", "command": "lint"}]}]},
  "atlas": {"enabled": false, "Stop": [{"type": "command", "command": "sh -c 'exec atlas-cli hooks antigravity old-verb'"}]}
}`)
	a := Agent{}
	if a.State(root) != hooks.Outdated {
		t.Fatalf("State = %v, want outdated", a.State(root))
	}
	changed, err := a.Install(root, false)
	if err != nil || !changed {
		t.Fatalf("Install = %v, %v; want changed", changed, err)
	}
	file := readHooks(t, root)
	if string(file["my-linter"]) == "" || !strings.Contains(string(file["my-linter"]), `"lint"`) {
		t.Fatalf("other entry lost: %v", file)
	}
	var e entry
	if err := json.Unmarshal(file[Key], &e); err != nil {
		t.Fatal(err)
	}
	if e.Enabled == nil || *e.Enabled {
		t.Fatal("the user's enabled:false was not kept")
	}
	if len(e.Stop) != 1 || e.Stop[0].Command != hooks.Wrap("atlas-cli hooks antigravity stop") {
		t.Fatalf("old Stop hook survived: %+v", e.Stop)
	}
	if len(e.PreToolUse) != 1 || len(e.PostToolUse) != 1 || len(e.PreInvocation) != 1 {
		t.Fatalf("entry = %+v", e)
	}
	if a.State(root) != hooks.Current {
		t.Fatalf("State = %v, want current", a.State(root))
	}
	if changed, err := a.Install(root, false); err != nil || changed {
		t.Fatalf("second Install = %v, %v; want unchanged", changed, err)
	}

	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	file = readHooks(t, root)
	if _, ok := file[Key]; ok {
		t.Fatal("entry not removed")
	}
	if _, ok := file["my-linter"]; !ok {
		t.Fatal("other entry lost on uninstall")
	}
	if a.State(root) != hooks.Absent {
		t.Fatalf("State = %v, want absent", a.State(root))
	}
	if err := a.Uninstall(root); err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
	if err := (Agent{}).Uninstall(t.TempDir()); err != nil {
		t.Fatalf("Uninstall without a file: %v", err)
	}
}

func TestStateOutdatedOnExtraHooks(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	if _, err := a.Install(root, false); err != nil {
		t.Fatal(err)
	}
	file := readHooks(t, root)
	var e entry
	_ = json.Unmarshal(file[Key], &e)
	e.PostInvocation = []plainHandler{{Type: "command", Command: hooks.Wrap("atlas-cli hooks antigravity post-invocation")}}
	file[Key] = hooks.Raw(e)
	if err := hooks.SaveJSON(HooksPath(root), file, 0o644); err != nil {
		t.Fatal(err)
	}
	if a.State(root) != hooks.Outdated {
		t.Fatalf("State = %v, want outdated", a.State(root))
	}
}

func TestDetect(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", t.TempDir())
	a := Agent{}
	if a.Detect(root) {
		t.Fatal("detected with no .agents and no agy")
	}
	if err := os.Mkdir(filepath.Join(root, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !a.Detect(root) {
		t.Fatal("not detected with .agents")
	}
}

const common = `"conversationId":"ec33ebf9-0cba-4100-8142-c61503f6c587","modelName":"gemini-3.1-pro-low","workspacePaths":["/workspace/project"],"transcriptPath":"/home/u/.gemini/antigravity-cli/brain/ec33ebf9/.system_generated/logs/transcript_full.jsonl","artifactDirectoryPath":"/workspace/project/.gemini/artifacts"`

func TestParse(t *testing.T) {
	a := Agent{}
	cases := []struct {
		name, verb, payload string
		kind                hooks.Kind // 0 for a nil event
		files               []string
	}{
		{"first invocation", VerbPreInvocation, `{"invocationNum":0,"initialNumSteps":1,` + common + `}`, hooks.TurnStart, nil},
		{"follow-up invocation", VerbPreInvocation, `{"invocationNum":1,"initialNumSteps":5,` + common + `}`, hooks.TurnStart, nil},
		{"read tool", VerbPreToolUse, `{"toolCall":{"name":"run_command","args":{"CommandLine":"npm test","Cwd":"/workspace/project"}},"stepIdx":19,` + common + `}`, 0, nil},
		{"write documented shape", VerbPreToolUse, `{"toolCall":{"name":"write_to_file","args":{"TargetFile":"/workspace/project/a.txt","Overwrite":true,"CodeContent":"x"}},` + common + `}`, hooks.ToolUse, []string{"/workspace/project/a.txt"}},
		{"write wire shape", VerbPreToolUse, `{"toolCall":{"name":"write_to_file","args":{"TargetFile":"\"/workspace/project/b.txt\"","Overwrite":"true"}},` + common + `}`, hooks.ToolUse, []string{"/workspace/project/b.txt"}},
		{"replace wire shape", VerbPreToolUse, `{"toolCall":{"name":"replace_file_content","args":{"TargetFile":"\"c.go\"","ReplacementContent":"\"y\""}},` + common + `}`, hooks.ToolUse, []string{"c.go"}},
		{"multi replace", VerbPreToolUse, `{"toolCall":{"name":"multi_replace_file_content","args":{"TargetFile":"d.go"}},` + common + `}`, hooks.ToolUse, []string{"d.go"}},
		{"write without target", VerbPreToolUse, `{"toolCall":{"name":"write_to_file","args":{"Overwrite":"true"}},` + common + `}`, 0, nil},
		{"stop busy", VerbStop, `{"executionNum":1,"terminationReason":"model_stop","error":"","fullyIdle":false,` + common + `}`, 0, nil},
		{"stop idle", VerbStop, `{"executionNum":1,"terminationReason":"model_stop","error":"","fullyIdle":true,` + common + `}`, hooks.TurnEnd, nil},
		{"post without a step", VerbPostToolUse, `{"toolCall":{"name":"run_command","args":{"CommandLine":"ls"}},"error":"",` + common + `}`, 0, nil},
		{"post without a call", VerbPostToolUse, `{"stepIdx":2,"error":"",` + common + `}`, 0, nil},
		{"unknown verb", "post-invocation", `{"invocationNum":0,` + common + `}`, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ev, err := a.Parse(c.verb, []byte(c.payload))
			if err != nil {
				t.Fatal(err)
			}
			if c.kind == 0 {
				if ev != nil {
					t.Fatalf("event = %+v, want nil", ev)
				}
				return
			}
			if ev == nil {
				t.Fatal("event = nil")
			}
			if ev.Kind != c.kind {
				t.Fatalf("Kind = %v, want %v", ev.Kind, c.kind)
			}
			if ev.SessionID != "ec33ebf9-0cba-4100-8142-c61503f6c587" || !strings.HasSuffix(ev.Transcript, "transcript_full.jsonl") || ev.Cwd != "/workspace/project" {
				t.Fatalf("event = %+v", ev)
			}
			if ev.Prompt != "" || ev.Usage != nil || ev.Model != "gemini-3.1-pro-low" {
				t.Fatalf("event carries prompt/usage or lacks the payload's model: %+v", ev)
			}
			if strings.Join(ev.Files, ",") != strings.Join(c.files, ",") {
				t.Fatalf("Files = %v, want %v", ev.Files, c.files)
			}
		})
	}
	if _, err := a.Parse(VerbStop, []byte(`{not json`)); err == nil {
		t.Fatal("malformed payload should error")
	}
	if ev, err := a.Parse(VerbPreInvocation, nil); err != nil || ev == nil || ev.SessionID != "" || ev.Model != "" {
		t.Fatalf("empty payload = %+v, %v", ev, err)
	}
}

// postToolUse is the payload agy 1.3.3 sends after a tool ran (captured
// 2026-10-12): the call, stepIdx (the index of the step its output will be
// written as), an empty error, and no output.
const postToolUse = `{"artifactDirectoryPath":"/home/u/.gemini/antigravity-cli/brain/ec33ebf9","error":"","stepIdx":%d,"toolCall":{"args":%s,"name":"%s"},` + common + `}`

func TestParsePostToolUse(t *testing.T) {
	a := Agent{}
	writeArgs := `{"Append":false,"CodeContent":"probe","Description":"Create probe.txt","Overwrite":false,"TargetFile":"/workspace/project/probe.txt","toolAction":"Creating probe.txt","toolSummary":"File creation"}`
	ev, err := a.Parse(VerbPostToolUse, []byte(fmt.Sprintf(postToolUse, 2, writeArgs, "write_to_file")))
	if err != nil || ev == nil || ev.Kind != hooks.ToolUse || ev.Tool == nil {
		t.Fatalf("event = %+v, %v", ev, err)
	}
	if ev.SessionID != "ec33ebf9-0cba-4100-8142-c61503f6c587" || ev.Cwd != "/workspace/project" || ev.Model != "gemini-3.1-pro-low" || ev.Prompt != "" || len(ev.Files) != 0 {
		t.Fatalf("event = %+v", ev)
	}
	w := ev.Tool
	if w.NativeID != "1-0" || w.Name != "write_to_file" || w.Kind != "edit" || w.Status != "completed" || w.Result != nil ||
		len(w.Locations) != 1 || w.Locations[0] != "/workspace/project/probe.txt" {
		t.Fatalf("tool = %+v", w)
	}
	var args map[string]any
	if err := json.Unmarshal(w.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["Append"] != false || args["CodeContent"] != "probe" || args["TargetFile"] != "/workspace/project/probe.txt" {
		t.Fatalf("args = %s", w.Arguments)
	}
	if name := record.CanonicalToolName(w.Name, w.Title, w.Kind, w.Arguments); name != "Write" {
		t.Fatalf("canonical name = %q, want Write", name)
	}

	// The same call as the transcript records it: same id, arguments and
	// location, so the dispatcher's keys match and it is not recorded twice.
	path := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	line := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nmake it\n</USER_REQUEST>"}` + "\n" +
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"write_to_file","args":` + writeArgs + `}]}` + "\n" +
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Created"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	turn, _, err := reader{}.Read(path, 0)
	if err != nil || len(turn.Items) != 1 || turn.Items[0].Tool == nil {
		t.Fatalf("Read = %+v, %v", turn, err)
	}
	r := turn.Items[0].Tool
	if r.NativeID != w.NativeID || r.Name != w.Name || r.Kind != w.Kind || !bytes.Equal(r.Arguments, w.Arguments) ||
		strings.Join(r.Locations, ",") != strings.Join(w.Locations, ",") {
		t.Fatalf("hook tool %+v differs from transcript tool %+v", w, r)
	}

	// Two calls in one planner step (seen live: write_to_file and
	// run_command together in step 1, outputs at steps 2 and 3): the hook's
	// stepIdx names the output step, and the id follows the reader's.
	two := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nboth\n</USER_REQUEST>"}` + "\n" +
		`{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"write_to_file","args":` + writeArgs + `},{"name":"run_command","args":{"CommandLine":"cat probe.txt","Cwd":"/workspace/project"}}]}` + "\n"
	if err := os.WriteFile(path, []byte(two), 0o644); err != nil {
		t.Fatal(err)
	}
	withPath := strings.Replace(postToolUse, `"transcriptPath":"/home/u/.gemini/antigravity-cli/brain/ec33ebf9/.system_generated/logs/transcript_full.jsonl"`, `"transcriptPath":%q`, 1)
	for _, c := range []struct {
		stepIdx    int
		name, args string
		id         string
	}{
		{2, "write_to_file", writeArgs, "1-0"},
		{3, "run_command", `{"CommandLine":"cat probe.txt","Cwd":"/workspace/project"}`, "1-1"},
		{4, "run_command", `{"CommandLine":"ls"}`, "3-0"}, // no call at that index: the step before
		{3, "view_file", `{"AbsolutePath":"/x"}`, "2-0"},  // a different call there: the step before
	} {
		ev, err := a.Parse(VerbPostToolUse, []byte(fmt.Sprintf(withPath, c.stepIdx, c.args, c.name, path)))
		if err != nil || ev == nil || ev.Tool == nil || ev.Tool.NativeID != c.id {
			t.Fatalf("stepIdx %d %s: event = %+v, %v; want id %s", c.stepIdx, c.name, ev, err, c.id)
		}
	}
	turn, _, err = reader{}.Read(path, 0)
	if err != nil || len(turn.Items) != 2 || turn.Items[0].Tool.NativeID != "1-0" || turn.Items[1].Tool.NativeID != "1-1" {
		t.Fatalf("reader ids = %+v, %v", turn, err)
	}

	// A shell call: execute kind, wire-shape (double encoded) args undone,
	// and a failure reported in the payload's error.
	raw := fmt.Sprintf(postToolUse, 4, `{"CommandLine":"\"cat probe.txt\"","Cwd":"/workspace/project","IsDaemon":false,"WaitMsBeforeAsync":5000}`, "run_command")
	raw = strings.Replace(raw, `"error":""`, `"error":"exit status 1"`, 1)
	ev, err = a.Parse(VerbPostToolUse, []byte(raw))
	if err != nil || ev == nil || ev.Tool == nil {
		t.Fatalf("event = %+v, %v", ev, err)
	}
	run := ev.Tool
	if run.NativeID != "3-0" || run.Kind != "execute" || run.Status != "failed" || run.Result == nil || *run.Result != "exit status 1" || run.Locations != nil {
		t.Fatalf("tool = %+v", run)
	}
	if err := json.Unmarshal(run.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["CommandLine"] != "cat probe.txt" || args["WaitMsBeforeAsync"] != float64(5000) {
		t.Fatalf("args = %s", run.Arguments)
	}
	if name := record.CanonicalToolName(run.Name, run.Title, run.Kind, run.Arguments); name != "Bash" {
		t.Fatalf("canonical name = %q, want Bash", name)
	}
}

// TestParsePreInvocationPrompt: the first invocation of a turn finds the
// prompt step already in the transcript and opens the turn with it; a
// follow-up invocation, whose transcript ends with the model's steps, does
// not; a transcript that is not there yet gives no prompt either.
func TestParsePreInvocationPrompt(t *testing.T) {
	a := Agent{}
	path := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	payload := func(n int) []byte {
		return []byte(fmt.Sprintf(`{"invocationNum":%d,"initialNumSteps":1,"conversationId":"c1","modelName":"m","workspacePaths":["/w"],"transcriptPath":%q}`, n, path))
	}
	ev, err := a.Parse(VerbPreInvocation, payload(0))
	if err != nil || ev == nil || ev.Kind != hooks.TurnStart || ev.Prompt != "" {
		t.Fatalf("no transcript yet: %+v, %v", ev, err)
	}

	prompt := `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nCreate rt.txt, then run: cat rt.txt\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nnow\n</ADDITIONAL_METADATA>"}` + "\n"
	if err := os.WriteFile(path, []byte(prompt), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, err = a.Parse(VerbPreInvocation, payload(0))
	if err != nil || ev == nil || ev.Kind != hooks.TurnStart || ev.Prompt != "Create rt.txt, then run: cat rt.txt" {
		t.Fatalf("first invocation = %+v, %v", ev, err)
	}
	if ev.SessionID != "c1" || ev.Transcript != path || ev.Cwd != "/w" || ev.Model != "m" {
		t.Fatalf("event = %+v", ev)
	}

	model := `{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","tool_calls":[{"name":"run_command","args":{"CommandLine":"cat rt.txt"}}]}` + "\n" +
		`{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","content":"rt"}` + "\n"
	if err := os.WriteFile(path, []byte(prompt+model), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, err = a.Parse(VerbPreInvocation, payload(1))
	if err != nil || ev == nil || ev.Kind != hooks.TurnStart || ev.Prompt != "" {
		t.Fatalf("follow-up invocation = %+v, %v", ev, err)
	}

	// The next turn's prompt, after a finished one (and agy's bookkeeping
	// step after it), is the pending one.
	next := prompt + model + `{"step_index":3,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nagain\n</USER_REQUEST>"}` + "\n" +
		`{"step_index":4,"source":"SYSTEM","type":"CONVERSATION_HISTORY","status":"DONE"}` + "\n"
	if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
		t.Fatal(err)
	}
	ev, err = a.Parse(VerbPreInvocation, payload(0))
	if err != nil || ev == nil || ev.Prompt != "again" {
		t.Fatalf("second turn = %+v, %v", ev, err)
	}
}
