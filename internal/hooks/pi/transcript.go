package pi

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

// reader reads Pi's session file: JSONL whose lines form a tree. Every line
// has id and parentId; a /fork or /clone keeps both branches in the one
// file, so only the active branch (the chain from the last message back to
// the root) is content, which is why Read parses the whole file and then
// keeps the lines at or after the position.
type reader struct{}

func (reader) Position(path string) (int, error) { return hooks.CountLines(path) }

// Settle waits for Pi to finish flushing the turn.
func (reader) Settle(ctx context.Context, path string) {
	hooks.SettleFile(ctx, path, 300*time.Millisecond, 2*time.Second)
}

type entry struct {
	Type     string          `json:"type"`
	ID       string          `json:"id"`
	ParentID *string         `json:"parentId"`
	Message  json.RawMessage `json:"message"`
}

type message struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	Usage      *usage          `json:"usage"`
	Model      string          `json:"model"`
	ToolCallID string          `json:"toolCallId"`
	ToolName   string          `json:"toolName"`
	IsError    bool            `json:"isError"`
}

type usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	Reasoning  int64 `json:"reasoning"`
}

// item is one element of message.content: text, thinking or toolCall on
// an assistant message, text on a user or toolResult message.
type item struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Read parses the file, resolves the active branch, and turns the branch's
// lines from the position into a turn.
func (reader) Read(path string, from int) (*hooks.Turn, int, error) {
	lines, pos, err := hooks.LinesFrom(path, 0)
	if err != nil {
		return nil, from, err
	}
	entries := make([]entry, len(lines))
	for i, raw := range lines {
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		_ = json.Unmarshal(raw, &entries[i])
	}
	active := activeBranch(entries)
	t := &hooks.Turn{}
	tools := map[string]int{} // tool call id → item index
	for i, e := range entries {
		if e.Type != "message" || len(e.Message) == 0 || (active != nil && !active[e.ID]) {
			continue
		}
		var m message
		if json.Unmarshal(e.Message, &m) != nil {
			continue
		}
		if m.Role == "assistant" && m.Model != "" {
			t.Model = m.Model // the branch's latest model, even before the window
		}
		if i < from {
			continue
		}
		switch m.Role {
		case "user":
			for _, c := range contentItems(m.Content) {
				if c.Type == "text" && strings.TrimSpace(c.Text) != "" {
					t.Prompts = append(t.Prompts, c.Text)
				}
			}
		case "assistant":
			if m.Usage != nil {
				t.HasUsage = true
				t.Usage.Input += m.Usage.Input
				t.Usage.Output += m.Usage.Output
				t.Usage.CacheRead += m.Usage.CacheRead
				t.Usage.CacheCreation += m.Usage.CacheWrite
				t.Usage.Reasoning += m.Usage.Reasoning
			}
			for j, c := range contentItems(m.Content) {
				switch c.Type {
				case "text":
					t.Items = appendRun(t.Items, e.ID, j, "text", c.Text)
				case "thinking":
					t.Items = appendRun(t.Items, e.ID, j, "thinking", c.Thinking)
				case "toolCall":
					if c.ID == "" {
						continue
					}
					tools[c.ID] = len(t.Items)
					t.Items = append(t.Items, hooks.Item{Tool: &record.Tool{
						NativeID: c.ID, Name: c.Name, Arguments: c.Arguments, Status: "completed",
					}})
				}
			}
		case "toolResult":
			k, ok := tools[m.ToolCallID]
			if !ok {
				continue
			}
			res := resultText(m.Content)
			t.Items[k].Tool.Result = &res
			if m.IsError {
				t.Items[k].Tool.Status = "failed"
			}
		}
	}
	return t, pos, nil
}

// activeBranch is the set of entry ids on the chain from the last message
// back to the root, or nil when the file has no tree (every line is active).
func activeBranch(entries []entry) map[string]bool {
	parentOf := map[string]string{}
	last := ""
	tree := false
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		if e.ParentID != nil {
			parentOf[e.ID] = *e.ParentID
			if *e.ParentID != "" {
				tree = true
			}
		}
		if e.Type == "message" {
			last = e.ID
		}
	}
	if !tree || last == "" {
		return nil
	}
	active := map[string]bool{}
	for cur := last; cur != "" && !active[cur]; cur = parentOf[cur] {
		active[cur] = true
	}
	return active
}

// appendRun adds a text or thinking run, skipping blank ones.
func appendRun(items []hooks.Item, msgID string, i int, mode, body string) []hooks.Item {
	if strings.TrimSpace(body) == "" {
		return items
	}
	return append(items, hooks.Item{ID: msgID + "-" + strconv.Itoa(i), Mode: mode, Body: body})
}

// contentItems reads message.content: a string is one text item.
func contentItems(raw json.RawMessage) []item {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return []item{{Type: "text", Text: s}}
		}
		return nil
	}
	var items []item
	_ = json.Unmarshal(raw, &items)
	return items
}

// resultText flattens a toolResult's content: a string, or its text items.
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
	var items []item
	if json.Unmarshal(raw, &items) != nil {
		return string(raw)
	}
	var parts []string
	for _, c := range items {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}
