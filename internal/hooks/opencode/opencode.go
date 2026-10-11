// Package opencode integrates OpenCode: a TypeScript plugin written whole
// into .opencode/plugins/atlas.ts forwards the session lifecycle, and the
// transcript is the agent's own export command (ADR-0013 §2: "or, for
// OpenCode, the agent's own export command"; docs/research/entire-cli.md §3).
package opencode

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/tryatlas/atlas-cli/internal/hooks"
	"github.com/tryatlas/atlas-cli/internal/project"
)

// ID is the agent's registry key.
const ID = "opencode"

func init() { hooks.Register(Agent{}) }

// Agent is the OpenCode integration.
type Agent struct{}

// Hook verbs, as the plugin invokes them.
const (
	VerbSessionStart = "session-start"
	VerbTurnStart    = "turn-start"
	VerbTurnEnd      = "turn-end"
	VerbCompaction   = "compaction"
	VerbSessionEnd   = "session-end"
)

// Plugin is the plugin OpenCode loads from .opencode/plugins/atlas.ts.
//
//go:embed plugin.ts
var Plugin string

// Marker is the header line that identifies the file as Atlas's, whatever
// its vintage.
const Marker = "Atlas plugin for OpenCode — written by atlas agents add"

// ID is the registry key.
func (Agent) ID() string { return ID }

// Name is the display name.
func (Agent) Name() string { return "OpenCode" }

// Binary is the agent's command.
func (Agent) Binary() string { return "opencode" }

// Verbs are the hook verbs the plugin invokes.
func (Agent) Verbs() []string {
	return []string{VerbSessionStart, VerbTurnStart, VerbTurnEnd, VerbCompaction, VerbSessionEnd}
}

// PluginPath is the plugin file under root. OpenCode loads every file in
// .opencode/plugins/, so the file is owned whole: no merging.
func PluginPath(root string) string {
	return filepath.Join(root, ".opencode", "plugins", "atlas.ts")
}

// Install writes the plugin when the file differs from the embedded render
// (or always, with force). 0644: OpenCode reads it.
func (Agent) Install(root string, force bool) (bool, error) {
	path := PluginPath(root)
	if !force {
		if cur, err := os.ReadFile(path); err == nil && string(cur) == Plugin { //nolint:gosec // G304: the plugin file Atlas owns
			return false, nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	tmp := path + ".atlas-tmp"
	if err := os.WriteFile(tmp, []byte(Plugin), 0o644); err != nil { //nolint:gosec // G306: OpenCode must read the plugin
		return false, err
	}
	return true, os.Rename(tmp, path)
}

// Uninstall removes the plugin, and .opencode/plugins when that leaves it
// empty.
func (Agent) Uninstall(root string) error {
	path := PluginPath(root)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_ = os.Remove(filepath.Dir(path)) // only an empty directory goes
	return nil
}

// State compares the plugin file with the embedded render: byte-equal is
// current, an older Atlas render (the marker) is outdated.
func (Agent) State(root string) hooks.InstallState {
	cur, err := os.ReadFile(PluginPath(root))
	switch {
	case err != nil:
		return hooks.Absent
	case string(cur) == Plugin:
		return hooks.Current
	case bytes.Contains(cur, []byte(Marker)):
		return hooks.Outdated
	}
	return hooks.Absent
}

// Detect reports an OpenCode project (.opencode/, opencode.json or
// opencode.jsonc in root) or opencode on PATH.
func (a Agent) Detect(root string) bool {
	for _, name := range []string{".opencode", "opencode.json", "opencode.jsonc"} {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return true
		}
	}
	_, err := exec.LookPath(a.Binary())
	return err == nil
}

// Reader reads the export.
func (Agent) Reader() hooks.Reader { return reader{} }

// Response prints nothing: the plugin discards stdout.
func (Agent) Response(hooks.Kind, string) []byte { return nil }

// ResumeCommand continues a session in OpenCode.
func (Agent) ResumeCommand(id string) string { return "opencode -s " + id }

// payload is what the plugin sends: session_id always, prompt and model at
// turn start, model at turn end.
type payload struct {
	SessionID string `json:"session_id"`
	Prompt    string `json:"prompt"`
	Model     string `json:"model"`
}

// Parse maps a hook verb and payload to an event. The payload names no
// transcript; the event names the export cache for the session, which the
// reader fills from `opencode export` (export.go). The cache lives under the
// Project the hook runs in, found the way the dispatcher finds it.
func (Agent) Parse(verb string, raw []byte) (*hooks.Event, error) {
	var p payload
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
	}
	ev := &hooks.Event{SessionID: p.SessionID, Model: p.Model}
	switch verb {
	case VerbSessionStart:
		ev.Kind = hooks.SessionStart
	case VerbTurnStart:
		ev.Kind = hooks.TurnStart
		ev.Prompt = StripReminders(p.Prompt)
	case VerbTurnEnd:
		ev.Kind = hooks.TurnEnd
	case VerbCompaction:
		ev.Kind = hooks.Compaction
	case VerbSessionEnd:
		ev.Kind = hooks.SessionEnd
	default:
		return nil, nil
	}
	if hooks.ValidSessionID(p.SessionID) {
		ev.Transcript = CachePath(projectRoot(), p.SessionID)
	}
	return ev, nil
}

// projectRoot is the Project the hook runs in (the plugin's cwd is the
// OpenCode directory), else the cwd itself.
func projectRoot() string {
	cwd, err := os.Getwd()
	if err != nil {
		return "."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if p, err := project.Discover(ctx, cwd); err == nil {
		return p.Root
	}
	return cwd
}

// AddNote says OpenCode installs the plugin SDK next to the plugin on first start.
func (Agent) AddNote() string {
	return "OpenCode installs @opencode-ai/plugin into .opencode/ (package.json, node_modules) the first time it loads the plugin."
}
