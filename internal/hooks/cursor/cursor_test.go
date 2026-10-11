package cursor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func readHooks(t *testing.T, root string) (map[string]json.RawMessage, map[string][]map[string]string) {
	t.Helper()
	b, err := os.ReadFile(HooksPath(root))
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatal(err)
	}
	hs := map[string][]map[string]string{}
	if raw, ok := obj["hooks"]; ok {
		if err := json.Unmarshal(raw, &hs); err != nil {
			t.Fatal(err)
		}
	}
	return obj, hs
}

func TestInstallIntoEmptyProject(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	if a.State(root) != hooks.Absent {
		t.Fatal("state before install")
	}
	changed, err := a.Install(root, false)
	if err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	obj, hs := readHooks(t, root)
	if string(obj["version"]) != "1" {
		t.Fatalf("version %s", obj["version"])
	}
	if len(hs) != 7 {
		t.Fatalf("events %v", hs)
	}
	for _, h := range installed {
		list := hs[h.Event]
		if len(list) != 1 || list[0]["command"] != hooks.Wrap("atlas hooks cursor "+h.Verb) {
			t.Fatalf("%s: %v", h.Event, list)
		}
		if _, ok := list[0]["matcher"]; ok {
			t.Fatalf("%s carries a matcher", h.Event)
		}
	}
	if a.State(root) != hooks.Current {
		t.Fatal("state after install")
	}
	if changed, err := a.Install(root, false); err != nil || changed {
		t.Fatalf("second install: %v %v", changed, err)
	}
	fi, _ := os.Stat(HooksPath(root))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestInstallKeepsUserHooksAndPrunesStale(t *testing.T) {
	root := t.TempDir()
	existing := `{
  "version": 1,
  "other": {"keep": true},
  "hooks": {
    "sessionStart": [
      {"command": "echo mine", "matcher": "x"},
      {"command": "` + hooks.Wrap("atlas hooks cursor old-verb") + `"}
    ],
    "afterFileEdit": [{"command": "./lint.sh"}],
    "preToolUse": [{"command": "atlas hooks cursor pre-tool-use"}]
  }
}`
	if err := os.MkdirAll(filepath.Dir(HooksPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HooksPath(root), []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	a := Agent{}
	if a.State(root) != hooks.Outdated {
		t.Fatal("a stale Atlas entry alone should read as outdated")
	}
	if changed, err := a.Install(root, false); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	obj, hs := readHooks(t, root)
	if string(obj["other"]) != `{"keep":true}` && !strings.Contains(string(obj["other"]), `"keep": true`) {
		t.Fatalf("other key lost: %s", obj["other"])
	}
	ss := hs["sessionStart"]
	if len(ss) != 2 || ss[0]["command"] != "echo mine" || ss[0]["matcher"] != "x" || !hooks.Owned(ss[1]["command"]) {
		t.Fatalf("sessionStart %v", ss)
	}
	if ss[1]["command"] != command(VerbSessionStart) {
		t.Fatalf("stale entry not replaced: %v", ss[1])
	}
	// Atlas installs into afterFileEdit too: the user's entry stays first.
	if fe := hs["afterFileEdit"]; len(fe) != 2 || fe[0]["command"] != "./lint.sh" || fe[1]["command"] != command(VerbAfterFileEdit) {
		t.Fatalf("user event lost or not appended to: %v", hs["afterFileEdit"])
	}
	if _, ok := hs["preToolUse"]; ok {
		t.Fatalf("stale Atlas-only event kept: %v", hs["preToolUse"])
	}
	if a.State(root) != hooks.Current {
		t.Fatal("state after install")
	}

	// Lose one entry: outdated; force reinstall restores it.
	delete(hs, "stop")
	obj["hooks"] = hooks.Raw(hs)
	if err := hooks.SaveJSON(HooksPath(root), obj, 0o600); err != nil {
		t.Fatal(err)
	}
	if a.State(root) != hooks.Outdated {
		t.Fatal("state with a missing entry")
	}
	if changed, err := a.Install(root, true); err != nil || !changed {
		t.Fatalf("force install: %v %v", changed, err)
	}
	if a.State(root) != hooks.Current {
		t.Fatal("state after force")
	}

	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	obj, hs = readHooks(t, root)
	if len(hs) != 2 || len(hs["sessionStart"]) != 1 || hs["sessionStart"][0]["command"] != "echo mine" || len(hs["afterFileEdit"]) != 1 || hs["afterFileEdit"][0]["command"] != "./lint.sh" {
		t.Fatalf("after uninstall %v", hs)
	}
	if string(obj["version"]) != "1" {
		t.Fatal("version lost on uninstall")
	}
	if a.State(root) != hooks.Absent {
		t.Fatal("state after uninstall")
	}
	if err := a.Uninstall(t.TempDir()); err != nil {
		t.Fatalf("uninstall without a file: %v", err)
	}
}

func TestInstallRejectsForeignShape(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(HooksPath(root)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HooksPath(root), []byte(`{"hooks":{"stop":{"command":"x"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (Agent{}).Install(root, false); err == nil {
		t.Fatal("a non-list hook event should not be overwritten")
	}
}

const common = `"conversation_id":"c0ffee00-1111-2222-3333-444444444444","generation_id":"g1","model":"gpt-5.2-codex","hook_event_name":"%s","cursor_version":"2026.02.13","workspace_roots":["/tmp/proj"],"user_email":"u@example.com"`

func TestParse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := Agent{}
	nested := func() string {
		dir, _ := TranscriptDir("/tmp/proj")
		return filepath.Join(dir, "c0ffee00-1111-2222-3333-444444444444", "c0ffee00-1111-2222-3333-444444444444.jsonl")
	}()

	ev, err := a.Parse(VerbSessionStart, []byte(`{`+common+`,"transcript_path":null,"session_id":"c0ffee00-1111-2222-3333-444444444444","is_background_agent":false}`))
	if err != nil || ev.Kind != hooks.SessionStart || ev.SessionID != "c0ffee00-1111-2222-3333-444444444444" || ev.Cwd != "/tmp/proj" || ev.Model != "gpt-5.2-codex" {
		t.Fatalf("session-start %+v %v", ev, err)
	}
	if ev.Transcript != nested {
		t.Fatalf("derived transcript %q, want %q", ev.Transcript, nested)
	}

	ev, err = a.Parse(VerbBeforePrompt, []byte(`{`+common+`,"transcript_path":"/x/y.jsonl","prompt":"fix the build"}`))
	if err != nil || ev.Kind != hooks.TurnStart || ev.Prompt != "fix the build" || ev.Transcript != "/x/y.jsonl" {
		t.Fatalf("before-submit-prompt %+v %v", ev, err)
	}

	ev, err = a.Parse(VerbStop, []byte(`{`+common+`,"transcript_path":null,"status":"completed","loop_count":2,"input_tokens":1000,"output_tokens":50,"cache_read_tokens":700,"cache_write_tokens":200}`))
	if err != nil || ev.Kind != hooks.TurnEnd || ev.Usage == nil {
		t.Fatalf("stop %+v %v", ev, err)
	}
	if u := *ev.Usage; u.Input != 100 || u.Output != 50 || u.CacheRead != 700 || u.CacheCreation != 200 || u.Reasoning != 0 {
		t.Fatalf("usage %+v", u)
	}
	ev, _ = a.Parse(VerbStop, []byte(`{`+common+`,"input_tokens":10,"output_tokens":5,"cache_read_tokens":20,"cache_write_tokens":0}`))
	if ev.Usage.Input != 0 || ev.Usage.CacheRead != 20 {
		t.Fatalf("floor: %+v", *ev.Usage)
	}
	ev, _ = a.Parse(VerbStop, []byte(`{`+common+`,"status":"completed","loop_count":0}`))
	if ev.Usage != nil {
		t.Fatalf("no counts should give no usage: %+v", *ev.Usage)
	}
	if ev.TurnEndFailed {
		t.Fatal("a completed stop is not a failure")
	}
	// The CLI's stop after an API error or a user interrupt: no counts.
	ev, _ = a.Parse(VerbStop, []byte(`{`+common+`,"status":"error","loop_count":0}`))
	if !ev.TurnEndFailed || ev.Usage != nil {
		t.Fatalf("error stop %+v", ev)
	}
	ev, _ = a.Parse(VerbStop, []byte(`{`+common+`,"status":"aborted","loop_count":0}`))
	if ev.TurnEndFailed || ev.Usage != nil {
		t.Fatalf("aborted stop %+v", ev)
	}

	ev, err = a.Parse(VerbPreCompact, []byte(`{`+common+`,"trigger":"auto"}`))
	if err != nil || ev.Kind != hooks.Compaction {
		t.Fatalf("pre-compact %+v %v", ev, err)
	}
	ev, err = a.Parse(VerbSessionEnd, []byte(`{`+common+`,"reason":"completed","duration_ms":5505}`))
	if err != nil || ev.Kind != hooks.SessionEnd || ev.SessionID == "" {
		t.Fatalf("session-end %+v %v", ev, err)
	}
	if ev, err := a.Parse("subagent-start", []byte(`{`+common+`}`)); ev != nil || err != nil {
		t.Fatalf("unknown verb %+v %v", ev, err)
	}
	if _, err := a.Parse(VerbStop, []byte(`{not json`)); err == nil {
		t.Fatal("bad payload")
	}
	if ev, err := a.Parse(VerbSessionStart, nil); err != nil || ev.Kind != hooks.SessionStart || ev.Transcript != "" {
		t.Fatalf("empty payload %+v %v", ev, err)
	}
}

// TestParseLiveCLIPayloads holds payloads recorded from cursor-agent
// 2026.10.01 in its TUI: the lifecycle events as the CLI sends them.
func TestParseLiveCLIPayloads(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := Agent{}
	const id = "d8cde9cd-f8d5-4bb5-be8c-ac56bff22a5b"
	const tail = `"session_id":"` + id + `","cursor_version":"2026.10.01-14929f9","workspace_roots":["/private/tmp/hooks-cursor"],"user_email":"u@example.com"`
	named := "/Users/u/.cursor/projects/private-tmp-hooks-cursor/agent-transcripts/" + id + "/" + id + ".jsonl"
	derived := func() string {
		dir, _ := TranscriptDir("/private/tmp/hooks-cursor")
		return filepath.Join(dir, id, id+".jsonl")
	}()

	ev, err := a.Parse(VerbSessionStart, []byte(`{"conversation_id":"`+id+`","generation_id":"`+id+`","model":"default","is_background_agent":false,`+tail+`,"hook_event_name":"sessionStart","transcript_path":null}`))
	if err != nil || ev.Kind != hooks.SessionStart || ev.SessionID != id || ev.Cwd != "/private/tmp/hooks-cursor" {
		t.Fatalf("sessionStart %+v %v", ev, err)
	}
	if ev.Model != "auto" {
		t.Fatalf("model %q: Cursor's default is Auto", ev.Model)
	}
	if ev.Transcript != derived {
		t.Fatalf("transcript %q, want the nested path %q", ev.Transcript, derived)
	}

	ev, err = a.Parse(VerbBeforePrompt, []byte(`{"conversation_id":"`+id+`","generation_id":"055d3bc7","model":"default","prompt":"Create a file named note2.txt","attachments":[],`+tail+`,"hook_event_name":"beforeSubmitPrompt","transcript_path":null}`))
	if err != nil || ev.Kind != hooks.TurnStart || ev.Prompt != "Create a file named note2.txt" || ev.Transcript != derived {
		t.Fatalf("beforeSubmitPrompt %+v %v", ev, err)
	}

	// The tool hooks, recorded from the same CLI: a new file, a replacement
	// in it, and a shell command with its output.
	ev, err = a.Parse(VerbAfterFileEdit, []byte(`{"conversation_id":"`+id+`","generation_id":"9c3d906a-6fae-4aca-8800-640b0373b6c2","model":"default","file_path":"/private/tmp/hooks-cursor/probe.txt","edits":[{"old_string":"","new_string":"alpha\n"}],`+tail+`,"hook_event_name":"afterFileEdit","transcript_path":"`+named+`"}`))
	if err != nil || ev.Kind != hooks.ToolUse || ev.Tool == nil || ev.Transcript != named || ev.SessionID != id {
		t.Fatalf("afterFileEdit %+v %v", ev, err)
	}
	w := ev.Tool
	if w.Name != "Write" || w.Status != "completed" || string(w.Arguments) != `{"contents":"alpha\n","path":"/private/tmp/hooks-cursor/probe.txt"}` || !strings.HasPrefix(w.NativeID, "9c3d906a-6fae-4aca-8800-640b0373b6c2-edit-") {
		t.Fatalf("new file %+v %s", w, w.Arguments)
	}
	if got := record.WritePaths(w.Arguments, nil); len(got) != 1 || got[0] != "/private/tmp/hooks-cursor/probe.txt" {
		t.Fatalf("write paths %q", got)
	}
	ev, err = a.Parse(VerbAfterFileEdit, []byte(`{"conversation_id":"`+id+`","generation_id":"9c3d906a-6fae-4aca-8800-640b0373b6c2","model":"default","file_path":"/private/tmp/hooks-cursor/probe.txt","edits":[{"old_string":"alpha","new_string":"beta"}],`+tail+`,"hook_event_name":"afterFileEdit","transcript_path":"`+named+`"}`))
	if err != nil || ev.Kind != hooks.ToolUse || ev.Tool == nil {
		t.Fatalf("afterFileEdit (edit) %+v %v", ev, err)
	}
	if e := ev.Tool; e.Name != "Edit" || string(e.Arguments) != `{"edits":[{"old_string":"alpha","new_string":"beta"}],"new_string":"beta","old_string":"alpha","path":"/private/tmp/hooks-cursor/probe.txt"}` {
		t.Fatalf("edit %+v %s", e, e.Arguments)
	}
	if record.CanonicalToolName(ev.Tool.Name, "", "", ev.Tool.Arguments) != "Edit" || record.EditPatch(ev.Tool.Arguments, "") == "" {
		t.Fatalf("edit should canonicalise to Edit with a patch: %s", ev.Tool.Arguments)
	}
	ev, err = a.Parse(VerbAfterShell, []byte(`{"conversation_id":"`+id+`","generation_id":"9c3d906a-6fae-4aca-8800-640b0373b6c2","model":"default","command":"cat probe.txt","output":"beta\n","duration":13261.171,"sandbox":false,`+tail+`,"hook_event_name":"afterShellExecution","transcript_path":"`+named+`"}`))
	if err != nil || ev.Kind != hooks.ToolUse || ev.Tool == nil {
		t.Fatalf("afterShellExecution %+v %v", ev, err)
	}
	if sh := ev.Tool; sh.Name != "Shell" || string(sh.Arguments) != `{"command":"cat probe.txt"}` || sh.Result == nil || *sh.Result != "beta\n" || sh.Status != "completed" || !strings.HasPrefix(sh.NativeID, "9c3d906a-6fae-4aca-8800-640b0373b6c2-shell-") {
		t.Fatalf("shell %+v %s", sh, sh.Arguments)
	}
	if record.CanonicalToolName(ev.Tool.Name, "", "", ev.Tool.Arguments) != "Bash" {
		t.Fatal("shell should canonicalise to Bash")
	}
	// A tool hook without its call (nothing edited, nothing run) is no event.
	if ev, err := a.Parse(VerbAfterFileEdit, []byte(`{"conversation_id":"`+id+`",`+tail+`}`)); ev != nil || err != nil {
		t.Fatalf("empty afterFileEdit %+v %v", ev, err)
	}
	if ev, err := a.Parse(VerbAfterShell, []byte(`{"conversation_id":"`+id+`","command":"",`+tail+`}`)); ev != nil || err != nil {
		t.Fatalf("empty afterShellExecution %+v %v", ev, err)
	}

	ev, err = a.Parse(VerbStop, []byte(`{"conversation_id":"`+id+`","generation_id":"055d3bc7","model":"default","status":"completed","loop_count":0,"input_tokens":31219,"output_tokens":230,"cache_read_tokens":15488,"cache_write_tokens":0,`+tail+`,"hook_event_name":"stop","transcript_path":"`+named+`"}`))
	if err != nil || ev.Kind != hooks.TurnEnd || ev.TurnEndFailed || ev.Transcript != named || ev.Usage == nil {
		t.Fatalf("stop %+v %v", ev, err)
	}
	if u := *ev.Usage; u.Input != 31219-15488 || u.Output != 230 || u.CacheRead != 15488 || u.CacheCreation != 0 {
		t.Fatalf("usage %+v", u)
	}

	ev, err = a.Parse(VerbSessionEnd, []byte(`{"conversation_id":"`+id+`","generation_id":"`+id+`","model":"default","reason":"completed","duration_ms":60696,"is_background_agent":false,"final_status":"completed",`+tail+`,"hook_event_name":"sessionEnd","transcript_path":"`+named+`"}`))
	if err != nil || ev.Kind != hooks.SessionEnd || ev.Transcript != named {
		t.Fatalf("sessionEnd %+v %v", ev, err)
	}
}

func TestTranscriptPathDerivation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	const id = "abc-123"
	if got := ProjectSlug("/Users/me/Codes/my app"); got != "Users-me-Codes-my-app" {
		t.Fatalf("slug %q", got)
	}
	dir := filepath.Join(home, ".cursor", "projects", "Users-me-Codes-my-app", "agent-transcripts")
	nested := filepath.Join(dir, id, id+".jsonl")
	flat := filepath.Join(dir, id+".jsonl")

	if got := TranscriptPath("/named.jsonl", "/Users/me/Codes/my app", id); got != "/named.jsonl" {
		t.Fatalf("named: %q", got)
	}
	if got := TranscriptPath("", "/Users/me/Codes/my app", id); got != nested {
		t.Fatalf("nothing exists: %q, want %q", got, nested)
	}
	if err := os.MkdirAll(filepath.Dir(nested), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := TranscriptPath("", "/Users/me/Codes/my app", id); got != nested {
		t.Fatalf("nested dir only: %q, want %q", got, nested)
	}
	if err := os.WriteFile(flat, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := TranscriptPath("", "/Users/me/Codes/my app", id); got != flat {
		t.Fatalf("an existing flat file beats a nested one that is not there: %q", got)
	}
	if err := os.WriteFile(nested, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := TranscriptPath("", "/Users/me/Codes/my app", id); got != nested {
		t.Fatalf("both exist: %q, want nested", got)
	}
	if got := TranscriptPath("", "", id); got != "" {
		t.Fatalf("no workspace: %q", got)
	}
	if got := TranscriptPath("", "/Users/me", "../escape"); got != "" {
		t.Fatalf("bad id: %q", got)
	}
}

func TestIdentity(t *testing.T) {
	a := Agent{}
	if a.ID() != "cursor" || a.Name() != "Cursor" || a.Binary() != "cursor-agent" {
		t.Fatal("identity")
	}
	if a.ResumeCommand("s1") != "cursor-agent --resume s1" {
		t.Fatal(a.ResumeCommand("s1"))
	}
	if a.Response(hooks.SessionStart, "hello") != nil {
		t.Fatal("Cursor shows no banner")
	}
	if len(a.Verbs()) != 7 {
		t.Fatal(a.Verbs())
	}
	if got, ok := hooks.Get(ID); !ok || got.ID() != ID {
		t.Fatal("not registered")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !a.Detect(root) {
		t.Fatal("a .cursor directory should be detected")
	}
}
