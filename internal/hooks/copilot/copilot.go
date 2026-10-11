// Package copilot integrates GitHub Copilot CLI: an Atlas-owned hook file
// under .github/hooks/ and the session's events.jsonl transcript
// (docs/research/entire-cli.md §3, Copilot row).
//
// Copilot loads a repository's hooks and its .github/mcp.json only from a
// trusted working directory (verified against Copilot CLI 1.0.95). The
// interactive `copilot` asks on first launch in a folder; "This and future
// sessions" records it under trustedFolders in ~/.copilot/config.json. Prompt
// mode (`copilot -p`) never asks: the folder must already be trusted, or
// COPILOT_ALLOW_ALL must be exactly "true", or GITHUB_COPILOT_PROMPT_MODE_REPO_HOOKS=true
// (hooks) and GITHUB_COPILOT_PROMPT_MODE_WORKSPACE_MCP=true (memory) must be set.
// --allow-all-tools alone leaves the folder untrusted and the hooks unread.
package copilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// ID is the agent's registry key.
const ID = "copilot"

func init() { hooks.Register(Agent{}) }

// Agent is the GitHub Copilot CLI integration.
type Agent struct{}

// Hook verbs, as the installed commands name them.
const (
	VerbSessionStart    = "session-start"
	VerbPromptSubmitted = "user-prompt-submitted"
	VerbAgentStop       = "agent-stop"
	VerbSessionEnd      = "session-end"
)

// Copilot's own hook event names, the keys of the hooks object.
const (
	eventSessionStart        = "sessionStart"
	eventUserPromptSubmitted = "userPromptSubmitted"
	eventAgentStop           = "agentStop"
	eventSessionEnd          = "sessionEnd"
)

// ID is the registry key.
func (Agent) ID() string { return ID }

// Name is the display name.
func (Agent) Name() string { return "GitHub Copilot CLI" }

// Binary is the agent's command.
func (Agent) Binary() string { return "copilot" }

// Verbs are the hook verbs the installed config invokes.
func (Agent) Verbs() []string {
	return []string{VerbSessionStart, VerbPromptSubmitted, VerbAgentStop, VerbSessionEnd}
}

// HooksPath is the hook file Atlas owns. Copilot auto-discovers every JSON
// file under .github/hooks/, so Atlas keeps its hooks in a file of its own
// and never edits a user's file.
func HooksPath(root string) string {
	return filepath.Join(root, ".github", "hooks", "atlas.json")
}

// hookEntry is one Copilot hook: {"type":"command","bash":…}. The command
// key is bash, not command as in Claude-shaped configs.
type hookEntry struct {
	Type string `json:"type"`
	Bash string `json:"bash"`
}

// hookFile is the whole file: {"version":1,"hooks":{<event>:[entry…]}}.
type hookFile struct {
	Version int                    `json:"version"`
	Hooks   map[string][]hookEntry `json:"hooks"`
}

// wanted is the file Install writes.
func wanted() hookFile {
	entry := func(verb string) []hookEntry {
		return []hookEntry{{Type: "command", Bash: hooks.Wrap(hooks.Command(ID, verb))}}
	}
	return hookFile{Version: 1, Hooks: map[string][]hookEntry{
		eventSessionStart:        entry(VerbSessionStart),
		eventUserPromptSubmitted: entry(VerbPromptSubmitted),
		eventAgentStop:           entry(VerbAgentStop),
		eventSessionEnd:          entry(VerbSessionEnd),
	}}
}

// Install writes the hook file. The file is Atlas's alone, so there is
// nothing to merge or prune: it is rewritten whenever its content differs
// from what this version wants.
func (a Agent) Install(root string, force bool) (bool, error) {
	if !force && a.State(root) == hooks.Current {
		return false, nil
	}
	return true, hooks.SaveJSON(HooksPath(root), wanted(), 0o644)
}

// Uninstall deletes the hook file, and the hooks directory when that leaves
// it empty.
func (Agent) Uninstall(root string) error {
	path := HooksPath(root)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Dir(path)) // only an empty directory goes
	return nil
}

// State compares the file's content with what Install writes: a missing
// file is absent, a different one (an older Atlas version's, or anything
// else under our name) is outdated.
func (Agent) State(root string) hooks.InstallState {
	b, err := os.ReadFile(HooksPath(root))
	if err != nil {
		return hooks.Absent
	}
	var cur hookFile
	if json.Unmarshal(b, &cur) != nil {
		return hooks.Outdated
	}
	if bytes.Equal(hooks.Raw(cur), hooks.Raw(wanted())) {
		return hooks.Current
	}
	return hooks.Outdated
}

// Detect reports a .github/hooks directory in root or copilot on PATH.
// Copilot keeps no dot-directory of its own in a repository.
func (a Agent) Detect(root string) bool {
	if fi, err := os.Stat(filepath.Join(root, ".github", "hooks")); err == nil && fi.IsDir() {
		return true
	}
	_, err := exec.LookPath(a.Binary())
	return err == nil
}

// Reader reads the events.jsonl transcript.
func (Agent) Reader() hooks.Reader { return reader{} }

// Response is nil: Copilot shows nothing a hook prints.
func (Agent) Response(hooks.Kind, string) []byte { return nil }

// ResumeCommand continues a session in Copilot CLI.
func (Agent) ResumeCommand(id string) string { return "copilot --resume " + id }

// AddNote says Copilot reads a repository's hooks and MCP config only from
// a trusted working directory, which prompt mode never asks about.
func (Agent) AddNote() string {
	return "Copilot loads the Atlas hooks and .github/mcp.json only from a trusted folder: start `copilot` here once and answer the trust prompt with \"This and future sessions\" (stored in ~/.copilot/config.json trustedFolders). `copilot -p` never asks, so an untrusted folder runs without capture even with --allow-all-tools; set COPILOT_ALLOW_ALL=true (exactly \"true\") to trust it for that run."
}

// Home is Copilot's state directory: $COPILOT_HOME, else ~/.copilot.
func Home() (string, error) {
	if h := os.Getenv("COPILOT_HOME"); h != "" {
		return h, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".copilot"), nil
}

// TranscriptPath is the transcript of a session: <Home>/session-state/<id>/
// events.jsonl. Copilot names it in its agentStop payload only; the
// userPromptSubmitted payload, where the dispatcher records the turn-start
// offset, carries just the session id. Deriving the path from that id is the
// allowed exception of ADR-0013 §2: the payload's own session id names the
// file, so no directory is scanned and no other session's file is touched.
// An id that is not a safe file name yields "".
func TranscriptPath(sessionID string) string {
	if !hooks.ValidSessionID(sessionID) {
		return ""
	}
	home, err := Home()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "session-state", sessionID, "events.jsonl")
}

// Parse maps a hook verb and payload to an event. Copilot's keys are
// camelCase (sessionId, transcriptPath, cwd, prompt); the snake_case forms
// VS Code's hook host sends are accepted too.
func (Agent) Parse(verb string, raw []byte) (*hooks.Event, error) {
	m, err := hooks.Decode(raw)
	if err != nil {
		return nil, err
	}
	ev := &hooks.Event{
		SessionID:  first(m, "sessionId", "session_id"),
		Transcript: first(m, "transcriptPath", "transcript_path"),
		Cwd:        hooks.Str(m, "cwd"),
	}
	switch verb {
	case VerbSessionStart:
		ev.Kind = hooks.SessionStart
	case VerbPromptSubmitted:
		ev.Kind = hooks.TurnStart
		ev.Prompt = hooks.Str(m, "prompt")
	case VerbAgentStop:
		ev.Kind = hooks.TurnEnd
	case VerbSessionEnd:
		ev.Kind = hooks.SessionEnd
	default:
		return nil, nil
	}
	if ev.Transcript == "" {
		ev.Transcript = TranscriptPath(ev.SessionID)
	}
	return ev, nil
}

// first is the first non-empty string among the keys.
func first(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s := hooks.Str(m, k); s != "" {
			return s
		}
	}
	return ""
}
