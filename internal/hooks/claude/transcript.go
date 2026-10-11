package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// reader reads Claude Code's JSONL transcript: one object per line with
// type user|assistant|system|…, message{role, content, id, model, usage},
// and the flags isMeta, isSidechain, isCompactSummary.
type reader struct{}

func (reader) Position(path string) (int, error) { return hooks.CountLines(path) }

// Settle waits for Claude's async writer: its own hook_progress line for the
// Stop hook lands in the transcript after the Stop payload is sent, else the
// file stays the same size for half a second.
func (reader) Settle(ctx context.Context, path string) {
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		tail := hooks.Tail(path, 4096)
		if bytes.Contains(tail, []byte(`hook_progress`)) && bytes.Contains(tail, []byte(`hooks claude-code stop`)) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	hooks.SettleFile(ctx, path, 500*time.Millisecond, 2*time.Second)
}

type line struct {
	Type             string          `json:"type"`
	IsMeta           bool            `json:"isMeta"`
	IsSidechain      bool            `json:"isSidechain"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Message          json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	ID      string          `json:"id"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *usage          `json:"usage"`
}

type usage struct {
	Input         int64 `json:"input_tokens"`
	Output        int64 `json:"output_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// droppedPrompt marks user text that is not something the user typed.
var droppedPrompt = regexp.MustCompile(`^\s*(<task-notification>|\[Request interrupted|<bash-input>|<bash-stdout>|<bash-stderr>|<local-command-caveat>|<command-name>|<system-reminder>)`)

// Read parses the lines from a position into a turn.
func (reader) Read(path string, from int) (*hooks.Turn, int, error) {
	lines, pos, err := hooks.LinesFrom(path, from)
	if err != nil {
		return nil, from, err
	}
	t := &hooks.Turn{}
	byID := map[string]usage{}
	var order []string
	tools := map[string]int{} // tool_use id → item index
	for _, raw := range lines {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var l line
		if json.Unmarshal(raw, &l) != nil || len(l.Message) == 0 {
			continue
		}
		var m message
		if json.Unmarshal(l.Message, &m) != nil {
			continue
		}
		blocks := contentBlocks(m.Content)
		switch {
		case l.Type == "user" || m.Role == "user":
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if !l.IsMeta && !l.IsSidechain && !l.IsCompactSummary && !droppedPrompt.MatchString(b.Text) && strings.TrimSpace(b.Text) != "" {
						t.Prompts = append(t.Prompts, b.Text)
					}
				case "tool_result":
					if i, ok := tools[b.ToolUseID]; ok {
						res := resultText(b.Content)
						t.Items[i].Tool.Result = &res
						if b.IsError {
							t.Items[i].Tool.Status = "failed"
						}
					}
				}
			}
		case l.Type == "assistant" || m.Role == "assistant":
			if l.IsSidechain {
				continue
			}
			if m.Model != "" && m.Model != "<synthetic>" {
				t.Model = m.Model
			}
			if m.Usage != nil && m.ID != "" {
				if prev, ok := byID[m.ID]; !ok || m.Usage.Output > prev.Output {
					if !ok {
						order = append(order, m.ID)
					}
					byID[m.ID] = *m.Usage
				}
			}
			for i, b := range blocks {
				id := m.ID
				if id == "" {
					id = "assistant"
				}
				switch b.Type {
				case "text":
					t.Items = appendRun(t.Items, id, i, "text", b.Text)
				case "thinking":
					t.Items = appendRun(t.Items, id, i, "thinking", b.Thinking)
				case "tool_use":
					if b.ID == "" {
						continue
					}
					tool := &record.Tool{NativeID: b.ID, Name: b.Name, Arguments: b.Input, Status: "completed"}
					tools[b.ID] = len(t.Items)
					t.Items = append(t.Items, hooks.Item{Tool: tool})
				}
			}
		}
	}
	if len(order) > 0 {
		t.HasUsage = true
		for _, id := range order {
			u := byID[id]
			t.Usage.Input += u.Input
			t.Usage.Output += u.Output
			t.Usage.CacheCreation += u.CacheCreation
			t.Usage.CacheRead += u.CacheRead
		}
	}
	return t, pos, nil
}

// appendRun adds a text or thinking run, merging into the previous run when
// it belongs to the same message and mode (streamed chunks share a message
// id).
func appendRun(items []hooks.Item, msgID string, i int, mode, body string) []hooks.Item {
	if strings.TrimSpace(body) == "" {
		return items
	}
	if n := len(items); n > 0 && items[n-1].Tool == nil && items[n-1].Mode == mode &&
		strings.HasPrefix(items[n-1].ID, msgID+"-") {
		items[n-1].Body += body
		return items
	}
	return append(items, hooks.Item{ID: msgID + "-" + strconv.Itoa(i), Mode: mode, Body: body})
}

// contentBlocks reads message.content: a string becomes one text block.
func contentBlocks(raw json.RawMessage) []block {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []block{{Type: "text", Text: s}}
		}
		return nil
	}
	var blocks []block
	_ = json.Unmarshal(raw, &blocks)
	return blocks
}

// resultText flattens a tool_result content: a string, or text blocks.
func resultText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// ideTags are the IDE context blocks Claude appends to a typed prompt.
var ideTags = regexp.MustCompile(`(?s)<(ide_opened_file|ide_selection|ide_diagnostics)>.*?</(ide_opened_file|ide_selection|ide_diagnostics)>`)

// StripIDE removes IDE context blocks from a prompt payload.
func StripIDE(prompt string) string { return strings.TrimSpace(ideTags.ReplaceAllString(prompt, "")) }
