// Package antigravity integrates Google's Antigravity CLI (agy): hooks in
// .agents/hooks.json under a named "atlas" entry and its step JSONL
// transcript (docs/research/entire-cli.md §3).
//
// agy's hooks fire per model invocation, not per user prompt, and no session
// start or end hook exists. Observed live on agy 1.3.3: the hook payload
// names the model (modelName); the transcript is written step by step, the
// USER_INPUT step before PreInvocation fires and the last PLANNER_RESPONSE
// before Stop (older releases wrote it after Stop, which Settle still
// tolerates); each PLANNER_RESPONSE step carries input_tokens,
// output_tokens and cache_read_tokens; and PreToolUse/PostToolUse carry the
// call (toolCall) and stepIdx, the index of the step its output will be, one
// after the PLANNER_RESPONSE that made it, which is not yet on disk when
// PostToolUse fires, so no hook sees the tool's output.
package antigravity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"github.com/tryatlas/cli/internal/capture/record"
	"github.com/tryatlas/cli/internal/hooks"
)

// ID is the agent's registry key.
const ID = "antigravity"

// Key is the named entry Atlas owns in .agents/hooks.json.
const Key = "atlas"

func init() { hooks.Register(Agent{}) }

// Agent is the Antigravity integration.
type Agent struct{}

// Hook verbs, as the installed commands name them.
const (
	VerbPreInvocation = "pre-invocation"
	VerbPreToolUse    = "pre-tool-use"
	VerbPostToolUse   = "post-tool-use"
	VerbStop          = "stop"
)

// stopTimeout is the Stop handler's timeout in seconds: the turn-end hook
// waits for the late transcript and then reads it, which may exceed agy's
// default.
const stopTimeout = 300

// ID is the registry key.
func (Agent) ID() string { return ID }

// Name is the display name.
func (Agent) Name() string { return "Antigravity" }

// Binary is the agent's command.
func (Agent) Binary() string { return "agy" }

// Verbs are the hook verbs the installed config invokes.
func (Agent) Verbs() []string {
	return []string{VerbPreInvocation, VerbPreToolUse, VerbPostToolUse, VerbStop}
}

// HooksPath is the workspace hooks file agy loads for a trusted workspace.
func HooksPath(root string) string { return filepath.Join(root, ".agents", "hooks.json") }

// entry mirrors agy's hooks.json schema for one named entry. The event keys
// Atlas does not install are kept so a stale entry that carries them still
// compares as different and is replaced.
type entry struct {
	Enabled        *bool          `json:"enabled,omitempty"`
	PreToolUse     []toolHandler  `json:"PreToolUse,omitempty"`
	PostToolUse    []toolHandler  `json:"PostToolUse,omitempty"`
	PreInvocation  []plainHandler `json:"PreInvocation,omitempty"`
	PostInvocation []plainHandler `json:"PostInvocation,omitempty"`
	Stop           []plainHandler `json:"Stop,omitempty"`
}

// toolHandler is a PreToolUse/PostToolUse group: a matcher and its hooks.
type toolHandler struct {
	Matcher string              `json:"matcher,omitempty"`
	Hooks   []hooks.CommandHook `json:"hooks"`
}

// plainHandler is a PreInvocation/PostInvocation/Stop handler.
type plainHandler = hooks.CommandHook

// render is the entry Install writes; disabled carries over a user's
// "enabled": false so an install never silently re-arms a hook the user
// switched off.
func render(disabled bool) entry {
	e := entry{
		PreToolUse: []toolHandler{{Matcher: "*", Hooks: []hooks.CommandHook{
			{Type: "command", Command: hooks.Wrap(hooks.Command(ID, VerbPreToolUse))},
		}}},
		PostToolUse: []toolHandler{{Matcher: "*", Hooks: []hooks.CommandHook{
			{Type: "command", Command: hooks.Wrap(hooks.Command(ID, VerbPostToolUse))},
		}}},
		PreInvocation: []plainHandler{{Type: "command", Command: hooks.Wrap(hooks.Command(ID, VerbPreInvocation))}},
		Stop:          []plainHandler{{Type: "command", Command: hooks.Wrap(hooks.Command(ID, VerbStop)), Timeout: stopTimeout}},
	}
	if disabled {
		f := false
		e.Enabled = &f
	}
	return e
}

// current reports whether the installed entry already is what render gives,
// comparing both as compact JSON through the schema struct.
func current(raw json.RawMessage) bool {
	var have entry
	if json.Unmarshal(raw, &have) != nil {
		return false
	}
	want := render(have.Enabled != nil && !*have.Enabled)
	return bytes.Equal(hooks.Raw(have), hooks.Raw(want))
}

// Install replaces the whole "atlas" entry in .agents/hooks.json, keeping
// every other named entry as it is.
func (Agent) Install(root string, force bool) (bool, error) {
	path := HooksPath(root)
	file, err := hooks.LoadJSONObject(path)
	if err != nil {
		return false, err
	}
	disabled := false
	if raw, ok := file[Key]; ok {
		if !force && current(raw) {
			return false, nil
		}
		var have entry
		if json.Unmarshal(raw, &have) == nil && have.Enabled != nil && !*have.Enabled {
			disabled = true
		}
	}
	file[Key] = hooks.Raw(render(disabled))
	return true, hooks.SaveJSON(path, file, 0o644)
}

// Uninstall deletes the "atlas" entry; a file without one is left untouched.
func (Agent) Uninstall(root string) error {
	path := HooksPath(root)
	file, err := hooks.LoadJSONObject(path)
	if err != nil {
		return err
	}
	if _, ok := file[Key]; !ok {
		return nil
	}
	delete(file, Key)
	return hooks.SaveJSON(path, file, 0o644)
}

// State compares the "atlas" entry with what Install writes.
func (Agent) State(root string) hooks.InstallState {
	file, err := hooks.LoadJSONObject(HooksPath(root))
	if err != nil {
		return hooks.Absent
	}
	raw, ok := file[Key]
	switch {
	case !ok:
		return hooks.Absent
	case current(raw):
		return hooks.Current
	}
	return hooks.Outdated
}

// Detect reports a .agents directory in root or agy on PATH.
func (a Agent) Detect(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".agents")); err == nil {
		return true
	}
	_, err := exec.LookPath(a.Binary())
	return err == nil
}

// Reader reads the step JSONL transcript.
func (Agent) Reader() hooks.Reader { return reader{} }

// Response prints nothing: agy has no hook whose stdout reaches the user.
func (Agent) Response(hooks.Kind, string) []byte { return nil }

// ResumeCommand continues a conversation in agy.
func (Agent) ResumeCommand(id string) string { return "agy --conversation " + id }

// payload is the union of the fields read from agy's hook stdin. agy also
// sends artifactDirectoryPath, initialNumSteps, executionNum and
// terminationReason; only the ones below are used. modelName (e.g.
// "gemini-3.1-pro-low") is in every hook's payload on agy 1.3.3. stepIdx
// (Pre/PostToolUse) is the index of the step the tool's output will be
// written as; error (PostToolUse, Stop) is "" unless the tool or run failed.
type payload struct {
	ConversationID string   `json:"conversationId"`
	TranscriptPath string   `json:"transcriptPath"`
	WorkspacePaths []string `json:"workspacePaths"`
	InvocationNum  int      `json:"invocationNum"`
	ModelName      string   `json:"modelName"`
	FullyIdle      bool     `json:"fullyIdle"`
	StepIdx        *int     `json:"stepIdx"`
	Error          string   `json:"error"`
	ToolCall       toolCall `json:"toolCall"`
}

// Parse maps a hook verb and payload to an event:
//
//   - pre-invocation → TurnStart. On the first model call of a turn the
//     prompt is already in the transcript the payload names and no model
//     step follows it, so it is read from there and opens the turn at once;
//     a follow-up call within the turn (the transcript ends with model
//     steps) carries no prompt, and the dispatcher keeps the turn open;
//   - pre-tool-use → ToolUse with the target file of a write tool, nothing
//     for any other tool;
//   - post-tool-use → ToolUse with the completed call, keyed like the
//     transcript reader keys it so the turn-end read does not record it
//     twice; the payload has no output, only an error when the call failed;
//   - stop → TurnEnd when fullyIdle is true, nothing while background work
//     continues.
func (Agent) Parse(verb string, raw []byte) (*hooks.Event, error) {
	var p payload
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("antigravity %s payload: %w", verb, err)
		}
	}
	ev := &hooks.Event{SessionID: p.ConversationID, Transcript: p.TranscriptPath, Model: p.ModelName}
	if len(p.WorkspacePaths) > 0 {
		ev.Cwd = p.WorkspacePaths[0]
	}
	switch verb {
	case VerbPreInvocation:
		ev.Kind = hooks.TurnStart
		if p.TranscriptPath != "" {
			ev.Prompt = pendingPrompt(p.TranscriptPath)
		}
	case VerbPreToolUse:
		files := p.ToolCall.writeTargets()
		if len(files) == 0 {
			return nil, nil
		}
		ev.Kind = hooks.ToolUse
		ev.Files = files
	case VerbPostToolUse:
		if p.ToolCall.Name == "" || p.StepIdx == nil || *p.StepIdx < 1 {
			return nil, nil
		}
		ev.Kind = hooks.ToolUse
		ev.Tool = p.ToolCall.tool(callID(p.TranscriptPath, *p.StepIdx, p.ToolCall))
		if p.Error != "" {
			msg := p.Error
			ev.Tool.Status, ev.Tool.Result = "failed", &msg
		}
	case VerbStop:
		if !p.FullyIdle {
			return nil, nil
		}
		ev.Kind = hooks.TurnEnd
	default:
		return nil, nil
	}
	return ev, nil
}

// writeTools are agy's tools that change a file; TargetFile names it.
var writeTools = map[string]bool{
	"write_to_file":              true,
	"replace_file_content":       true,
	"multi_replace_file_content": true,
}

// toolKinds maps agy's tool names to the ACP kind the recorder classifies
// by: its alias table knows single words (write, read, grep), not agy's
// snake_case names, so without a kind every call records as Other and a
// write samples no file touch.
var toolKinds = map[string]string{
	"write_to_file":              "edit",
	"replace_file_content":       "edit",
	"multi_replace_file_content": "edit",
	"view_file":                  "read",
	"view_file_outline":          "read",
	"view_code_item":             "read",
	"view_content_chunk":         "read",
	"list_dir":                   "read",
	"grep_search":                "search",
	"find_by_name":               "search",
	"codebase_search":            "search",
	"run_command":                "execute",
	"send_command_input":         "execute",
	"command_status":             "execute",
	"read_url_content":           "fetch",
	"search_web":                 "fetch",
}

// writeTargets is the file a write tool call names, or nothing.
func (tc toolCall) writeTargets() []string {
	if !writeTools[tc.Name] {
		return nil
	}
	target, _ := decodeString(tc.Args["TargetFile"])
	if target == "" {
		return nil
	}
	return []string{target}
}

// callID is the id the transcript reader gives the call whose output step
// will be stepIdx: the PLANNER_RESPONSE step that made it (the last one
// before stepIdx, on disk before the tool runs) and the call's index in it.
// A planner step may hold several calls; their outputs follow it one step
// each in call order, so the index is the distance from the planner step.
// With no readable transcript or no step that fits, the call is taken as
// the only one of the step before.
func callID(path string, stepIdx int, tc toolCall) string {
	fallback := strconv.Itoa(stepIdx-1) + "-0"
	if path == "" {
		return fallback
	}
	var planner *step
	err := eachNonBlankLine(path, func(_ int, raw []byte) {
		var s step
		if json.Unmarshal(raw, &s) != nil || s.Type != "PLANNER_RESPONSE" || s.StepIndex >= stepIdx {
			return
		}
		planner = &s
	})
	if err != nil || planner == nil {
		return fallback
	}
	i := stepIdx - planner.StepIndex - 1
	if i < 0 || i >= len(planner.ToolCalls) || planner.ToolCalls[i].Name != tc.Name {
		return fallback
	}
	return strconv.Itoa(planner.StepIndex) + "-" + strconv.Itoa(i)
}

// tool is the call as the recorder takes it, under id: the ACP kind the
// alias table cannot derive from agy's snake_case names, the arguments with
// their double encoding undone, and the target file of a write tool as its
// location. The hook payload and the transcript describe one call the same
// way, so the dispatcher's keys (id, name + arguments, written path) match.
func (tc toolCall) tool(id string) *record.Tool {
	return &record.Tool{
		NativeID:  id,
		Name:      tc.Name,
		Kind:      toolKinds[tc.Name],
		Arguments: decodeArgs(tc.Args),
		Locations: tc.writeTargets(),
		Status:    "completed",
	}
}

// decodeString reads a string arg in either of agy's shapes: a plain JSON
// string, or (its wire format) a JSON string holding a JSON-encoded string.
// ok is false when the value is not a string at all.
func decodeString(raw json.RawMessage) (s string, ok bool) {
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return "", false
	}
	var inner string
	if json.Unmarshal([]byte(s), &inner) == nil {
		return inner, true
	}
	return s, true
}

// decodeArgs undoes the double encoding of a tool call's string args and
// re-marshals the rest as it came, so the recorded arguments read like the
// documented shape.
func decodeArgs(args map[string]json.RawMessage) json.RawMessage {
	if len(args) == 0 {
		return nil
	}
	out := make(map[string]any, len(args))
	for k, raw := range args {
		if s, ok := decodeString(raw); ok {
			out[k] = s
			continue
		}
		var v any
		if json.Unmarshal(raw, &v) == nil {
			out[k] = v
		}
	}
	return hooks.Raw(out)
}
