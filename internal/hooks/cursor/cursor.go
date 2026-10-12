// Package cursor integrates Cursor, the IDE and its agent CLI: hooks in
// .cursor/hooks.json and the Claude-style JSONL transcript Cursor keeps
// under ~/.cursor/projects (docs/research/entire-cli.md §3).
//
// Observed live against cursor-agent 2026.10.01: the interactive TUI fires
// sessionStart (at the first prompt, not at launch), beforeSubmitPrompt, the
// tool hooks afterFileEdit and afterShellExecution as each call completes,
// stop and sessionEnd. The prompt opens the turn, each tool hook records its
// call into the open turn, and stop closes it with the transcript's text.
// Print mode (cursor-agent -p) fires only sessionStart, the tool hooks and
// sessionEnd: no beforeSubmitPrompt and no stop, so a -p run yields a
// session with no turns until sessionEnd captures the whole transcript as
// one. The TUI keeps a hook's stdin open; ReadPayload returns after one JSON
// value, so Atlas never blocks it.
package cursor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// ID is the agent's registry key.
const ID = "cursor"

func init() { hooks.Register(Agent{}) }

// Agent is the Cursor integration.
type Agent struct{}

// Hook verbs, as the installed commands name them.
const (
	VerbSessionStart  = "session-start"
	VerbBeforePrompt  = "before-submit-prompt"
	VerbAfterFileEdit = "after-file-edit"
	VerbAfterShell    = "after-shell-execution"
	VerbStop          = "stop"
	VerbSessionEnd    = "session-end"
	VerbPreCompact    = "pre-compact"
)

// hook is one Cursor hook event and the verb Atlas runs for it.
type hook struct {
	Event string // Cursor's camelCase hook name
	Verb  string
}

// installed are the hooks Atlas writes, in lifecycle order. Cursor hooks
// take no matcher; the stdin payload shares its common fields across events.
// Cursor has no generic post-tool hook: afterFileEdit and afterShellExecution
// are the two that name a completed call (afterMCPExecution is left to the
// transcript).
var installed = []hook{
	{"sessionStart", VerbSessionStart},
	{"beforeSubmitPrompt", VerbBeforePrompt},
	{"afterFileEdit", VerbAfterFileEdit},
	{"afterShellExecution", VerbAfterShell},
	{"stop", VerbStop},
	{"sessionEnd", VerbSessionEnd},
	{"preCompact", VerbPreCompact},
}

// ID is the registry key.
func (Agent) ID() string { return ID }

// Name is the display name.
func (Agent) Name() string { return "Cursor" }

// Binary is the agent CLI's command. The CLI also installs as agent, which
// Detect accepts too.
func (Agent) Binary() string { return "cursor-agent" }

// Verbs are the hook verbs the installed config invokes.
func (Agent) Verbs() []string {
	out := make([]string, 0, len(installed))
	for _, h := range installed {
		out = append(out, h.Verb)
	}
	return out
}

// HooksPath is the project hooks file Cursor reads: .cursor/hooks.json.
func HooksPath(root string) string { return filepath.Join(root, ".cursor", "hooks.json") }

// command is the wrapped command installed for a verb.
func command(verb string) string { return hooks.Wrap(hooks.Command(ID, verb)) }

// ── Hook file ────────────────────────────────────────────────────────────────
//
// The file is {"version":1,"hooks":{"<event>":[{"command":"…"}, …]}}. It is
// edited as raw JSON so version, unknown hook events and every entry Atlas
// does not own survive untouched; an entry is Atlas's when hooks.Owned says
// so of its command.

// entryCommand reads the command of one hook entry; "" when it has none.
func entryCommand(raw json.RawMessage) string {
	var e struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(raw, &e)
	return e.Command
}

// loadHooks reads the file and its hooks object.
func loadHooks(path string) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	root, err := hooks.LoadJSONObject(path)
	if err != nil {
		return nil, nil, err
	}
	hs := map[string]json.RawMessage{}
	if raw, ok := root["hooks"]; ok && len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &hs); err != nil {
			return nil, nil, fmt.Errorf("%s: hooks is not an object: %w", path, err)
		}
	}
	return root, hs, nil
}

// pruneOwned drops the Atlas entries of one event that are not wanted (all of
// them when all is set). It reports whether anything was dropped.
func pruneOwned(raw json.RawMessage, wanted map[string]bool, all bool) ([]json.RawMessage, bool) {
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) != nil {
		return nil, false // not a list: the user's value, left alone
	}
	keep := list[:0:0]
	pruned := false
	for _, e := range list {
		if c := entryCommand(e); hooks.Owned(c) && (all || !wanted[c]) {
			pruned = true
			continue
		}
		keep = append(keep, e)
	}
	return keep, pruned
}

func hasCommand(list []json.RawMessage, cmd string) bool {
	for _, e := range list {
		if entryCommand(e) == cmd {
			return true
		}
	}
	return false
}

// Install writes the Atlas hooks into .cursor/hooks.json, pruning stale
// Atlas entries; force rewrites every Atlas entry.
func (Agent) Install(root string, force bool) (bool, error) {
	path := HooksPath(root)
	obj, hs, err := loadHooks(path)
	if err != nil {
		return false, err
	}
	wanted := map[string]bool{}
	for _, h := range installed {
		wanted[command(h.Verb)] = true
	}
	changed := false
	for event, raw := range hs {
		out, pruned := pruneOwned(raw, wanted, force)
		if !pruned {
			continue
		}
		changed = true
		if len(out) == 0 {
			delete(hs, event)
		} else {
			hs[event] = hooks.Raw(out)
		}
	}
	for _, h := range installed {
		var list []json.RawMessage
		if raw, ok := hs[h.Event]; ok {
			if err := json.Unmarshal(raw, &list); err != nil {
				return false, fmt.Errorf("%s: hooks.%s is not a list: %w", path, h.Event, err)
			}
		}
		cmd := command(h.Verb)
		if hasCommand(list, cmd) {
			continue
		}
		list = append(list, hooks.Raw(map[string]string{"command": cmd}))
		hs[h.Event] = hooks.Raw(list)
		changed = true
	}
	if !changed {
		return false, nil
	}
	if _, ok := obj["version"]; !ok {
		obj["version"] = json.RawMessage(`1`)
	}
	obj["hooks"] = hooks.Raw(hs)
	return true, hooks.SaveJSON(path, obj, 0o600)
}

// Uninstall removes every Atlas entry; the rest of the file is kept.
func (Agent) Uninstall(root string) error {
	path := HooksPath(root)
	obj, hs, err := loadHooks(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	changed := false
	for event, raw := range hs {
		out, pruned := pruneOwned(raw, nil, true)
		if !pruned {
			continue
		}
		changed = true
		if len(out) == 0 {
			delete(hs, event)
		} else {
			hs[event] = hooks.Raw(out)
		}
	}
	if !changed {
		return nil
	}
	if len(hs) == 0 {
		delete(obj, "hooks")
	} else {
		obj["hooks"] = hooks.Raw(hs)
	}
	return hooks.SaveJSON(path, obj, 0o600)
}

// State compares .cursor/hooks.json with what Install writes.
func (Agent) State(root string) hooks.InstallState {
	_, hs, err := loadHooks(HooksPath(root))
	if err != nil {
		return hooks.Absent
	}
	some, all := false, true
	for _, h := range installed {
		var list []json.RawMessage
		_ = json.Unmarshal(hs[h.Event], &list)
		if hasCommand(list, command(h.Verb)) {
			some = true
		} else {
			all = false
		}
	}
	switch {
	case all:
		return hooks.Current
	case some:
		return hooks.Outdated
	}
	// An Atlas entry with an old command counts as outdated.
	for _, raw := range hs {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			continue
		}
		for _, e := range list {
			if hooks.Owned(entryCommand(e)) {
				return hooks.Outdated
			}
		}
	}
	return hooks.Absent
}

// Detect reports a .cursor directory in root, or the agent CLI on PATH
// under either of its names.
func (a Agent) Detect(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".cursor")); err == nil {
		return true
	}
	// Only cursor-agent counts: a bare "agent" on PATH is often another tool.
	_, err := exec.LookPath(a.Binary())
	return err == nil
}

// Reader reads the JSONL transcript.
func (Agent) Reader() hooks.Reader { return reader{} }

// Response is nil: Cursor shows nothing a hook prints at sessionStart.
func (Agent) Response(hooks.Kind, string) []byte { return nil }

// ResumeCommand continues a conversation in the agent CLI.
func (Agent) ResumeCommand(id string) string { return "cursor-agent --resume " + id }

// ── Payloads ─────────────────────────────────────────────────────────────────

// payload is the shape of Cursor hook input: the common fields, plus the
// prompt of beforeSubmitPrompt and the status and per-turn token counts of
// stop. The CLI (2026.10) sends transcript_path as null at sessionStart and
// at the first beforeSubmitPrompt of a conversation, and names the file from
// the first tool hook on; the IDE names it throughout.
type payload struct {
	ConversationID string   `json:"conversation_id"`
	GenerationID   string   `json:"generation_id"` // changes with each user message
	Model          string   `json:"model"`
	WorkspaceRoots []string `json:"workspace_roots"`
	TranscriptPath string   `json:"transcript_path"`
	Prompt         string   `json:"prompt"`
	// stop: completed, error or aborted (the user interrupted). The token
	// counts come only with completed.
	Status string `json:"status"`
	// stop: input_tokens is the whole input, cache portions included.
	InputTokens      *json.Number `json:"input_tokens"`
	OutputTokens     *json.Number `json:"output_tokens"`
	CacheReadTokens  *json.Number `json:"cache_read_tokens"`
	CacheWriteTokens *json.Number `json:"cache_write_tokens"`
	// afterFileEdit: the absolute path and the replacements applied to it. A
	// new file arrives as one edit with an empty old_string.
	FilePath string `json:"file_path"`
	Edits    []edit `json:"edits"`
	// afterShellExecution: the command and, when Cursor captured it, its
	// output.
	Command string  `json:"command"`
	Output  *string `json:"output"`
}

// edit is one replacement of an afterFileEdit payload.
type edit struct {
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// nativeID names a hook-reported call: Cursor's tool hooks carry no call id
// (and its transcript gives tool_use blocks none either), so the id is the
// generation plus a hash of what the call did. The dispatcher matches the
// transcript's copy by written path or command text, not by this id.
func (p payload) nativeID(kind string, parts ...string) string {
	h := store.SHA256Hex([]byte(strings.Join(parts, "\x00")))[:16]
	if p.GenerationID == "" {
		return kind + "-" + h
	}
	return p.GenerationID + "-" + kind + "-" + h
}

// editTool is the completed call an afterFileEdit payload describes, in the
// shape Cursor's transcript gives the same call so the dispatcher's keys
// line up: a new file is Write {path, contents}; anything else is Edit
// {path, edits} with the single replacement also flattened to old_string and
// new_string so the recorder can synthesise its patch.
func (p payload) editTool() *record.Tool {
	if p.FilePath == "" {
		return nil
	}
	edits, _ := json.Marshal(p.Edits)
	t := &record.Tool{NativeID: p.nativeID("edit", p.FilePath, string(edits)), Status: "completed"}
	if len(p.Edits) == 1 && p.Edits[0].OldString == "" {
		t.Name = "Write"
		t.Arguments = hooks.Raw(map[string]any{"path": p.FilePath, "contents": p.Edits[0].NewString})
		return t
	}
	t.Name = "Edit"
	args := map[string]any{"path": p.FilePath, "edits": p.Edits}
	if len(p.Edits) == 1 {
		args["old_string"], args["new_string"] = p.Edits[0].OldString, p.Edits[0].NewString
	}
	t.Arguments = hooks.Raw(args)
	return t
}

// shellTool is the completed call an afterShellExecution payload describes:
// Shell {command}, as the transcript names it, with the output as its result.
func (p payload) shellTool() *record.Tool {
	if strings.TrimSpace(p.Command) == "" {
		return nil
	}
	out := ""
	if p.Output != nil {
		out = *p.Output
	}
	t := &record.Tool{NativeID: p.nativeID("shell", p.Command, out), Name: "Shell", Status: "completed",
		Arguments: hooks.Raw(map[string]string{"command": p.Command}), Result: p.Output}
	return t
}

func (p payload) cwd() string {
	if len(p.WorkspaceRoots) > 0 {
		return p.WorkspaceRoots[0]
	}
	return ""
}

// modelDefault is the model id Cursor's lifecycle hooks send for Auto (the
// CLI config pairs modelId "default" with the display name "Auto"). The
// model that actually answered is named only by afterAgentThought, which
// Atlas does not install.
const modelDefault = "default"

// model is the payload's model as Atlas records it: Auto under its own name.
func (p payload) model() string {
	if p.Model == modelDefault {
		return "auto"
	}
	return p.Model
}

// usage converts the stop payload's counts to a total: the fresh input is
// what is left of input_tokens once the cache portions are taken out. Nil
// when the payload carries no count at all.
func (p payload) usage() *store.TokenTotals {
	if p.InputTokens == nil && p.OutputTokens == nil && p.CacheReadTokens == nil && p.CacheWriteTokens == nil {
		return nil
	}
	read, write := count(p.CacheReadTokens), count(p.CacheWriteTokens)
	fresh := count(p.InputTokens) - read - write
	if fresh < 0 {
		fresh = 0
	}
	return &store.TokenTotals{Input: fresh, Output: count(p.OutputTokens), CacheRead: read, CacheCreation: write}
}

// count reads a token field; a missing or unreadable one is 0.
func count(n *json.Number) int64 {
	if n == nil {
		return 0
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	if f, err := n.Float64(); err == nil {
		return int64(f)
	}
	return 0
}

// Parse maps a hook verb and payload to an event.
func (Agent) Parse(verb string, raw []byte) (*hooks.Event, error) {
	var p payload
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
	}
	ev := &hooks.Event{SessionID: p.ConversationID, Cwd: p.cwd(), Model: p.model()}
	switch verb {
	case VerbSessionStart:
		ev.Kind = hooks.SessionStart
	case VerbBeforePrompt:
		ev.Kind = hooks.TurnStart
		ev.Prompt = p.Prompt
	case VerbAfterFileEdit:
		ev.Kind = hooks.ToolUse
		if ev.Tool = p.editTool(); ev.Tool == nil {
			return nil, nil
		}
	case VerbAfterShell:
		ev.Kind = hooks.ToolUse
		if ev.Tool = p.shellTool(); ev.Tool == nil {
			return nil, nil
		}
	case VerbStop:
		ev.Kind = hooks.TurnEnd
		ev.Usage = p.usage()
		ev.TurnEndFailed = p.Status == "error"
	case VerbSessionEnd:
		ev.Kind = hooks.SessionEnd
	case VerbPreCompact:
		ev.Kind = hooks.Compaction
	default:
		return nil, nil
	}
	ev.Transcript = TranscriptPath(p.TranscriptPath, p.cwd(), p.ConversationID)
	return ev, nil
}

// ── Transcript location ──────────────────────────────────────────────────────

// nonAlphanumeric is what Cursor replaces in a workspace path to name its
// project directory.
var nonAlphanumeric = regexp.MustCompile(`[^A-Za-z0-9]`)

// ProjectSlug is Cursor's name for a workspace under ~/.cursor/projects: the
// path without its leading / and every other character replaced by -.
func ProjectSlug(workspace string) string {
	return nonAlphanumeric.ReplaceAllString(strings.TrimLeft(workspace, "/"), "-")
}

// TranscriptDir is where Cursor keeps a workspace's transcripts:
// ~/.cursor/projects/<slug>/agent-transcripts. CURSOR_DATA_DIR is ignored on
// purpose: Cursor's file service writes under the real home whatever the
// variable says.
func TranscriptDir(workspace string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "projects", ProjectSlug(workspace), "agent-transcripts"), nil
}

// TranscriptPath is the transcript of a conversation: the path the payload
// names when it names one, else the one Cursor's own layout fixes for the
// payload's workspace root and conversation id.
//
// Deriving the path is the one allowed exception to ADR-0013 §2, which lets
// a hook read only the transcript file the agent's payload names: the Cursor
// CLI sends transcript_path as null until the first tool hook of a
// conversation, and the file it writes is determined by nothing but the
// payload's own ids. The IDE and the CLI (2026.10, observed live) nest the
// file, <dir>/<id>/<id>.jsonl; older CLIs wrote it flat, <dir>/<id>.jsonl.
// The nested path wins when it exists, then the flat one; a file that does
// not exist yet is reported at the nested path. Nothing else under ~/.cursor
// is read.
func TranscriptPath(named, workspace, conversationID string) string {
	if named != "" {
		return named
	}
	if workspace == "" || !hooks.ValidSessionID(conversationID) {
		return ""
	}
	// Cursor names the directory after the resolved path; try that first,
	// then the path as the payload spelled it.
	roots := []string{workspace}
	if r, err := filepath.EvalSymlinks(workspace); err == nil && r != workspace {
		roots = []string{r, workspace}
	}
	var first string
	for _, w := range roots {
		dir, err := TranscriptDir(w)
		if err != nil {
			continue
		}
		nested := filepath.Join(dir, conversationID, conversationID+".jsonl")
		flat := filepath.Join(dir, conversationID+".jsonl")
		for _, p := range []string{nested, flat} {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
		if first == "" {
			first = nested
		}
	}
	return first
}

// AddNote says Cursor asks before using the project's MCP server.
func (Agent) AddNote() string {
	return "Cursor asks to approve the atlas_memory MCP server the first time it starts here (or pass --approve-mcps)."
}
