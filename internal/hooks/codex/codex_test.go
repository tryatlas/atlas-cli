package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

type hooksFile struct {
	Hooks map[string][]struct {
		Matcher *string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
			Timeout int    `json:"timeout"`
		} `json:"hooks"`
	} `json:"hooks"`
}

func readHooks(t *testing.T, root string) (hooksFile, map[string]json.RawMessage) {
	t.Helper()
	b, err := os.ReadFile(HooksPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var f hooksFile
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	_ = json.Unmarshal(b, &top)
	return f, top
}

func TestInstallIntoEmptyDir(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	if a.State(root) != hooks.Absent {
		t.Fatal("expected absent")
	}
	changed, err := a.Install(root, false)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	f, _ := readHooks(t, root)
	want := map[string]int{"SessionStart": 30, "SessionEnd": 3, "UserPromptSubmit": 30, "Stop": 30, "PostToolUse": 30}
	if len(f.Hooks) != len(want) {
		t.Fatalf("types: %v", f.Hooks)
	}
	for typ, timeout := range want {
		groups := f.Hooks[typ]
		if len(groups) != 1 || groups[0].Matcher != nil || len(groups[0].Hooks) != 1 {
			t.Fatalf("%s: %+v", typ, groups)
		}
		h := groups[0].Hooks[0]
		if h.Type != "command" || h.Timeout != timeout || !hooks.Owned(h.Command) || !strings.Contains(h.Command, "atlas-cli hooks codex ") {
			t.Fatalf("%s: %+v", typ, h)
		}
	}
	if !strings.Contains(f.Hooks["SessionStart"][0].Hooks[0].Command, "systemMessage") {
		t.Fatal("SessionStart should print the missing-atlas message")
	}
	if a.State(root) != hooks.Current {
		t.Fatalf("state %v", a.State(root))
	}
	if changed, err := a.Install(root, false); err != nil || changed {
		t.Fatalf("second install changed=%v err=%v", changed, err)
	}
	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	_, top := readHooks(t, root)
	if _, ok := top["hooks"]; ok {
		t.Fatalf("hooks left after uninstall: %s", top["hooks"])
	}
	if a.State(root) != hooks.Absent {
		t.Fatal("expected absent after uninstall")
	}
}

func TestInstallKeepsUserHookAndPrunesStale(t *testing.T) {
	root := t.TempDir()
	stale := hooks.Wrap(hooks.Command(ID, "old-stop"))
	existing := `{
  "notifications": true,
  "hooks": {
    "Stop": [{"matcher": null, "hooks": [
      {"type": "command", "command": "echo done", "timeout": 5},
      {"type": "command", "command": ` + string(hooks.Raw(stale)) + `, "timeout": 30}
    ]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "my-lint"}]}]
  }
}`
	if err := os.MkdirAll(filepath.Dir(HooksPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HooksPath(root), []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}
	a := Agent{}
	if a.State(root) != hooks.Outdated {
		t.Fatalf("state %v, want outdated for a stale Atlas entry", a.State(root))
	}
	if changed, err := a.Install(root, false); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	f, top := readHooks(t, root)
	if string(top["notifications"]) != "true" {
		t.Fatal("unrelated key lost")
	}
	stop := f.Hooks["Stop"]
	if len(stop) != 1 || len(stop[0].Hooks) != 2 {
		t.Fatalf("Stop: %+v", stop)
	}
	if stop[0].Hooks[0].Command != "echo done" || stop[0].Hooks[0].Timeout != 5 {
		t.Fatalf("user hook changed: %+v", stop[0].Hooks[0])
	}
	if stop[0].Hooks[1].Command != hooks.Wrap(hooks.Command(ID, VerbStop)) {
		t.Fatalf("Atlas Stop entry: %+v", stop[0].Hooks[1])
	}
	for _, g := range stop {
		for _, h := range g.Hooks {
			if h.Command == stale {
				t.Fatal("stale entry survived")
			}
		}
	}
	pre := f.Hooks["PreToolUse"]
	if len(pre) != 1 || pre[0].Matcher == nil || *pre[0].Matcher != "Bash" || pre[0].Hooks[0].Command != "my-lint" {
		t.Fatalf("PreToolUse: %+v", pre)
	}
	if a.State(root) != hooks.Current {
		t.Fatalf("state %v", a.State(root))
	}
	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	f, _ = readHooks(t, root)
	if len(f.Hooks) != 2 || len(f.Hooks["Stop"][0].Hooks) != 1 || f.Hooks["Stop"][0].Hooks[0].Command != "echo done" {
		t.Fatalf("after uninstall: %+v", f.Hooks)
	}
}

func TestParse(t *testing.T) {
	a := Agent{}
	common := `"session_id":"s1","transcript_path":"/t/rollout.jsonl","cwd":"/repo","model":"gpt-5-codex"`

	ev, err := a.Parse(VerbSessionStart, []byte(`{`+common+`,"hook_event_name":"SessionStart","source":"startup"}`))
	if err != nil || ev == nil || ev.Kind != hooks.SessionStart || ev.SessionID != "s1" || ev.Transcript != "/t/rollout.jsonl" || ev.Cwd != "/repo" || ev.Model != "gpt-5-codex" {
		t.Fatalf("%+v %v", ev, err)
	}

	ev, err = a.Parse(VerbPromptSubmit, []byte(`{`+common+`,"turn_id":"t1","prompt":"Create hello.txt"}`))
	if err != nil || ev == nil || ev.Kind != hooks.TurnStart || ev.Prompt != "Create hello.txt" {
		t.Fatalf("%+v %v", ev, err)
	}

	ev, err = a.Parse(VerbStop, []byte(`{`+common+`,"turn_id":"t1","stop_hook_active":true,"last_assistant_message":null}`))
	if err != nil || ev == nil || ev.Kind != hooks.TurnEnd || ev.TurnEndFailed {
		t.Fatalf("%+v %v", ev, err)
	}

	// SessionEnd carries no model; a null transcript (--ephemeral) decodes to "".
	ev, err = a.Parse(VerbSessionEnd, []byte(`{"session_id":"s1","transcript_path":null,"cwd":"/repo","hook_event_name":"SessionEnd","reason":"other"}`))
	if err != nil || ev == nil || ev.Kind != hooks.SessionEnd || ev.Transcript != "" || ev.Model != "" {
		t.Fatalf("%+v %v", ev, err)
	}

	// apply_patch: the call is shaped like the rollout's (transcript.go
	// patchTool) so the dispatcher's dedupe matches by arguments and by the
	// written paths; Files are absolute for the git attribution.
	patch := "*** Begin Patch\n*** Add File: docs/new.md\n+hi\n*** Update File: main.go\n@@\n-a\n+b\n*** Delete File: /abs/old.txt\n*** End Patch"
	ev, err = a.Parse(VerbPostToolUse, []byte(`{`+common+`,"tool_name":"apply_patch","tool_use_id":"exec-1","tool_input":{"command":`+string(hooks.Raw(patch))+`},"tool_response":"Exit code: 0\nOutput:\nSuccess."}`))
	if err != nil || ev == nil || ev.Kind != hooks.ToolUse || ev.ToolUseID != "exec-1" || ev.Tool == nil {
		t.Fatalf("%+v %v", ev, err)
	}
	if want := []string{"/repo/docs/new.md", "/repo/main.go", "/abs/old.txt"}; strings.Join(ev.Files, ",") != strings.Join(want, ",") {
		t.Fatalf("files %v", ev.Files)
	}
	want := patchTool("exec-1", patch)
	if ev.Tool.NativeID != want.NativeID || ev.Tool.Name != want.Name || string(ev.Tool.Arguments) != string(want.Arguments) || strings.Join(ev.Tool.Locations, ",") != strings.Join(want.Locations, ",") || ev.Tool.Status != "completed" {
		t.Fatalf("got %+v\nwant %+v", ev.Tool, want)
	}
	if ev.Tool.Result == nil || *ev.Tool.Result != "Exit code: 0\nOutput:\nSuccess." {
		t.Fatalf("result %v", ev.Tool.Result)
	}
	if record.CanonicalToolName(ev.Tool.Name, "", "", ev.Tool.Arguments) != "Edit" {
		t.Fatal("apply_patch should canonicalise to Edit")
	}

	// The Write and Edit aliases are the same tool; without a tool_use_id
	// there is no call to deliver, but the files still count.
	ev, err = a.Parse(VerbPostToolUse, []byte(`{`+common+`,"tool_name":"Edit","tool_input":{"patch":"*** Begin Patch\n*** Update File: a.go\n*** End Patch"}}`))
	if err != nil || ev == nil || ev.Kind != hooks.ToolUse || ev.Tool != nil || len(ev.Files) != 1 || ev.Files[0] != "/repo/a.go" {
		t.Fatalf("%+v %v", ev, err)
	}

	// A shell call (Codex reports a code-mode exec_command as Bash with the
	// command text): the call is delivered with its arguments re-encoded
	// and an object response kept as text; no files.
	ev, err = a.Parse(VerbPostToolUse, []byte(`{`+common+`,"tool_name":"Bash","tool_use_id":"exec-2","tool_input":{"workdir":"/repo","command":"cat rt.txt"},"tool_response":{"output":"realtime\n","exit_code":0}}`))
	if err != nil || ev == nil || ev.Kind != hooks.ToolUse || ev.Tool == nil || len(ev.Files) != 0 {
		t.Fatalf("%+v %v", ev, err)
	}
	if ev.Tool.NativeID != "exec-2" || ev.Tool.Name != "Bash" || string(ev.Tool.Arguments) != `{"command":"cat rt.txt","workdir":"/repo"}` || ev.Tool.Status != "completed" {
		t.Fatalf("%+v", ev.Tool)
	}
	if ev.Tool.Result == nil || *ev.Tool.Result != `{"output":"realtime\n","exit_code":0}` {
		t.Fatalf("result %v", ev.Tool.Result)
	}
	// The same command in the rollout's code-mode script yields the same
	// command text, which is the dispatcher's shell key.
	args, ok := CodeModeCommand(`const r = await tools.exec_command({"cmd":"cat rt.txt","workdir":"/repo","yield_time_ms":10000});` + "\ntext(r.output);\n")
	if !ok {
		t.Fatal("expected a lone exec_command")
	}
	var hookArgs, rolloutArgs struct {
		Command string `json:"command"`
		Cmd     string `json:"cmd"`
	}
	if json.Unmarshal(ev.Tool.Arguments, &hookArgs) != nil || json.Unmarshal(args, &rolloutArgs) != nil || hookArgs.Command+hookArgs.Cmd != rolloutArgs.Command+rolloutArgs.Cmd {
		t.Fatalf("hook %s, rollout %s", ev.Tool.Arguments, args)
	}

	// An MCP or other tool: delivered as named, a null response is no result.
	ev, err = a.Parse(VerbPostToolUse, []byte(`{`+common+`,"tool_name":"mcp__atlas_memory__memory_search","tool_use_id":"exec-3","tool_input":{"query":"hooks"},"tool_response":null}`))
	if err != nil || ev == nil || ev.Tool == nil || ev.Tool.Name != "mcp__atlas_memory__memory_search" || string(ev.Tool.Arguments) != `{"query":"hooks"}` || ev.Tool.Result != nil || len(ev.Files) != 0 {
		t.Fatalf("%+v %v", ev, err)
	}

	// An apply_patch whose envelope names no file is still a call, just not
	// an edit.
	ev, err = a.Parse(VerbPostToolUse, []byte(`{`+common+`,"tool_name":"apply_patch","tool_use_id":"exec-4","tool_input":{"command":"garbage"}}`))
	if err != nil || ev == nil || ev.Tool == nil || ev.Tool.Name != "apply_patch" || string(ev.Tool.Arguments) != `{"command":"garbage"}` || len(ev.Tool.Locations) != 0 || len(ev.Files) != 0 {
		t.Fatalf("%+v %v", ev, err)
	}

	// Nothing to deliver: no id and no files.
	ev, err = a.Parse(VerbPostToolUse, []byte(`{`+common+`,"tool_name":"shell","tool_input":{"command":["ls"]}}`))
	if err != nil || ev != nil {
		t.Fatalf("%+v %v", ev, err)
	}

	ev, err = a.Parse("pre-compact", []byte(`{`+common+`}`))
	if err != nil || ev != nil {
		t.Fatalf("unknown verb: %+v %v", ev, err)
	}

	if _, err := a.Parse(VerbStop, []byte(`{not json`)); err == nil {
		t.Fatal("expected a decode error")
	}
}

func TestPatchFiles(t *testing.T) {
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: a.txt",
		"+one",
		"*** Update File: b.go",
		"*** Move to: c.go",
		"@@",
		"-x",
		"+y",
		"*** Delete File: d.txt",
		"*** Update File: a.txt",
		"+two",
		"*** End Patch",
	}, "\n")
	got := PatchFiles(patch)
	if want := "a.txt,b.go,c.go,d.txt"; strings.Join(got, ",") != want {
		t.Fatalf("got %v want %s", got, want)
	}
	if PatchFiles("") != nil || PatchFiles("+ not an envelope") != nil {
		t.Fatal("expected nothing")
	}
}

func TestResponseAndResume(t *testing.T) {
	a := Agent{}
	if got := string(a.Response(hooks.SessionStart, "hello")); got != `{"systemMessage":"hello"}`+"\n" {
		t.Fatalf("%q", got)
	}
	if a.Response(hooks.TurnEnd, "hello") != nil || a.Response(hooks.SessionStart, "") != nil {
		t.Fatal("only SessionStart with a message prints")
	}
	if a.ResumeCommand("abc") != "codex resume abc" {
		t.Fatal(a.ResumeCommand("abc"))
	}
	if a.ID() != "codex" || a.Name() != "Codex" || a.Binary() != "codex" || len(a.Verbs()) != 5 {
		t.Fatal("identity")
	}
	if got, ok := hooks.Get(ID); !ok || got.ID() != ID {
		t.Fatal("not registered")
	}
}

func TestDetect(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !(Agent{}).Detect(root) {
		t.Fatal("a .codex directory should be detected")
	}
}

func TestCodeModePatches(t *testing.T) {
	// The real shape Codex 0.153 writes in code mode: one JS string literal.
	script := `const r = await tools.apply_patch("*** Begin Patch\n*** Add File: /private/tmp/hooks-codex/note2.txt\n+hello\n*** End Patch"); text(typeof r === "string" ? r : JSON.stringify(r));`
	patches, others := CodeModePatches(script)
	if others || len(patches) != 1 || patches[0] != "*** Begin Patch\n*** Add File: /private/tmp/hooks-codex/note2.txt\n+hello\n*** End Patch" {
		t.Fatalf("%q %v", patches, others)
	}

	// Single quotes, a template literal, escapes, and another tool in the
	// same script.
	script = "await tools.shell({command: 'ls'});\n" +
		`tools.apply_patch('*** Begin Patch\n*** Update File: a.go\n+say \"hi\" é \x41 \u{1F600}\n*** End Patch');` + "\n" +
		"tools.apply_patch(`*** Begin Patch\\n*** Delete File: b.txt\\n*** End Patch`);"
	patches, others = CodeModePatches(script)
	if !others || len(patches) != 2 {
		t.Fatalf("%q %v", patches, others)
	}
	if want := "*** Begin Patch\n*** Update File: a.go\n+say \"hi\" é A 😀\n*** End Patch"; patches[0] != want {
		t.Fatalf("got %q want %q", patches[0], want)
	}
	if PatchFiles(patches[1])[0] != "b.txt" {
		t.Fatalf("%q", patches[1])
	}

	// Not decodable: a template with an interpolation, an unterminated
	// literal, a non-literal argument, and a patch naming no file.
	for _, bad := range []string{
		"tools.apply_patch(`*** Begin Patch\\n*** Add File: ${name}\\n*** End Patch`)",
		`tools.apply_patch("*** Begin Patch\n*** Add File: a`,
		`tools.apply_patch(patch)`,
		`tools.apply_patch("nothing here")`,
	} {
		if patches, others := CodeModePatches(bad); len(patches) != 0 || !others {
			t.Fatalf("%s: %q %v", bad, patches, others)
		}
	}
	if patches, others := CodeModePatches("console.log('no tools')"); patches != nil || others {
		t.Fatal("expected nothing")
	}
}

func TestParseCodeModeExec(t *testing.T) {
	// A PostToolUse for the exec tool itself, carrying the code-mode script.
	common := `"session_id":"s1","transcript_path":"/t/rollout.jsonl","cwd":"/repo","model":"gpt-5-codex"`
	patch := "*** Begin Patch\n*** Add File: note.txt\n+hi\n*** End Patch"
	script := `await tools.apply_patch(` + string(hooks.Raw(patch)) + `);`

	// A script with one patch and nothing else is that apply_patch call,
	// shaped like the rollout's.
	ev, err := Agent{}.Parse(VerbPostToolUse, []byte(`{`+common+`,"tool_name":"exec","tool_use_id":"exec-1","tool_input":{"input":`+string(hooks.Raw(script))+`}}`))
	if err != nil || ev == nil || ev.Kind != hooks.ToolUse || ev.ToolUseID != "exec-1" || len(ev.Files) != 1 || ev.Files[0] != "/repo/note.txt" {
		t.Fatalf("%+v %v", ev, err)
	}
	want := patchTool("exec-1", patch)
	if ev.Tool == nil || ev.Tool.NativeID != "exec-1" || ev.Tool.Name != "apply_patch" || string(ev.Tool.Arguments) != string(want.Arguments) || strings.Join(ev.Tool.Locations, ",") != "note.txt" {
		t.Fatalf("got %+v\nwant %+v", ev.Tool, want)
	}

	// A script that also runs a command stays the exec call, in the
	// rollout's {"input": script} shape, and still names the patched files.
	mixed := `await tools.shell({command: 'ls'}); ` + script + ` await tools.apply_patch("*** Begin Patch\n*** Delete File: old.txt\n*** End Patch");`
	ev, err = Agent{}.Parse(VerbPostToolUse, []byte(`{`+common+`,"tool_name":"exec","tool_use_id":"exec-2","tool_input":{"input":`+string(hooks.Raw(mixed))+`}}`))
	if err != nil || ev == nil || ev.Tool == nil || ev.Tool.Name != "exec" || string(ev.Tool.Arguments) != string(hooks.Raw(map[string]string{"input": mixed})) {
		t.Fatalf("%+v %v", ev, err)
	}
	if strings.Join(ev.Files, ",") != "/repo/note.txt,/repo/old.txt" {
		t.Fatalf("files %v", ev.Files)
	}

	// A script that only runs a shell command is the exec call with no files.
	ev, err = Agent{}.Parse(VerbPostToolUse, []byte(`{`+common+`,"tool_name":"exec","tool_use_id":"exec-3","tool_input":{"input":"await tools.shell({command: 'ls'})"}}`))
	if err != nil || ev == nil || ev.Tool == nil || ev.Tool.Name != "exec" || len(ev.Files) != 0 {
		t.Fatalf("%+v %v", ev, err)
	}
	// Without a tool_use_id a shell-only script is nothing.
	ev, err = Agent{}.Parse(VerbPostToolUse, []byte(`{`+common+`,"tool_name":"exec","tool_input":{"input":"await tools.shell({command: 'ls'})"}}`))
	if err != nil || ev != nil {
		t.Fatalf("%+v %v", ev, err)
	}
}

func TestCodeModeCommand(t *testing.T) {
	args, ok := CodeModeCommand(`const r = await tools.exec_command({"cmd":"cat rt.txt","workdir":"/repo","yield_time_ms":10000,"max_output_tokens":2000});` + "\ntext(r.output);\n")
	if !ok || string(args) != `{"cmd":"cat rt.txt","max_output_tokens":2000,"workdir":"/repo","yield_time_ms":10000}` {
		t.Fatalf("%s %v", args, ok)
	}
	for _, bad := range []string{
		`tools.exec_command({cmd: 'ls'})`, // a JS literal, not JSON
		`tools.exec_command({"cmd":""})`,
		`tools.exec_command({"workdir":"/repo"})`,
		`tools.exec_command(args)`,
		`tools.exec_command({"cmd":"ls"}); tools.exec_command({"cmd":"pwd"})`,
		`tools.shell({"command":"ls"})`,
		`tools.exec_command({"cmd":"ls"}); tools.apply_patch("*** Begin Patch\n*** Add File: a\n*** End Patch")`,
		"",
	} {
		if args, ok := CodeModeCommand(bad); ok {
			t.Fatalf("%s: %s", bad, args)
		}
	}
}
