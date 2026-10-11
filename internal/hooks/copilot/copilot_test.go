package copilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	if len(a.Verbs()) != 4 {
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
	if file.Version != 1 || len(file.Hooks) != 4 {
		t.Fatalf("%s", b)
	}
	for ev, verb := range map[string]string{
		"sessionStart": "session-start", "userPromptSubmitted": "user-prompt-submitted",
		"agentStop": "agent-stop", "sessionEnd": "session-end",
	} {
		list := file.Hooks[ev]
		if len(list) != 1 || list[0].Type != "command" {
			t.Fatalf("%s: %+v", ev, list)
		}
		if want := hooks.Wrap("atlas hooks copilot " + verb); list[0].Bash != want {
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
	if err := os.WriteFile(path, []byte(`{"version":1,"hooks":{"agentStop":[{"type":"command","bash":"atlas hooks copilot stop"}]}}`), 0o644); err != nil {
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
