// Package codex integrates OpenAI Codex CLI: hooks in .codex/hooks.json and
// its JSONL rollout transcript (docs/research/entire-cli.md §3).
package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// ID is the agent's registry key.
const ID = "codex"

func init() { hooks.Register(Agent{}) }

// Agent is the Codex integration.
type Agent struct{}

// Hook verbs, as the installed commands name them.
const (
	VerbSessionStart = "session-start"
	VerbSessionEnd   = "session-end"
	VerbPromptSubmit = "user-prompt-submit"
	VerbStop         = "stop"
	VerbPostToolUse  = "post-tool-use"
)

// Hook timeouts in seconds. Codex clamps SessionEnd to 3 s and warns at
// every startup when a config asks for more; the other hooks keep 30 s.
const (
	hookTimeout       = 30
	sessionEndTimeout = 3
)

// ID is the registry key.
func (Agent) ID() string { return ID }

// Name is the display name.
func (Agent) Name() string { return "Codex" }

// Binary is the agent's command.
func (Agent) Binary() string { return "codex" }

// Verbs are the hook verbs the installed config invokes.
func (Agent) Verbs() []string {
	return []string{VerbSessionStart, VerbSessionEnd, VerbPromptSubmit, VerbStop, VerbPostToolUse}
}

// HooksPath is the project-level Codex hooks file.
func HooksPath(root string) string {
	return filepath.Join(root, ".codex", "hooks.json")
}

func (a Agent) shaped(root string) hooks.ClaudeShaped {
	return hooks.ClaudeShaped{Path: HooksPath(root), NullMatcher: true, Types: []hooks.ClaudeHook{
		{Type: "SessionStart", Command: hooks.WrapWithMessage(hooks.Command(ID, VerbSessionStart), hooks.MissingMessage), Timeout: hookTimeout},
		{Type: "SessionEnd", Command: hooks.Wrap(hooks.Command(ID, VerbSessionEnd)), Timeout: sessionEndTimeout},
		{Type: "UserPromptSubmit", Command: hooks.Wrap(hooks.Command(ID, VerbPromptSubmit)), Timeout: hookTimeout},
		{Type: "Stop", Command: hooks.Wrap(hooks.Command(ID, VerbStop)), Timeout: hookTimeout},
		{Type: "PostToolUse", Command: hooks.Wrap(hooks.Command(ID, VerbPostToolUse)), Timeout: hookTimeout},
	}}
}

// Install writes the hooks into .codex/hooks.json.
func (a Agent) Install(root string, force bool) (bool, error) { return a.shaped(root).Install(force) }

// Uninstall removes them.
func (a Agent) Uninstall(root string) error { return a.shaped(root).Uninstall() }

// State compares the hooks file with what Install writes.
func (a Agent) State(root string) hooks.InstallState { return a.shaped(root).State() }

// Detect reports a .codex directory in root or codex on PATH.
func (a Agent) Detect(root string) bool {
	if _, err := os.Stat(filepath.Join(root, ".codex")); err == nil {
		return true
	}
	_, err := exec.LookPath(a.Binary())
	return err == nil
}

// Reader reads the JSONL rollout.
func (Agent) Reader() hooks.Reader { return reader{} }

// Response renders a SessionStart systemMessage, which Codex shows the user.
func (Agent) Response(k hooks.Kind, message string) []byte {
	if k != hooks.SessionStart || message == "" {
		return nil
	}
	b, _ := json.Marshal(map[string]string{"systemMessage": message})
	return append(b, '\n')
}

// ResumeCommand continues a session in Codex.
func (Agent) ResumeCommand(id string) string { return "codex resume " + id }

// payload is the common shape of Codex hook input. transcript_path is null
// under --ephemeral, which decodes to "".
type payload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	Cwd            string          `json:"cwd"`
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	ToolUseID      string          `json:"tool_use_id"`
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
		ev.Prompt = p.Prompt
	case VerbStop:
		ev.Kind = hooks.TurnEnd
	case VerbSessionEnd:
		ev.Kind = hooks.SessionEnd
	case VerbPostToolUse:
		tool, files := p.toolOf()
		if tool == nil && len(files) == 0 {
			return nil, nil
		}
		ev.Kind = hooks.ToolUse
		ev.ToolUseID = p.ToolUseID
		ev.Tool = tool
		for _, f := range files {
			if !filepath.IsAbs(f) && p.Cwd != "" {
				f = filepath.Join(p.Cwd, f)
			}
			ev.Files = append(ev.Files, f)
		}
	default:
		return nil, nil
	}
	return ev, nil
}

// toolOf is the completed call a PostToolUse payload describes, shaped so
// the dispatcher recognises the same call when it reads the rollout at turn
// end (toolKeys in dispatch.go), plus the files an apply_patch wrote for the
// git attribution.
//
// Codex 0.153 reports each tool the model used, in code mode the inner call
// of the exec script: apply_patch with the envelope under command, and Bash
// with the shell command under command. Its tool_use_id (exec-<uuid>) is not
// the rollout's call_id (call_<hex>), so the id never matches and the other
// keys must:
//
//   - apply_patch (also under its Write and Edit aliases, and a code-mode
//     exec script that only calls tools.apply_patch once): the shape of
//     transcript.go's patchTool, name "apply_patch", arguments {"patch": …}
//     and the envelope's paths as locations, so both the arguments hash and
//     every written path match;
//   - Bash {"command": …}: the command text, which transcript.go reads out
//     of a lone tools.exec_command({"cmd": …}) script (CodeModeCommand) or a
//     shell call's command, matches as the shell key;
//   - a code-mode exec script that does more: name "exec" with arguments
//     {"input": script}, the rollout's custom_tool_call shape, so the
//     arguments hash matches; the patches it holds still name their files;
//   - anything else: the payload's name and arguments, re-encoded like the
//     rollout's (canonJSON), so the arguments hash matches a function_call
//     with the same arguments.
//
// The response is a string or an object; either is kept as text.
func (p payload) toolOf() (*record.Tool, []string) {
	var tool *record.Tool
	if p.ToolUseID != "" && p.ToolName != "" {
		tool = &record.Tool{NativeID: p.ToolUseID, Name: p.ToolName, Arguments: canonJSON(p.ToolInput), Status: "completed"}
		if r := bytes.TrimSpace(p.ToolResponse); len(r) > 0 && string(r) != "null" {
			var str string
			if json.Unmarshal(r, &str) != nil {
				str = string(r)
			}
			tool.Result = &str
		}
	}
	var files []string
	switch {
	case isApplyPatch(p.ToolName):
		patch := patchText(p.ToolInput)
		files = PatchFiles(patch)
		if tool != nil && len(files) > 0 {
			tool.Name, tool.Arguments, tool.Locations = patchArgs(patch, files)
		}
	case p.ToolName == "exec":
		script := patchText(p.ToolInput)
		patches, others := CodeModePatches(script)
		files = PatchFiles(strings.Join(patches, "\n"))
		if tool == nil || script == "" {
			break
		}
		if len(patches) == 1 && !others {
			tool.Name, tool.Arguments, tool.Locations = patchArgs(patches[0], files)
		} else {
			tool.Arguments = hooks.Raw(map[string]string{"input": script})
		}
	}
	return tool, files
}

// patchArgs is the apply_patch shape shared with transcript.go's patchTool.
func patchArgs(patch string, files []string) (string, json.RawMessage, []string) {
	return "apply_patch", hooks.Raw(map[string]string{"patch": patch}), files
}

// canonJSON re-encodes a JSON value with Go's encoder (sorted object keys,
// no HTML escaping) so the same arguments hash the same whether they came
// from a hook payload or the rollout; anything that is not JSON is kept.
func canonJSON(raw json.RawMessage) json.RawMessage {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	return hooks.Raw(v)
}

// isApplyPatch recognises Codex's file tool: apply_patch, plus the Write and
// Edit matcher aliases Codex registers for Claude-style hook configs.
func isApplyPatch(name string) bool {
	switch name {
	case "apply_patch", "Write", "Edit":
		return true
	}
	return false
}

// codeModeCall is a tool call in a code-mode script: tools.<name>(.
var codeModeCall = regexp.MustCompile(`\btools\.([A-Za-z_][A-Za-z0-9_]*)\s*\(\s*`)

// CodeModePatches pulls the apply_patch envelopes out of a code-mode script.
// Codex 0.153 runs its tools from a JavaScript program handed to the exec
// tool, where a file edit is tools.apply_patch("…") with the envelope as a
// string literal. It also reports whether the script called any other tool
// (or a patch it could not decode), so a caller knows the script did more
// than edit files.
func CodeModePatches(script string) (patches []string, others bool) {
	for _, m := range codeModeCall.FindAllStringSubmatchIndex(script, -1) {
		if script[m[2]:m[3]] != "apply_patch" {
			others = true
			continue
		}
		patch, ok := jsString(script, m[1])
		if !ok || len(PatchFiles(patch)) == 0 {
			others = true
			continue
		}
		patches = append(patches, patch)
	}
	return patches, others
}

// CodeModeCommand is the argument object of a code-mode script that calls
// tools.exec_command({…}) exactly once and no other tool, when that object
// is a JSON literal with a cmd string; the object is re-encoded with sorted
// keys. Anything else is not a lone command.
func CodeModeCommand(script string) (json.RawMessage, bool) {
	calls := codeModeCall.FindAllStringSubmatchIndex(script, -1)
	if len(calls) != 1 || script[calls[0][2]:calls[0][3]] != "exec_command" {
		return nil, false
	}
	var obj map[string]any
	if err := json.NewDecoder(strings.NewReader(script[calls[0][1]:])).Decode(&obj); err != nil {
		return nil, false
	}
	if cmd, ok := obj["cmd"].(string); !ok || strings.TrimSpace(cmd) == "" {
		return nil, false
	}
	return hooks.Raw(obj), true
}

// jsString decodes the JavaScript string literal that starts at s[i] with a
// ", ' or ` quote. A template literal holding ${…} and an unterminated or
// malformed literal fail.
func jsString(s string, i int) (string, bool) {
	if i >= len(s) || (s[i] != '"' && s[i] != '\'' && s[i] != '`') {
		return "", false
	}
	q := s[i]
	var b strings.Builder
	for j := i + 1; j < len(s); j++ {
		c := s[j]
		switch {
		case c == q:
			return b.String(), true
		case q == '`' && c == '$' && j+1 < len(s) && s[j+1] == '{':
			return "", false
		case c != '\\':
			b.WriteByte(c)
			continue
		}
		j++
		if j >= len(s) {
			return "", false
		}
		switch e := s[j]; e {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		case '0':
			b.WriteByte(0)
		case '\n':
			// a line continuation
		case '\r':
			if j+1 < len(s) && s[j+1] == '\n' {
				j++
			}
		case 'x':
			r, n, ok := hexRune(s[j+1:], 2)
			if !ok {
				return "", false
			}
			b.WriteRune(r)
			j += n
		case 'u':
			r, n, ok := unicodeEscape(s[j+1:])
			if !ok {
				return "", false
			}
			j += n
			// A UTF-16 surrogate pair is two \u escapes.
			if utf16.IsSurrogate(r) && j+2 < len(s) && s[j+1] == '\\' && s[j+2] == 'u' {
				if lo, n2, ok := unicodeEscape(s[j+3:]); ok {
					r = utf16.DecodeRune(r, lo)
					j += 2 + n2
				}
			}
			b.WriteRune(r)
		default:
			b.WriteByte(e)
		}
	}
	return "", false
}

// unicodeEscape decodes what follows \u: HHHH or {H…}. It returns the rune
// and the number of bytes consumed.
func unicodeEscape(s string) (rune, int, bool) {
	if strings.HasPrefix(s, "{") {
		end := strings.IndexByte(s, '}')
		if end < 2 {
			return 0, 0, false
		}
		r, _, ok := hexRune(s[1:end], end-1)
		return r, end + 1, ok
	}
	return hexRune(s, 4)
}

// hexRune reads exactly n hex digits as a code point.
func hexRune(s string, n int) (rune, int, bool) {
	if n <= 0 || len(s) < n {
		return 0, 0, false
	}
	var r rune
	for _, c := range []byte(s[:n]) {
		var d rune
		switch {
		case c >= '0' && c <= '9':
			d = rune(c - '0')
		case c >= 'a' && c <= 'f':
			d = rune(c-'a') + 10
		case c >= 'A' && c <= 'F':
			d = rune(c-'A') + 10
		default:
			return 0, 0, false
		}
		r = r*16 + d
		if r > utf8.MaxRune {
			return 0, 0, false
		}
	}
	return r, n, true
}

// patchText pulls the patch envelope out of a PostToolUse tool_input: Codex
// puts it under command; patch and input are accepted too, as is a bare
// string.
func patchText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, k := range []string{"command", "patch", "input"} {
		if json.Unmarshal(m[k], &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

// Envelope lines of Codex's apply_patch grammar. A rename is "*** Update
// File: old" followed by "*** Move to: new".
var (
	patchFileLine = regexp.MustCompile(`^\*\*\* (Add|Update|Delete) File: (.+)$`)
	patchMoveLine = regexp.MustCompile(`^\*\*\* Move to: (.+)$`)
)

// PatchFiles lists the paths an apply_patch envelope touches, in order of
// first mention and without duplicates: added, updated and deleted files,
// and both halves of a rename.
func PatchFiles(patch string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for _, l := range strings.Split(patch, "\n") {
		l = strings.TrimRight(l, "\r")
		if m := patchFileLine.FindStringSubmatch(l); m != nil {
			add(m[2])
		} else if m := patchMoveLine.FindStringSubmatch(l); m != nil {
			add(m[1])
		}
	}
	return out
}

// AddNote says Codex runs a project's hooks only after the user approves them.
func (Agent) AddNote() string {
	return "Codex asks you to approve the Atlas hooks the first time you start `codex` here; until then `codex exec` skips them silently. The memory server needs the project trusted too."
}
