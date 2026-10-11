package droid

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// reader reads Droid's JSONL transcript: one envelope per line,
// {type:"message", id, message:{role, id, model, content, usage}} with
// Claude-style content blocks. Lines of any other type (session_start, …)
// are skipped.
type reader struct{}

func (reader) Position(path string) (int, error) { return hooks.CountLines(path) }

// Settle waits until Droid's writer has gone quiet: the file stays the same
// size for half a second.
func (reader) Settle(ctx context.Context, path string) {
	hooks.SettleFile(ctx, path, 500*time.Millisecond, 2*time.Second)
}

type envelope struct {
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	Message json.RawMessage `json:"message"`
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

// Read parses the lines from a position into a turn. Streaming leaves
// several rows per message id, so usage is deduped per id keeping the row
// with the most output tokens, and runs of one message merge.
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
		var e envelope
		if json.Unmarshal(raw, &e) != nil || e.Type != "message" || len(e.Message) == 0 {
			continue
		}
		var m message
		if json.Unmarshal(e.Message, &m) != nil {
			continue
		}
		id := m.ID
		if id == "" {
			id = e.ID
		}
		blocks := contentBlocks(m.Content)
		switch m.Role {
		case "user":
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if strings.TrimSpace(b.Text) != "" {
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
		case "assistant":
			if m.Model != "" {
				t.Model = CleanModel(m.Model)
			}
			if m.Usage != nil && id != "" {
				if prev, ok := byID[id]; !ok || m.Usage.Output > prev.Output {
					if !ok {
						order = append(order, id)
					}
					byID[id] = *m.Usage
				}
			}
			if id == "" {
				id = "assistant"
			}
			for i, b := range blocks {
				switch b.Type {
				case "text":
					t.Items = appendRun(t.Items, id, i, "text", b.Text)
				case "thinking":
					t.Items = appendRun(t.Items, id, i, "thinking", b.Thinking)
				case "tool_use":
					if b.ID == "" {
						continue
					}
					if _, seen := tools[b.ID]; seen {
						continue // a streamed repeat of the same call
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
// it belongs to the same message and mode.
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
