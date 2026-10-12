// Package pi integrates the Pi coding agent: an owned extension file at
// .pi/extensions/atlas/index.ts and Pi's tree-shaped JSONL session file
// (docs/research/entire-cli.md §3).
package pi

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tryatlas/cli/internal/capture/record"
	"github.com/tryatlas/cli/internal/hooks"
)

// ID is the agent's registry key.
const ID = "pi"

func init() { hooks.Register(Agent{}) }

// Agent is the Pi integration.
type Agent struct{}

// Hook verbs, as the extension names them.
const (
	VerbSessionStart     = "session-start"
	VerbBeforeAgentStart = "before-agent-start"
	VerbToolResult       = "tool-result"
	VerbAgentEnd         = "agent-end"
)

//go:embed extension.ts
var extension []byte

// marker is the part of the extension's header that says Atlas wrote the
// file; an older render still carries it, a user's own extension does not.
const marker = "written by atlas agents add"

// ID is the registry key.
func (Agent) ID() string { return ID }

// Name is the display name.
func (Agent) Name() string { return "Pi" }

// Binary is the agent's command.
func (Agent) Binary() string { return "pi" }

// Verbs are the hook verbs the extension invokes.
func (Agent) Verbs() []string {
	return []string{VerbSessionStart, VerbBeforeAgentStart, VerbToolResult, VerbAgentEnd}
}

// ExtensionDir is the directory Pi loads the Atlas extension from; Pi
// discovers project extensions by directory under .pi/extensions.
func ExtensionDir(root string) string {
	return filepath.Join(root, ".pi", "extensions", "atlas")
}

// ExtensionPath is the extension file Install writes.
func ExtensionPath(root string) string { return filepath.Join(ExtensionDir(root), "index.ts") }

// Install writes the extension whole. A file Atlas did not write is left
// alone unless force is set, so a user's own extension at the same path
// survives.
func (Agent) Install(root string, force bool) (bool, error) {
	path := ExtensionPath(root)
	cur, err := os.ReadFile(path) //nolint:gosec // G304: the extension file under the Project the user asked to configure
	switch {
	case err == nil && string(cur) == string(extension):
		return false, nil
	case err == nil && !force && !isOurs(cur):
		return false, fmt.Errorf("%s is not an Atlas extension; remove it or pass --force", path)
	case err != nil && !errors.Is(err, fs.ErrNotExist):
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".atlas-tmp"
	// 0644: Pi reads the file itself.
	if err := os.WriteFile(tmp, extension, 0o644); err != nil { //nolint:gosec // G306: Pi must be able to read its extension
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// Uninstall removes the extension directory: Pi discovers extensions by
// directory, so an empty .pi/extensions/atlas would be a half-removed one.
// A foreign file at the path is kept.
func (Agent) Uninstall(root string) error {
	cur, err := os.ReadFile(ExtensionPath(root))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return os.RemoveAll(ExtensionDir(root))
	case err != nil:
		return err
	case !isOurs(cur):
		return nil
	}
	return os.RemoveAll(ExtensionDir(root))
}

// State compares the file with what Install writes.
func (Agent) State(root string) hooks.InstallState {
	cur, err := os.ReadFile(ExtensionPath(root))
	switch {
	case err != nil:
		return hooks.Absent
	case string(cur) == string(extension):
		return hooks.Current
	case isOurs(cur):
		return hooks.Outdated
	}
	return hooks.Absent
}

func isOurs(b []byte) bool { return strings.Contains(string(b), marker) }

// Detect reports a .pi directory in root or pi on PATH.
func (a Agent) Detect(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".pi")); err == nil {
		return true
	}
	_, err := exec.LookPath(a.Binary())
	return err == nil
}

// Reader reads the JSONL session file.
func (Agent) Reader() hooks.Reader { return reader{} }

// Response is nil: the extension ignores hook stdout.
func (Agent) Response(hooks.Kind, string) []byte { return nil }

// ResumeCommand continues a session in Pi, which accepts the session uuid
// (or a prefix of it).
func (Agent) ResumeCommand(id string) string { return "pi --session " + id }

// payload is what the extension pipes to the hook.
type payload struct {
	Type        string `json:"type"`
	Cwd         string `json:"cwd"`
	SessionFile string `json:"session_file"`
	SessionID   string `json:"session_id"`
	Prompt      string `json:"prompt"`
	// tool-result: the completed call, with its result as text.
	ToolCallID string          `json:"tool_call_id"`
	ToolName   string          `json:"tool_name"`
	Args       json.RawMessage `json:"args"`
	Result     *string         `json:"result"`
	IsError    bool            `json:"is_error"`
}

// toolOf is the completed call a tool-result payload describes; nil when
// the payload names no call.
func (p payload) toolOf() *record.Tool {
	if p.ToolCallID == "" || p.ToolName == "" {
		return nil
	}
	t := &record.Tool{NativeID: p.ToolCallID, Name: p.ToolName, Arguments: p.Args, Result: p.Result, Status: "completed"}
	if p.IsError {
		t.Status = "failed"
	}
	return t
}

// Parse maps a hook verb and payload to an event. A payload naming no
// session (pi --no-session, as a nested subagent runs) yields nothing.
func (Agent) Parse(verb string, raw []byte) (*hooks.Event, error) {
	var p payload
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
	}
	id := p.SessionID
	if id == "" {
		id = SessionIDFromPath(p.SessionFile)
	}
	if id == "" {
		return nil, nil
	}
	ev := &hooks.Event{SessionID: id, Transcript: p.SessionFile, Cwd: p.Cwd}
	switch verb {
	case VerbSessionStart:
		ev.Kind = hooks.SessionStart
	case VerbBeforeAgentStart:
		ev.Kind = hooks.TurnStart
		ev.Prompt = p.Prompt
	case VerbToolResult:
		ev.Kind = hooks.ToolUse
		ev.Tool = p.toolOf()
		if ev.Tool == nil {
			return nil, nil
		}
	case VerbAgentEnd:
		ev.Kind = hooks.TurnEnd
	default:
		return nil, nil
	}
	return ev, nil
}

// SessionIDFromPath is the uuid in a Pi session file name
// <timestamp>_<uuid>.jsonl; a name without the underscore is taken whole.
func SessionIDFromPath(path string) string {
	if path == "" {
		return ""
	}
	base := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if i := strings.LastIndex(base, "_"); i >= 0 {
		return base[i+1:]
	}
	return base
}

// AddNote says Pi loads project extensions and .pi/mcp.json only for a trusted project.
func (Agent) AddNote() string {
	return "Pi loads the Atlas extension and .pi/mcp.json once you trust this project in Pi."
}
