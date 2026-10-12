package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tryatlas/cli/internal/capture/record"
	"github.com/tryatlas/cli/internal/hooks"
)

// reader reads Cursor's JSONL transcript: one {role, message:{content:[…]}}
// per line in Claude's content-block shape, without message ids, model or
// usage; a {type:"turn_ended"} marker may follow a turn. Tool blocks carry
// no id in the files seen so far, so one is made from the line.
type reader struct{}

func (reader) Position(path string) (int, error) { return hooks.CountLines(path) }

// Settle waits for the file to appear and hold something, then for its size
// to stay still: Cursor's writer may lag the stop hook.
func (reader) Settle(ctx context.Context, path string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	hooks.SettleFile(ctx, path, 300*time.Millisecond, 2*time.Second)
}

type line struct {
	Role    string          `json:"role"`
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
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

// Read parses the lines from a position into a turn. Usage and model are
// not in the file; the stop payload carries them.
func (reader) Read(path string, from int) (*hooks.Turn, int, error) {
	lines, pos, err := hooks.LinesFrom(path, from)
	if err != nil {
		return nil, from, err
	}
	t := &hooks.Turn{}
	tools := map[string]int{} // tool_use id → item index
	for n, raw := range lines {
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
		role := l.Role
		if role == "" {
			role = m.Role
		}
		if role == "" {
			role = l.Type
		}
		blocks := contentBlocks(m.Content)
		msgID := "line" + strconv.Itoa(from+n)
		switch role {
		case "user":
			for _, b := range blocks {
				switch b.Type {
				case "text":
					if p := UserText(b.Text); p != "" {
						t.Prompts = append(t.Prompts, p)
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
			for i, b := range blocks {
				switch b.Type {
				case "text":
					t.Items = appendRun(t.Items, msgID, i, "text", b.Text)
				case "thinking":
					t.Items = appendRun(t.Items, msgID, i, "thinking", b.Thinking)
				case "tool_use":
					id := b.ID
					if id == "" {
						id = msgID + "-" + strconv.Itoa(i)
					}
					tool := &record.Tool{NativeID: id, Name: b.Name, Arguments: b.Input, Status: "completed"}
					tools[id] = len(t.Items)
					t.Items = append(t.Items, hooks.Item{Tool: tool})
				}
			}
		}
	}
	return t, pos, nil
}

// appendRun adds a text or thinking run, merging adjacent blocks of one mode
// within the same line.
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

var (
	// leadingTimestamp is the send-time line Cursor puts before the query;
	// only a leading one goes, so a timestamp the user pasted stays.
	leadingTimestamp = regexp.MustCompile(`\A\s*<timestamp(\s[^>]*)?>[^<]*</timestamp\s*>`)
	// userQuery is the wrapper around what the user typed.
	userQuery = regexp.MustCompile(`(?s)\A\s*<user_query>(.*)</user_query>\s*\z`)
)

// UserText is what the user typed, out of the text block Cursor records: the
// leading timestamp dropped and the user_query wrapper opened. Text without
// the wrapper is returned trimmed.
func UserText(s string) string {
	s = leadingTimestamp.ReplaceAllString(s, "")
	if m := userQuery.FindStringSubmatch(s); m != nil {
		s = m[1]
	}
	return strings.TrimSpace(s)
}
