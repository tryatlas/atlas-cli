package antigravity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	var e struct {
		Enabled    *bool `json:"enabled"`
		PreToolUse []struct {
			Matcher string              `json:"matcher"`
			Hooks   []hooks.CommandHook `json:"hooks"`
		} `json:"PreToolUse"`
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
		e.PreToolUse[0].Hooks[0].Command != hooks.Wrap("atlas hooks antigravity pre-tool-use") ||
		e.PreToolUse[0].Hooks[0].Type != "command" {
		t.Fatalf("PreToolUse = %+v", e.PreToolUse)
	}
	if len(e.PreInvocation) != 1 || e.PreInvocation[0].Command != hooks.Wrap("atlas hooks antigravity pre-invocation") {
		t.Fatalf("PreInvocation = %+v", e.PreInvocation)
	}
	if len(e.Stop) != 1 || e.Stop[0].Command != hooks.Wrap("atlas hooks antigravity stop") || e.Stop[0].Timeout != 300 {
		t.Fatalf("Stop = %+v", e.Stop)
	}
	for _, h := range []string{e.PreToolUse[0].Hooks[0].Command, e.PreInvocation[0].Command, e.Stop[0].Command} {
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
  "atlas": {"enabled": false, "Stop": [{"type": "command", "command": "sh -c 'exec atlas hooks antigravity old-verb'"}]}
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
	if len(e.Stop) != 1 || e.Stop[0].Command != hooks.Wrap("atlas hooks antigravity stop") {
		t.Fatalf("old Stop hook survived: %+v", e.Stop)
	}
	if len(e.PreToolUse) != 1 || len(e.PreInvocation) != 1 {
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
	e.PostInvocation = []plainHandler{{Type: "command", Command: hooks.Wrap("atlas hooks antigravity post-invocation")}}
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
		{"unknown verb", "post-tool-use", `{` + common + `}`, 0, nil},
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
