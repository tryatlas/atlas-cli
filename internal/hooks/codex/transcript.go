package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// reader reads Codex's JSONL rollout: one object per line with
// {timestamp, type: session_meta|response_item|event_msg|turn_context,
// payload}. Reasoning items carry encrypted_content bound to the session;
// it is never read or stored.
type reader struct{}

func (reader) Position(path string) (int, error) { return hooks.CountLines(path) }

// Settle waits for the rollout to stop growing: Codex flushes it before Stop
// fires, so this is a short guard against a late write.
func (reader) Settle(ctx context.Context, path string) {
	hooks.SettleFile(ctx, path, 300*time.Millisecond, 2*time.Second)
}

type line struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// item is a response_item payload; the fields each type uses are a subset.
type item struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Summary   json.RawMessage `json:"summary"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
	Status    string          `json:"status"`
}

type part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type eventMsg struct {
	Type string `json:"type"`
	Info struct {
		Total *usage `json:"total_token_usage"`
	} `json:"info"`
}

// usage is Codex's cumulative token snapshot.
type usage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
}

type turnContext struct {
	Model string `json:"model"`
}

// droppedPrompt marks user-role text Codex itself injects around the typed
// prompt: context blocks such as <environment_context>, <user_instructions>
// and <recommended_plugins>, which may share one message, so the whole text
// is tag-wrapped; and the AGENTS.md instructions.
var droppedPrompt = regexp.MustCompile(`(?s)^\s*(<[a-z_]+[^>]*>.*</[a-z_]+>\s*|# AGENTS\.md instructions.*)$`)

// Read parses the lines from a position into a turn. Usage is the delta of
// the cumulative token_count snapshots, so the whole file is scanned once:
// the last snapshot before the position is the baseline.
func (reader) Read(path string, from int) (*hooks.Turn, int, error) {
	lines, pos, err := hooks.LinesFrom(path, 0)
	if err != nil {
		return nil, from, err
	}
	t := &hooks.Turn{}
	var base, last *usage
	tools := map[string]int{} // call_id → item index
	for i, raw := range lines {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var l line
		if json.Unmarshal(raw, &l) != nil || len(l.Payload) == 0 {
			continue
		}
		switch l.Type {
		case "event_msg":
			var e eventMsg
			if json.Unmarshal(l.Payload, &e) != nil || e.Type != "token_count" || e.Info.Total == nil {
				continue
			}
			if i < from {
				base = e.Info.Total
			} else {
				last = e.Info.Total
			}
		case "turn_context":
			if i < from {
				continue
			}
			var tc turnContext
			if json.Unmarshal(l.Payload, &tc) == nil && tc.Model != "" {
				t.Model = tc.Model
			}
		case "response_item":
			if i < from {
				continue
			}
			var it item
			if json.Unmarshal(l.Payload, &it) != nil {
				continue
			}
			readItem(t, tools, it, i)
		}
	}
	if last != nil {
		t.HasUsage = true
		t.Usage = delta(base, last)
	}
	return t, pos, nil
}

// readItem adds one response item to the turn.
func readItem(t *hooks.Turn, tools map[string]int, it item, idx int) {
	switch it.Type {
	case "message":
		switch it.Role {
		case "user":
			for _, p := range parts(it.Content) {
				if p.Type == "input_text" && strings.TrimSpace(p.Text) != "" && !droppedPrompt.MatchString(p.Text) {
					t.Prompts = append(t.Prompts, p.Text)
				}
			}
		case "assistant":
			var texts []string
			for _, p := range parts(it.Content) {
				if p.Type == "output_text" && p.Text != "" {
					texts = append(texts, p.Text)
				}
			}
			addRun(t, itemID(it.ID, "assistant", idx), "text", strings.Join(texts, ""))
		}
	case "reasoning":
		// Only the plain-text summary and content parts; encrypted_content
		// is skipped.
		var texts []string
		for _, p := range append(parts(it.Summary), parts(it.Content)...) {
			if (p.Type == "summary_text" || p.Type == "reasoning_text") && strings.TrimSpace(p.Text) != "" {
				texts = append(texts, p.Text)
			}
		}
		addRun(t, itemID(it.ID, "reasoning", idx), "thinking", strings.Join(texts, "\n\n"))
	case "function_call":
		if it.CallID == "" {
			return
		}
		addTool(t, tools, &record.Tool{NativeID: it.CallID, Name: it.Name, Arguments: jsonArgs(it.Arguments), Status: "completed"})
	case "custom_tool_call":
		if it.CallID == "" {
			return
		}
		if it.Name == "apply_patch" {
			addTool(t, tools, patchTool(it.CallID, it.Input))
			return
		}
		// Code mode (Codex 0.153): the model edits files from a JavaScript
		// script run by the exec tool, as tools.apply_patch("…"), and runs
		// shell commands as tools.exec_command({"cmd": …}). Each patch
		// becomes its own apply_patch call so the edit and its files are
		// attributed; a script that only runs one command becomes that
		// exec_command call, whose cmd is what the PostToolUse hook reported
		// as the command (codex.go toolOf); the script itself stays only
		// when it did more, and then it keeps the call id and the output.
		patches, others := CodeModePatches(it.Input)
		if len(patches) == 0 {
			if args, ok := CodeModeCommand(it.Input); ok {
				addTool(t, tools, &record.Tool{NativeID: it.CallID, Name: "exec_command", Arguments: args, Status: "completed"})
				return
			}
		}
		if len(patches) == 0 || others {
			addTool(t, tools, &record.Tool{NativeID: it.CallID, Name: it.Name, Status: "completed", Arguments: hooks.Raw(map[string]string{"input": it.Input})})
		}
		for i, p := range patches {
			id := it.CallID
			if others || i > 0 {
				id += "#patch" + strconv.Itoa(i+1)
			}
			addTool(t, tools, patchTool(id, p))
		}
	case "local_shell_call":
		if it.CallID == "" {
			return
		}
		addTool(t, tools, &record.Tool{NativeID: it.CallID, Name: "shell", Arguments: it.Action, Status: "completed"})
	case "function_call_output", "custom_tool_call_output":
		if i, ok := tools[it.CallID]; ok {
			res := outputText(it.Output)
			t.Items[i].Tool.Result = &res
		}
	}
}

// patchTool is a completed apply_patch call with the files it names.
func patchTool(id, patch string) *record.Tool {
	return &record.Tool{NativeID: id, Name: "apply_patch", Status: "completed", Arguments: hooks.Raw(map[string]string{"patch": patch}), Locations: PatchFiles(patch)}
}

func addTool(t *hooks.Turn, tools map[string]int, tool *record.Tool) {
	tools[tool.NativeID] = len(t.Items)
	t.Items = append(t.Items, hooks.Item{Tool: tool})
}

func addRun(t *hooks.Turn, id, mode, body string) {
	if strings.TrimSpace(body) == "" {
		return
	}
	t.Items = append(t.Items, hooks.Item{ID: id, Mode: mode, Body: body})
}

// itemID is the item's own id, else a stable one from its line.
func itemID(id, kind string, idx int) string {
	if id != "" {
		return id
	}
	return kind + "-" + strconv.Itoa(idx)
}

// parts decodes a content or summary list; anything else is empty.
func parts(raw json.RawMessage) []part {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var out []part
	_ = json.Unmarshal(raw, &out)
	return out
}

// jsonArgs keeps a function call's arguments when they are JSON (Codex
// writes them as a JSON string), else wraps the text.
func jsonArgs(s string) json.RawMessage {
	if json.Valid([]byte(s)) && strings.HasPrefix(strings.TrimSpace(s), "{") {
		return json.RawMessage(s)
	}
	return hooks.Raw(map[string]string{"input": s})
}

// outputText flattens a tool output: a string, an object with a content
// string or parts, or a list of parts.
func outputText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	switch raw[0] {
	case '"':
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	case '{':
		var o struct {
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(raw, &o) == nil && len(o.Content) > 0 {
			return outputText(o.Content)
		}
	case '[':
		var ps []string
		for _, p := range parts(raw) {
			if p.Text != "" {
				ps = append(ps, p.Text)
			}
		}
		return strings.Join(ps, "\n")
	}
	return string(raw)
}

// delta is the turn's usage: the last cumulative snapshot minus the
// baseline. Codex's input_tokens include the cached ones, so the reported
// input is the uncached part.
func delta(base, last *usage) (u store.TokenTotals) {
	in, cached, wrote, out, reason := last.Input, last.Cached, last.CacheWrite, last.Output, last.Reasoning
	if base != nil {
		in -= base.Input
		cached -= base.Cached
		wrote -= base.CacheWrite
		out -= base.Output
		reason -= base.Reasoning
	}
	u.Input = max(in-cached, 0)
	u.CacheRead = max(cached, 0)
	u.CacheCreation = max(wrote, 0)
	u.Output = max(out, 0)
	u.Reasoning = max(reason, 0)
	return u
}
