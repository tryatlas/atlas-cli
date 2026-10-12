package opencode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func TestPluginInstallStateUninstall(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	if a.State(root) != hooks.Absent {
		t.Fatal("state before install")
	}
	changed, err := a.Install(root, false)
	if err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	path := PluginPath(root)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != Plugin || !strings.Contains(string(b), Marker) || !strings.Contains(string(b), "export const AtlasPlugin") {
		t.Fatal("the written plugin is not the embedded render")
	}
	for _, verb := range a.Verbs() {
		if !strings.Contains(string(b), `atlas hooks opencode ${verb}`) && !strings.Contains(string(b), `"`+verb+`"`) {
			t.Fatalf("plugin does not send %s", verb)
		}
	}
	for _, hook := range []string{`"tool.execute.before"`, `"tool.execute.after"`, `sendSync("tool-use"`} {
		if !strings.Contains(string(b), hook) {
			t.Fatalf("plugin lacks %s", hook)
		}
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if a.State(root) != hooks.Current {
		t.Fatal("state after install")
	}
	if changed, err := a.Install(root, false); err != nil || changed {
		t.Fatalf("second install: %v %v", changed, err)
	}
	if changed, err := a.Install(root, true); err != nil || !changed {
		t.Fatalf("forced install: %v %v", changed, err)
	}

	// An older render carries the marker: outdated, and rewritten.
	if err := os.WriteFile(path, []byte("// "+Marker+"\nexport const AtlasPlugin = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a.State(root) != hooks.Outdated {
		t.Fatal("state with an old render")
	}
	if changed, _ := a.Install(root, false); !changed || a.State(root) != hooks.Current {
		t.Fatal("old render not rewritten")
	}

	// Something else at the path is not ours.
	if err := os.WriteFile(path, []byte("export const Other = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a.State(root) != hooks.Absent {
		t.Fatal("state with a foreign file")
	}

	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("plugin still there")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("empty plugins dir kept")
	}
	if err := a.Uninstall(root); err != nil {
		t.Fatalf("second uninstall: %v", err)
	}
}

func TestUninstallKeepsOtherPlugins(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	if _, err := a.Install(root, false); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(root, ".opencode", "plugins", "mine.ts")
	if err := os.WriteFile(other, []byte("export const Mine = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("the user's plugin went with ours")
	}
}

func TestParse(t *testing.T) {
	a := Agent{}
	const id = "ses_ed37d20eeffeeHcUcSpQTFZiNv"
	cases := []struct {
		verb    string
		payload string
		kind    hooks.Kind
		prompt  string
		model   string
	}{
		{VerbSessionStart, `{"session_id":"` + id + `"}`, hooks.SessionStart, "", ""},
		{VerbTurnStart, `{"session_id":"` + id + `","prompt":"<system-reminder>ctx</system-reminder>\nfix it","model":"gemini-3.7-flash"}`, hooks.TurnStart, "fix it", "gemini-3.7-flash"},
		{VerbTurnEnd, `{"session_id":"` + id + `","model":"gemini-3.7-flash"}`, hooks.TurnEnd, "", "gemini-3.7-flash"},
		{VerbCompaction, `{"session_id":"` + id + `"}`, hooks.Compaction, "", ""},
		{VerbSessionEnd, `{"session_id":"` + id + `"}`, hooks.SessionEnd, "", ""},
	}
	for _, c := range cases {
		ev, err := a.Parse(c.verb, []byte(c.payload))
		if err != nil || ev == nil {
			t.Fatalf("%s: %v %v", c.verb, ev, err)
		}
		if ev.Kind != c.kind || ev.SessionID != id || ev.Prompt != c.prompt || ev.Model != c.model {
			t.Fatalf("%s: %+v", c.verb, ev)
		}
		if want := filepath.Join("hooks", ID, "export", id+".json"); !strings.HasSuffix(ev.Transcript, want) {
			t.Fatalf("%s: transcript %q", c.verb, ev.Transcript)
		}
	}
	// tool-use: OpenCode's tool.execute.after as the plugin forwards it, with
	// the id the export's tool part carries as callID.
	toolUse := `{"session_id":"` + id + `","tool":"write","call_id":"ZdNFvSAofdecladc","title":"rt.txt","output":"Wrote file successfully.",` +
		`"metadata":{"diagnostics":{},"filepath":"/w/rt.txt","exists":false},"args":{"filePath":"/w/rt.txt","content":"realtime\n"}}`
	ev, err := a.Parse(VerbToolUse, []byte(toolUse))
	if err != nil || ev == nil || ev.Kind != hooks.ToolUse || ev.SessionID != id || ev.Tool == nil {
		t.Fatalf("tool-use: %+v %v", ev, err)
	}
	if tool := ev.Tool; tool.NativeID != "ZdNFvSAofdecladc" || tool.Name != "write" || tool.Title != "rt.txt" || tool.Status != "completed" ||
		string(tool.Arguments) != `{"filePath":"/w/rt.txt","content":"realtime\n"}` || tool.Result == nil || *tool.Result != "Wrote file successfully." || tool.Locations != nil {
		t.Fatalf("tool-use tool: %+v", tool)
	}
	if !strings.HasSuffix(ev.Transcript, filepath.Join("hooks", ID, "export", id+".json")) {
		t.Fatalf("tool-use names no export cache: %q", ev.Transcript)
	}
	// apply_patch's metadata lists the files it wrote; null args and a
	// missing output are nothing, not "null".
	ev, err = a.Parse(VerbToolUse, []byte(`{"session_id":"`+id+`","tool":"apply_patch","call_id":"call_p","metadata":{"files":[{"filePath":"/w/a.go"},{"filePath":"/w/b.go"}]},"args":null}`))
	if err != nil || ev == nil || ev.Tool == nil || strings.Join(ev.Tool.Locations, ",") != "/w/a.go,/w/b.go" || ev.Tool.Arguments != nil || ev.Tool.Result != nil {
		t.Fatalf("apply_patch tool-use: %+v %v", ev.Tool, err)
	}
	// Without a call id or a tool name there is no call to record.
	for _, raw := range []string{`{"session_id":"` + id + `","tool":"bash"}`, `{"session_id":"` + id + `","call_id":"c1"}`} {
		if ev, err := a.Parse(VerbToolUse, []byte(raw)); ev != nil || err != nil {
			t.Fatalf("tool-use without a call: %+v %v", ev, err)
		}
	}
	if ev, err := a.Parse("something-else", []byte(`{"session_id":"x"}`)); ev != nil || err != nil {
		t.Fatalf("unknown verb: %v %v", ev, err)
	}
	if _, err := a.Parse(VerbTurnEnd, []byte(`not json`)); err == nil {
		t.Fatal("bad payload accepted")
	}
	ev, err = a.Parse(VerbTurnEnd, []byte(`{"session_id":"../../etc"}`))
	if err != nil || ev.Transcript != "" {
		t.Fatalf("an unusable id named a cache: %+v %v", ev, err)
	}
}

func TestDetectAndMisc(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	if err := os.WriteFile(filepath.Join(root, "opencode.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !a.Detect(root) {
		t.Fatal("opencode.json not detected")
	}
	if a.ID() != "opencode" || a.Name() != "OpenCode" || a.Binary() != "opencode" {
		t.Fatal("identity")
	}
	if a.ResumeCommand("ses_1") != "opencode -s ses_1" {
		t.Fatal(a.ResumeCommand("ses_1"))
	}
	if a.Response(hooks.SessionStart, "hello") != nil {
		t.Fatal("the plugin reads no stdout")
	}
	if got, ok := hooks.Get(ID); !ok || got.ID() != ID {
		t.Fatal("not registered")
	}
	if !hooks.Owned(`sh -c 'if ! command -v atlas >/dev/null 2>&1; then exit 0; fi; exec atlas hooks opencode turn-end'`) {
		t.Fatal("the plugin's command shape is not recognised as Atlas's")
	}
}
