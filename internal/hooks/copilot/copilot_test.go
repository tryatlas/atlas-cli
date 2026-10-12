package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func TestRegisteredAndNamed(t *testing.T) {
	a, ok := hooks.Get(ID)
	if !ok {
		t.Fatal("copilot is not registered")
	}
	if a.ID() != "copilot" || a.Name() != "GitHub Copilot CLI" || a.Binary() != "copilot" {
		t.Fatalf("%q %q %q", a.ID(), a.Name(), a.Binary())
	}
	if a.Response(hooks.SessionStart, "hello") != nil {
		t.Fatal("Copilot shows nothing: Response must be nil")
	}
	if a.ResumeCommand("abc") != "copilot --resume abc" {
		t.Fatal(a.ResumeCommand("abc"))
	}
	if len(a.Verbs()) != 5 {
		t.Fatal(a.Verbs())
	}
	n, ok := a.(hooks.Noter)
	if !ok || !strings.Contains(n.AddNote(), "trust") || !strings.Contains(n.AddNote(), "COPILOT_ALLOW_ALL=true") {
		t.Fatalf("the add note must say the folder needs trusting: %v %q", ok, n)
	}
}

func TestInstallStateUninstall(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	if a.State(root) != hooks.Absent {
		t.Fatal("expected absent")
	}
	changed, err := a.Install(root, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	path := filepath.Join(root, ".github", "hooks", "atlas.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Version int `json:"version"`
		Hooks   map[string][]struct {
			Type string `json:"type"`
			Bash string `json:"bash"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatal(err)
	}
	if file.Version != 1 || len(file.Hooks) != 5 {
		t.Fatalf("%s", b)
	}
	for ev, verb := range map[string]string{
		"sessionStart": "session-start", "userPromptSubmitted": "user-prompt-submitted",
		"postToolUse": "post-tool-use", "agentStop": "agent-stop", "sessionEnd": "session-end",
	} {
		list := file.Hooks[ev]
		if len(list) != 1 || list[0].Type != "command" {
			t.Fatalf("%s: %+v", ev, list)
		}
		if want := hooks.Wrap("atlas-cli hooks copilot " + verb); list[0].Bash != want {
			t.Fatalf("%s: %q", ev, list[0].Bash)
		}
		if !hooks.Owned(list[0].Bash) {
			t.Fatalf("%s: not recognised as Atlas's", ev)
		}
	}
	if strings.Contains(string(b), `"command":`) {
		t.Fatal("Copilot wants the bash key, not command")
	}
	if a.State(root) != hooks.Current {
		t.Fatal("expected current")
	}
	if changed, err := a.Install(root, false); err != nil || changed {
		t.Fatalf("second install changed=%v err=%v", changed, err)
	}
	if changed, err := a.Install(root, true); err != nil || !changed {
		t.Fatalf("forced install changed=%v err=%v", changed, err)
	}
	// An older Atlas version's file is outdated and rewritten.
	if err := os.WriteFile(path, []byte(`{"version":1,"hooks":{"agentStop":[{"type":"command","bash":"atlas-cli hooks copilot stop"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if a.State(root) != hooks.Outdated {
		t.Fatal("expected outdated")
	}
	if changed, err := a.Install(root, false); err != nil || !changed {
		t.Fatalf("upgrade changed=%v err=%v", changed, err)
	}
	if a.State(root) != hooks.Current {
		t.Fatal("expected current after upgrade")
	}
	// Uninstall removes the file and the directory it leaves empty; a
	// sibling hook file keeps the directory.
	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("empty hooks directory should be gone")
	}
	if err := a.Uninstall(root); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
	if _, err := a.Install(root, false); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, ".github", "hooks", "team.json")
	if err := os.WriteFile(other, []byte(`{"version":1,"hooks":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("the user's own hook file must survive")
	}
	if a.State(root) != hooks.Absent {
		t.Fatal("expected absent after uninstall")
	}
}

func TestDetect(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no copilot binary
	root := t.TempDir()
	a := Agent{}
	if a.Detect(root) {
		t.Fatal("nothing to detect")
	}
	if err := os.MkdirAll(filepath.Join(root, ".github", "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !a.Detect(root) {
		t.Fatal(".github/hooks should be detected")
	}
}

func TestParseEachVerb(t *testing.T) {
	t.Setenv("COPILOT_HOME", "/copilot-home")
	a := Agent{}
	const id = "b0ff98c0-8e01-4b73-bf92-9649b139931b"
	derived := filepath.Join("/copilot-home", "session-state", id, "events.jsonl")

	ev, err := a.Parse(VerbPromptSubmitted, []byte(`{"timestamp":1771480081360,"cwd":"/repo","sessionId":"`+id+`","prompt":"hi"}`))
	if err != nil || ev == nil {
		t.Fatal(err)
	}
	if ev.Kind != hooks.TurnStart || ev.SessionID != id || ev.Prompt != "hi" || ev.Cwd != "/repo" {
		t.Fatalf("%+v", ev)
	}
	if ev.Transcript != derived {
		t.Fatalf("turn start must derive the transcript from the session id: %q", ev.Transcript)
	}

	ev, err = a.Parse(VerbSessionStart, []byte(`{"timestamp":1771480081383,"cwd":"/repo","sessionId":"`+id+`","source":"new","initialPrompt":"hi"}`))
	if err != nil || ev == nil || ev.Kind != hooks.SessionStart || ev.SessionID != id || ev.Prompt != "" || ev.Transcript != derived {
		t.Fatalf("%+v %v", ev, err)
	}

	ev, err = a.Parse(VerbAgentStop, []byte(`{"timestamp":1771480085412,"cwd":"/repo","sessionId":"`+id+`","transcriptPath":"/home/u/.copilot/session-state/`+id+`/events.jsonl","stopReason":"end_turn"}`))
	if err != nil || ev == nil || ev.Kind != hooks.TurnEnd || ev.Transcript != "/home/u/.copilot/session-state/"+id+"/events.jsonl" || ev.TurnEndFailed {
		t.Fatalf("%+v %v", ev, err)
	}

	ev, err = a.Parse(VerbSessionEnd, []byte(`{"timestamp":1771480085425,"cwd":"/repo","sessionId":"`+id+`","reason":"complete"}`))
	if err != nil || ev == nil || ev.Kind != hooks.SessionEnd || ev.SessionID != id {
		t.Fatalf("%+v %v", ev, err)
	}

	// snake_case keys (VS Code's hook host) are accepted.
	ev, err = a.Parse(VerbAgentStop, []byte(`{"session_id":"`+id+`","transcript_path":"/t/events.jsonl","cwd":"/repo"}`))
	if err != nil || ev == nil || ev.SessionID != id || ev.Transcript != "/t/events.jsonl" {
		t.Fatalf("%+v %v", ev, err)
	}

	// An unusable session id derives nothing.
	ev, err = a.Parse(VerbPromptSubmitted, []byte(`{"sessionId":"../x","prompt":"p"}`))
	if err != nil || ev == nil || ev.Transcript != "" {
		t.Fatalf("%+v %v", ev, err)
	}

	// An empty payload and an unknown verb.
	if ev, err := a.Parse(VerbSessionEnd, nil); err != nil || ev == nil || ev.Kind != hooks.SessionEnd {
		t.Fatalf("%+v %v", ev, err)
	}
	if ev, err := a.Parse("pre-tool-use", []byte(`{}`)); err != nil || ev != nil {
		t.Fatalf("%+v %v", ev, err)
	}
	if _, err := a.Parse(VerbSessionEnd, []byte(`not json`)); err == nil {
		t.Fatal("bad JSON should error")
	}
}

func TestTranscriptPathDefaultsToTheHomeDirectory(t *testing.T) {
	t.Setenv("COPILOT_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	if got, want := TranscriptPath("s1"), filepath.Join(home, ".copilot", "session-state", "s1", "events.jsonl"); got != want {
		t.Fatalf("%q != %q", got, want)
	}
}

// The postToolUse payloads below are what Copilot CLI 1.0.95 sent to an echo
// hook: no tool call id; toolArgs an object for bash and MCP tools, the bare
// patch string for apply_patch; toolResult {resultType, textResultForLlm}.
func TestParsePostToolUse(t *testing.T) {
	t.Setenv("COPILOT_HOME", "/copilot-home")
	a := Agent{}
	const id = "c90a3853-49f2-4828-9293-c24163db8c36"
	derived := filepath.Join("/copilot-home", "session-state", id, "events.jsonl")

	shell := `{"sessionId":"` + id + `","timestamp":1791759613789,"cwd":"/private/tmp/hooks-copilot","toolName":"bash","toolArgs":{"command":"cat echo2.txt","description":"Read echo2.txt"},"toolResult":{"resultType":"success","textResultForLlm":"hello\n<shellId: 0 completed with exit code 0>"}}`
	ev, err := a.Parse(VerbPostToolUse, []byte(shell))
	if err != nil || ev == nil || ev.Kind != hooks.ToolUse || ev.SessionID != id || ev.Cwd != "/private/tmp/hooks-copilot" || ev.Transcript != derived {
		t.Fatalf("%+v %v", ev, err)
	}
	tool := ev.Tool
	if tool == nil || tool.Name != "bash" || tool.Status != "completed" || len(tool.Locations) != 0 ||
		string(tool.Arguments) != `{"command":"cat echo2.txt","description":"Read echo2.txt"}` ||
		tool.Result == nil || *tool.Result != "hello\n<shellId: 0 completed with exit code 0>" {
		t.Fatalf("%+v", tool)
	}
	if !strings.HasPrefix(tool.NativeID, "hook-") || len(tool.NativeID) != len("hook-")+24 {
		t.Fatalf("the payload has no call id; a derived one is expected: %q", tool.NativeID)
	}
	if record.CanonicalToolName(tool.Name, tool.Title, tool.Kind, tool.Arguments) != "Bash" {
		t.Fatal("bash must record as Bash")
	}
	// The same payload derives the same id; another timestamp another one.
	again, _ := a.Parse(VerbPostToolUse, []byte(shell))
	if again.Tool.NativeID != tool.NativeID {
		t.Fatalf("%q != %q", again.Tool.NativeID, tool.NativeID)
	}
	later, _ := a.Parse(VerbPostToolUse, []byte(strings.Replace(shell, "1791759613789", "1791759613790", 1)))
	if later.Tool.NativeID == tool.NativeID {
		t.Fatal("two calls at different times must not share an id")
	}

	patch := `{"sessionId":"` + id + `","timestamp":1791759612107,"cwd":"/private/tmp/hooks-copilot","toolName":"apply_patch","toolArgs":"*** Begin Patch\n*** Add File: echo2.txt\n+hello\n*** End Patch\n","toolResult":{"resultType":"success","textResultForLlm":"Added 1 file(s): /private/tmp/hooks-copilot/echo2.txt"}}`
	ev, err = a.Parse(VerbPostToolUse, []byte(patch))
	if err != nil || ev == nil || ev.Tool == nil {
		t.Fatalf("%+v %v", ev, err)
	}
	tool = ev.Tool
	if tool.Name != "apply_patch" || tool.Status != "completed" ||
		string(tool.Arguments) != `{"patch":"*** Begin Patch\n*** Add File: echo2.txt\n+hello\n*** End Patch\n"}` ||
		tool.Result == nil || *tool.Result != "Added 1 file(s): /private/tmp/hooks-copilot/echo2.txt" {
		t.Fatalf("%+v", tool)
	}
	if len(tool.Locations) != 1 || tool.Locations[0] != "/private/tmp/hooks-copilot/echo2.txt" {
		t.Fatalf("the patch's file, absolute like the transcript's fileEdits: %v", tool.Locations)
	}
	if record.CanonicalToolName(tool.Name, tool.Title, tool.Kind, tool.Arguments) != "Edit" {
		t.Fatal("apply_patch must record as Edit")
	}
	if paths := record.WritePaths(tool.Arguments, tool.Locations); len(paths) != 1 || paths[0] != "/private/tmp/hooks-copilot/echo2.txt" {
		t.Fatalf("the recorder must sample the patched file: %v", paths)
	}
	if record.EditPatch(tool.Arguments, tool.Locations[0]) != "*** Begin Patch\n*** Add File: echo2.txt\n+hello\n*** End Patch\n" {
		t.Fatal("the patch must be kept as the edit's patch")
	}

	mcp := `{"sessionId":"` + id + `","timestamp":1791759578328,"cwd":"/private/tmp/hooks-copilot","toolName":"atlas_memory-memory_briefing","toolArgs":{},"toolResult":{"resultType":"success","textResultForLlm":"{\"filesChanged\":[]}"}}`
	ev, err = a.Parse(VerbPostToolUse, []byte(mcp))
	if err != nil || ev == nil || ev.Tool == nil || ev.Tool.Name != "atlas_memory-memory_briefing" || string(ev.Tool.Arguments) != `{}` ||
		ev.Tool.Result == nil || *ev.Tool.Result != `{"filesChanged":[]}` || len(ev.Tool.Locations) != 0 {
		t.Fatalf("%+v %v", ev, err)
	}

	// A result that is not a success fails the call; a string or absent
	// result is kept as is; a string argument that is no patch is wrapped.
	ev, _ = a.Parse(VerbPostToolUse, []byte(`{"sessionId":"`+id+`","timestamp":1,"toolName":"bash","toolArgs":{"command":"rm -rf /"},"toolResult":{"resultType":"denied","textResultForLlm":"The user denied this tool call."}}`))
	if ev.Tool.Status != "failed" || *ev.Tool.Result != "The user denied this tool call." {
		t.Fatalf("%+v", ev.Tool)
	}
	ev, _ = a.Parse(VerbPostToolUse, []byte(`{"sessionId":"`+id+`","timestamp":2,"toolName":"view","toolArgs":"x.txt","toolResult":"hello"}`))
	if ev.Tool.Status != "completed" || *ev.Tool.Result != "hello" || string(ev.Tool.Arguments) != `{"input":"x.txt"}` || ev.Tool.Locations != nil {
		t.Fatalf("%+v", ev.Tool)
	}
	ev, _ = a.Parse(VerbPostToolUse, []byte(`{"sessionId":"`+id+`","timestamp":3,"toolName":"think"}`))
	if ev.Tool.Result != nil || ev.Tool.Arguments != nil || ev.Tool.Status != "completed" {
		t.Fatalf("%+v", ev.Tool)
	}

	// No tool named: nothing to do.
	if ev, err := a.Parse(VerbPostToolUse, []byte(`{"sessionId":"`+id+`","timestamp":4}`)); err != nil || ev != nil {
		t.Fatalf("%+v %v", ev, err)
	}
}

func TestPatchPaths(t *testing.T) {
	patch, _ := json.Marshal("*** Begin Patch\n*** Update File: src/a.go\n*** Move to: src/b.go\n@@\n-x\n+y\n*** Delete File: old.txt\n*** Add File: /abs/new.txt\n+n\n*** Update File: src/a.go\n*** End Patch\n")
	got := patchPaths(patch, "/repo")
	want := []string{"/repo/src/a.go", "/repo/src/b.go", "/repo/old.txt", "/abs/new.txt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%v", got)
	}
	if got := patchPaths(patch, ""); got[0] != "src/a.go" {
		t.Fatalf("no cwd keeps the path relative: %v", got)
	}
	if patchPaths([]byte(`{"path":"x"}`), "/repo") != nil || patchPaths([]byte(`"not a patch"`), "/repo") != nil {
		t.Fatal("only a patch string names files")
	}
}
