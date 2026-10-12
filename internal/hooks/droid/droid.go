// Package droid integrates Factory Droid: hooks in .factory/settings.json and
// its envelope JSONL transcript under ~/.factory/sessions/ (ADR-0013,
// docs/research/entire-cli.md §3).
package droid

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// ID is the agent's registry key.
const ID = "droid"

func init() { hooks.Register(Agent{}) }

// Agent is the Factory Droid integration.
type Agent struct{}

// Hook verbs, as the installed commands name them.
const (
	VerbSessionStart = "session-start"
	VerbPromptSubmit = "user-prompt-submit"
	VerbStop         = "stop"
	VerbPreCompact   = "pre-compact"
	VerbSessionEnd   = "session-end"
	VerbPostToolUse  = "post-tool-use"
)

// MissingMessage is what Droid shows when the hook is installed but atlas is
// not. Plain text: Droid prints a hook's stdout as is.
const MissingMessage = "Atlas is enabled here but atlas-cli is not installed or not on PATH."

// ID is the registry key.
func (Agent) ID() string { return ID }

// Name is the display name.
func (Agent) Name() string { return "Factory Droid" }

// Binary is the agent's command.
func (Agent) Binary() string { return "droid" }

// Verbs are the hook verbs the installed config invokes.
func (Agent) Verbs() []string {
	return []string{VerbSessionStart, VerbPromptSubmit, VerbPostToolUse, VerbStop, VerbPreCompact, VerbSessionEnd}
}

// SettingsPath is Droid's project settings file, which holds its hooks. It
// is a project file (often committed) and carries no secret.
func SettingsPath(root string) string {
	return filepath.Join(root, ".factory", "settings.json")
}

func (Agent) shaped(root string) hooks.ClaudeShaped {
	promptSubmit := hooks.Wrap(hooks.Command(ID, VerbPromptSubmit))
	return hooks.ClaudeShaped{Path: SettingsPath(root), Mode: 0o644, Types: []hooks.ClaudeHook{
		// SessionStart runs user-prompt-submit too: UserPromptSubmit never
		// fires under droid exec, so the turn opens at session start with no
		// prompt and the dispatcher takes it from the transcript at Stop.
		{Type: "SessionStart", Command: hooks.WrapWithMessage(hooks.Command(ID, VerbSessionStart), MissingMessage)},
		{Type: "SessionStart", Command: promptSubmit},
		{Type: "UserPromptSubmit", Command: promptSubmit},
		// Matcher "" is every tool: each completed call is recorded into the
		// open turn as it happens (per-event delivery).
		{Type: "PostToolUse", Command: hooks.Wrap(hooks.Command(ID, VerbPostToolUse))},
		{Type: "Stop", Command: hooks.Wrap(hooks.Command(ID, VerbStop))},
		{Type: "PreCompact", Command: hooks.Wrap(hooks.Command(ID, VerbPreCompact))},
		{Type: "SessionEnd", Command: hooks.Wrap(hooks.Command(ID, VerbSessionEnd))},
	}}
}

// Install writes the hooks into the project settings.
func (a Agent) Install(root string, force bool) (bool, error) { return a.shaped(root).Install(force) }

// Uninstall removes them.
func (a Agent) Uninstall(root string) error { return a.shaped(root).Uninstall() }

// State compares the project settings with what Install writes.
func (a Agent) State(root string) hooks.InstallState { return a.shaped(root).State() }

// Detect reports a .factory directory in root or droid on PATH.
func (a Agent) Detect(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".factory")); err == nil {
		return true
	}
	_, err := exec.LookPath(a.Binary())
	return err == nil
}

// Reader reads the envelope JSONL transcript.
func (Agent) Reader() hooks.Reader { return reader{} }

// Response renders the SessionStart banner as the plain text Droid shows.
func (Agent) Response(k hooks.Kind, message string) []byte {
	if k != hooks.SessionStart || message == "" {
		return nil
	}
	return []byte(message + "\n")
}

// ResumeCommand continues a session in Droid.
func (Agent) ResumeCommand(id string) string { return "droid --session-id " + id }

// payload is the common shape of Droid hook input. Stop omits model, and
// the SessionStart-fired user-prompt-submit omits prompt. PostToolUse
// carries the Claude-style tool fields (Entire's factoryaidroid/types.go
// postToolHookInputRaw).
type payload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
}

// toolOf is the completed call a PostToolUse payload describes, keyed by
// the tool_use_id Droid shares with its transcript's tool_use block. Nil
// without an id or a name: Droid has been seen to omit tool_use_id (Entire
// keeps a fingerprint fallback for it), and such a call is left to the
// transcript at Stop. The response is a string or an object; either is
// kept as text.
func (p payload) toolOf() *record.Tool {
	if p.ToolName == "" {
		return nil
	}
	id := p.ToolUseID
	if id == "" {
		// Droid has shipped tool payloads without an id: derive a stable one
		// from the call; the dispatcher still matches the transcript's call
		// by arguments or written path.
		id = "droid-" + store.SHA256Hex(append([]byte(p.ToolName+"\x00"), p.ToolInput...))[:16]
	}
	t := &record.Tool{NativeID: id, Name: p.ToolName, Arguments: p.ToolInput, Status: "completed"}
	if r := bytes.TrimSpace(p.ToolResponse); len(r) > 0 && string(r) != "null" {
		var str string
		if json.Unmarshal(r, &str) != nil {
			str = string(r)
		}
		t.Result = &str
	}
	return t
}

// Parse maps a hook verb and payload to an event.
func (Agent) Parse(verb string, raw []byte) (*hooks.Event, error) {
	var p payload
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
	}
	ev := &hooks.Event{SessionID: p.SessionID, Transcript: p.TranscriptPath, Cwd: p.Cwd, Model: CleanModel(p.Model)}
	switch verb {
	case VerbSessionStart:
		ev.Kind = hooks.SessionStart
	case VerbPromptSubmit:
		ev.Kind = hooks.TurnStart
		ev.Prompt = strings.TrimSpace(p.Prompt)
	case VerbPostToolUse:
		ev.Kind = hooks.ToolUse
		ev.Tool = p.toolOf()
		if ev.Tool == nil {
			return nil, nil
		}
	case VerbStop:
		ev.Kind = hooks.TurnEnd
		if ev.Model == "" {
			ev.Model = ModelFromSettings(p.TranscriptPath)
		}
	case VerbPreCompact:
		ev.Kind = hooks.Compaction
	case VerbSessionEnd:
		ev.Kind = hooks.SessionEnd
	default:
		return nil, nil
	}
	return ev, nil
}

// SettingsFor is the per-session settings file Droid keeps beside a
// transcript: <dir>/<id>.settings.json for <dir>/<id>.jsonl.
func SettingsFor(transcript string) string {
	return strings.TrimSuffix(transcript, ".jsonl") + ".settings.json"
}

// ModelFromSettings reads the session's model from the settings file beside
// the transcript the payload named; "" when there is none.
func ModelFromSettings(transcript string) string {
	if transcript == "" {
		return ""
	}
	b, err := os.ReadFile(SettingsFor(transcript))
	if err != nil {
		return ""
	}
	var s struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	return CleanModel(s.Model)
}

// CleanModel strips the custom: prefix Droid puts on a user-defined model
// (custom:Gemini-2.5-Pro-0 → Gemini-2.5-Pro-0).
func CleanModel(m string) string { return strings.TrimPrefix(m, "custom:") }
