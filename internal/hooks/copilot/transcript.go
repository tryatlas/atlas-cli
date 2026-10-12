package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/tryatlas/cli/internal/capture/record"
	"github.com/tryatlas/cli/internal/hooks"
)

// reader reads Copilot's events.jsonl: one event per line, {type, data, id,
// timestamp, parentId}. The event types read here are user.message,
// assistant.message, tool.execution_start, tool.execution_complete,
// session.usage_record and session.model_change. The session.shutdown totals
// are written only after the sessionEnd hook returns, so they are never seen
// by a hook; the per-request usage_record (1.0.95) is written before the
// message it paid for, so a turn's usage is their sum. Older releases put
// outputTokens on assistant.message instead; that is read when no
// usage_record was seen.
//
// Since the postToolUse hook delivers each call as it completes, the items
// read here are mostly the dispatcher's dedupe material at turn end: their
// arguments take the same shape as the hook's (toolArguments) so the two
// hash alike, and fileEdits give the written paths the hook's patch paths
// are matched against. Calls the hook did not report stay as items.
type reader struct{}

// Position is the line count.
func (reader) Position(path string) (int, error) { return hooks.CountLines(path) }

// Settle waits for the file to stop growing: Copilot has no marker of its
// own for the end of a turn's writes.
func (reader) Settle(ctx context.Context, path string) {
	hooks.SettleFile(ctx, path, 300*time.Millisecond, 2*time.Second)
}

type event struct {
	Type string          `json:"type"`
	ID   string          `json:"id"`
	Data json.RawMessage `json:"data"`
}

type userMessage struct {
	Content string `json:"content"`
}

type assistantMessage struct {
	MessageID    string `json:"messageId"`
	Content      string `json:"content"`
	Model        string `json:"model"`
	OutputTokens *int64 `json:"outputTokens"`
}

// usageRecord is one model request's usage: {"usage":{model, inputTokens,
// outputTokens, cacheReadTokens, cacheWriteTokens, reasoningTokens, …}}.
type usageRecord struct {
	Usage struct {
		Model            string `json:"model"`
		InputTokens      int64  `json:"inputTokens"`
		OutputTokens     int64  `json:"outputTokens"`
		CacheReadTokens  int64  `json:"cacheReadTokens"`
		CacheWriteTokens int64  `json:"cacheWriteTokens"`
		ReasoningTokens  int64  `json:"reasoningTokens"`
	} `json:"usage"`
}

// fileEdit is one entry of a completion's fileEdits: {"path":…,"kind":"create"|…}.
type fileEdit struct {
	Path string `json:"path"`
}

type toolEvent struct {
	ToolCallID string          `json:"toolCallId"`
	ID         string          `json:"id"`
	ToolName   string          `json:"toolName"`
	Arguments  json.RawMessage `json:"arguments"`
	Result     json.RawMessage `json:"result"`
	Success    *bool           `json:"success"`
	Error      json.RawMessage `json:"error"`
	FileEdits  []fileEdit      `json:"fileEdits"`
	Telemetry  struct {
		Properties           telemetryProps `json:"properties"`
		RestrictedProperties telemetryProps `json:"restrictedProperties"`
	} `json:"toolTelemetry"`
}

// telemetryProps carries filePaths: the files the tool changed, as a
// JSON-encoded string array in a string (or, tolerated, a plain array).
type telemetryProps struct {
	FilePaths json.RawMessage `json:"filePaths"`
}

type modelChange struct {
	NewModel string `json:"newModel"`
}

// Read parses the lines from a position into a turn.
func (reader) Read(path string, from int) (*hooks.Turn, int, error) {
	lines, pos, err := hooks.LinesFrom(path, from)
	if err != nil {
		return nil, from, err
	}
	t := &hooks.Turn{}
	tools := map[string]int{} // tool call id → item index
	var changeModel, messageModel string
	var messageOutput int64 // assistant.message outputTokens, older releases
	var hasMessageOutput, hasRecords bool
	for _, raw := range lines {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var e event
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		switch e.Type {
		case "user.message":
			var u userMessage
			if json.Unmarshal(e.Data, &u) == nil && strings.TrimSpace(u.Content) != "" {
				t.Prompts = append(t.Prompts, u.Content)
			}
		case "assistant.message":
			var m assistantMessage
			if json.Unmarshal(e.Data, &m) != nil {
				continue
			}
			if m.Model != "" {
				messageModel = m.Model
			}
			if m.OutputTokens != nil {
				hasMessageOutput = true
				messageOutput += *m.OutputTokens
			}
			if strings.TrimSpace(m.Content) != "" {
				id := m.MessageID
				if id == "" {
					id = e.ID
				}
				t.Items = append(t.Items, hooks.Item{ID: id, Mode: "text", Body: m.Content})
			}
		case "tool.execution_start":
			var te toolEvent
			if json.Unmarshal(e.Data, &te) != nil {
				continue
			}
			id := te.callID(e.ID)
			if id == "" {
				continue
			}
			tools[id] = len(t.Items)
			t.Items = append(t.Items, hooks.Item{Tool: &record.Tool{
				NativeID: id, Name: te.ToolName, Arguments: toolArguments(te.Arguments), Status: "running",
			}})
		case "tool.execution_complete":
			var te toolEvent
			if json.Unmarshal(e.Data, &te) != nil {
				continue
			}
			id := te.callID(e.ID)
			if id == "" {
				continue
			}
			i, ok := tools[id]
			if !ok {
				// The start was before the turn's offset (or never written):
				// the call still counts, with what the completion says.
				i = len(t.Items)
				tools[id] = i
				t.Items = append(t.Items, hooks.Item{Tool: &record.Tool{NativeID: id, Name: te.ToolName, Arguments: toolArguments(te.Arguments)}})
			}
			tool := t.Items[i].Tool
			tool.Status = "completed"
			if (te.Success != nil && !*te.Success) || isSet(te.Error) {
				tool.Status = "failed"
			}
			if res, ok := resultText(te.Result); ok {
				tool.Result = &res
			}
			if files := te.files(); len(files) > 0 {
				tool.Locations = files
			}
		case "session.usage_record":
			var ur usageRecord
			if json.Unmarshal(e.Data, &ur) != nil {
				continue
			}
			hasRecords = true
			u := ur.Usage
			t.Usage.Input += u.InputTokens
			t.Usage.Output += u.OutputTokens
			t.Usage.CacheRead += u.CacheReadTokens
			t.Usage.CacheCreation += u.CacheWriteTokens
			t.Usage.Reasoning += u.ReasoningTokens
			if u.Model != "" && messageModel == "" {
				messageModel = u.Model
			}
		case "session.model_change":
			var mc modelChange
			if json.Unmarshal(e.Data, &mc) == nil && mc.NewModel != "" {
				changeModel = mc.NewModel
			}
		}
	}
	switch {
	case hasRecords:
		t.HasUsage = true
	case hasMessageOutput:
		t.HasUsage = true
		t.Usage.Output = messageOutput
	}
	// A message names the model that answered; model_change may say "auto".
	t.Model = messageModel
	if t.Model == "" {
		t.Model = changeModel
	}
	return t, pos, nil
}

// callID is the tool call's id: toolCallId, else the data's id, else the
// event's own id.
func (te toolEvent) callID(eventID string) string {
	switch {
	case te.ToolCallID != "":
		return te.ToolCallID
	case te.ID != "":
		return te.ID
	}
	return eventID
}

// files is what the tool changed: the completion's fileEdits (1.0.95), else
// the telemetry's filePaths from properties (older releases) or
// restrictedProperties (current ones).
func (te toolEvent) files() []string {
	var out []string
	seen := map[string]bool{}
	for _, fe := range te.FileEdits {
		if fe.Path != "" && !seen[fe.Path] {
			seen[fe.Path] = true
			out = append(out, fe.Path)
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, raw := range []json.RawMessage{te.Telemetry.Properties.FilePaths, te.Telemetry.RestrictedProperties.FilePaths} {
		if paths := decodePaths(raw); len(paths) > 0 {
			return paths
		}
	}
	return nil
}

// decodePaths reads a filePaths value: a JSON string holding an encoded
// array, or an array.
func decodePaths(raw json.RawMessage) []string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return nil
		}
		raw = []byte(s)
	}
	var paths []string
	if json.Unmarshal(raw, &paths) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, p := range paths {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// resultText flattens a tool result: a string as is, anything else as its
// JSON. It reports false when there is no result.
func resultText(raw json.RawMessage) (string, bool) {
	raw = bytes.TrimSpace(raw)
	if !isSet(raw) {
		return "", false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s, true
		}
	}
	return string(raw), true
}

// isSet reports a present, non-null JSON value.
func isSet(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && string(raw) != "null"
}
