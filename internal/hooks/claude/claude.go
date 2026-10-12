// Package claude integrates Claude Code: hooks in .claude/settings.local.json
// and its JSONL transcript (docs/research/entire-cli.md §2.4).
package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/tryatlas/cli/internal/capture/record"
	"github.com/tryatlas/cli/internal/hooks"
)

// ID is the agent's registry key.
const ID = "claude-code"

func init() { hooks.Register(Agent{}) }

// Agent is the Claude Code integration.
type Agent struct{}

// Hook verbs, as the installed commands name them.
const (
	VerbSessionStart = "session-start"
	VerbPromptSubmit = "user-prompt-submit"
	VerbStop         = "stop"
	VerbStopFailure  = "stop-failure"
	VerbPreCompact   = "pre-compact"
	VerbSessionEnd   = "session-end"
	VerbPostToolUse  = "post-tool-use"
)

// ID is the registry key.
func (Agent) ID() string { return ID }

// Name is the display name.
func (Agent) Name() string { return "Claude Code" }

// Binary is the agent's command.
func (Agent) Binary() string { return "claude" }

// Verbs are the hook verbs the installed config invokes.
func (Agent) Verbs() []string {
	return []string{VerbSessionStart, VerbPromptSubmit, VerbPostToolUse, VerbStop, VerbStopFailure, VerbPreCompact, VerbSessionEnd}
}

// SettingsPath is the local (per-machine, uncommitted) Claude settings
// file: hooks here bind this checkout to this machine's atlas.
func SettingsPath(root string) string {
	return filepath.Join(root, ".claude", "settings.local.json")
}

func (a Agent) shaped(root string) hooks.ClaudeShaped {
	return hooks.ClaudeShaped{Path: SettingsPath(root), Types: []hooks.ClaudeHook{
		{Type: "SessionStart", Command: hooks.WrapWithMessage(hooks.Command(ID, VerbSessionStart), hooks.MissingMessage)},
		{Type: "UserPromptSubmit", Command: hooks.Wrap(hooks.Command(ID, VerbPromptSubmit))},
		{Type: "PostToolUse", Command: hooks.Wrap(hooks.Command(ID, VerbPostToolUse))},
		{Type: "Stop", Command: hooks.Wrap(hooks.Command(ID, VerbStop))},
		{Type: "StopFailure", Command: hooks.Wrap(hooks.Command(ID, VerbStopFailure))},
		{Type: "PreCompact", Command: hooks.Wrap(hooks.Command(ID, VerbPreCompact))},
		{Type: "SessionEnd", Command: hooks.Wrap(hooks.Command(ID, VerbSessionEnd))},
	}}
}

// Install writes the hooks into the local settings.
func (a Agent) Install(root string, force bool) (bool, error) { return a.shaped(root).Install(force) }

// Uninstall removes them.
func (a Agent) Uninstall(root string) error { return a.shaped(root).Uninstall() }

// State compares the local settings with what Install writes.
func (a Agent) State(root string) hooks.InstallState { return a.shaped(root).State() }

// Detect reports a .claude directory in root or claude on PATH.
func (a Agent) Detect(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".claude")); err == nil {
		return true
	}
	_, err := exec.LookPath(a.Binary())
	return err == nil
}

// Reader reads the JSONL transcript.
func (Agent) Reader() hooks.Reader { return reader{} }

// Response renders a SessionStart systemMessage, which Claude shows the user.
func (Agent) Response(k hooks.Kind, message string) []byte {
	if k != hooks.SessionStart || message == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"systemMessage": message})
	return append(b, '\n')
}

// ResumeCommand continues a session in Claude Code.
func (Agent) ResumeCommand(id string) string { return "claude -r " + id }

// payload is the common shape of Claude Code hook input.
type payload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	Source         string          `json:"source"`
	ToolName       string          `json:"tool_name"`
	ToolUseID      string          `json:"tool_use_id"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
}

// toolOf is the completed call a PostToolUse payload describes. The
// response is a string or an object; either is kept as text.
func (p payload) toolOf() *record.Tool {
	if p.ToolUseID == "" || p.ToolName == "" {
		return nil
	}
	t := &record.Tool{NativeID: p.ToolUseID, Name: p.ToolName, Arguments: p.ToolInput, Status: "completed"}
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
	ev := &hooks.Event{SessionID: p.SessionID, Transcript: p.TranscriptPath, Cwd: p.Cwd, Model: p.Model}
	switch verb {
	case VerbSessionStart:
		ev.Kind = hooks.SessionStart
	case VerbPromptSubmit:
		ev.Kind = hooks.TurnStart
		ev.Prompt = StripIDE(p.Prompt)
	case VerbStop:
		ev.Kind = hooks.TurnEnd
	case VerbStopFailure:
		ev.Kind = hooks.TurnEnd
		ev.TurnEndFailed = true
	case VerbPostToolUse:
		ev.Kind = hooks.ToolUse
		ev.Tool = p.toolOf()
		if ev.Tool == nil {
			return nil, nil
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
