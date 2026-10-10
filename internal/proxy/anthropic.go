package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
)

// The Anthropic Messages dialect (SPEC §7.2), parsed from copies of the
// bytes after they were forwarded.

// Request is the part of POST /v1/messages the recorder reads.
type Request struct {
	Model    string            `json:"model"`
	System   json.RawMessage   `json:"system"`
	Messages []Message         `json:"messages"`
	Tools    []json.RawMessage `json:"tools"`
	Metadata struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

// Message is one conversation message; content is a string or blocks.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// Block is one content block of a request or response.
type Block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

// Blocks reads a message's content as blocks (a string is one text block).
func (m Message) Blocks() []Block {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []Block{{Type: "text", Text: s}}
	}
	var bs []Block
	_ = json.Unmarshal(m.Content, &bs)
	return bs
}

// ParseRequest decodes a Messages request body.
func ParseRequest(body []byte) (*Request, bool) {
	var r Request
	if json.Unmarshal(body, &r) != nil || len(r.Messages) == 0 {
		return nil, false
	}
	return &r, true
}

// Usage is one response's token counts.
type Usage struct {
	Input         int64 `json:"input_tokens"`
	Output        int64 `json:"output_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
}

// merge keeps the largest value of each field: message_start carries the
// input counts, the last message_delta the final output count.
func (u *Usage) merge(o Usage) {
	u.Input = max(u.Input, o.Input)
	u.Output = max(u.Output, o.Output)
	u.CacheCreation = max(u.CacheCreation, o.CacheCreation)
	u.CacheRead = max(u.CacheRead, o.CacheRead)
}

// Response is a decoded Messages response, streamed or not.
type Response struct {
	ID         string
	Model      string
	Blocks     []Block
	StopReason string
	Usage      Usage
}

// ParseResponse decodes a JSON or SSE Messages response.
func ParseResponse(contentType string, body []byte) (*Response, bool) {
	if strings.HasPrefix(contentType, "text/event-stream") {
		return parseSSE(body)
	}
	var m struct {
		ID         string  `json:"id"`
		Type       string  `json:"type"`
		Model      string  `json:"model"`
		Content    []Block `json:"content"`
		StopReason string  `json:"stop_reason"`
		Usage      Usage   `json:"usage"`
	}
	if json.Unmarshal(body, &m) != nil || m.Type != "message" {
		return nil, false
	}
	return &Response{ID: m.ID, Model: m.Model, Blocks: m.Content, StopReason: m.StopReason, Usage: m.Usage}, true
}

func parseSSE(body []byte) (*Response, bool) {
	r := &Response{}
	inputs := map[int]*strings.Builder{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), 64<<20)
	ok := false
	for sc.Scan() {
		line := sc.Bytes()
		data, found := bytes.CutPrefix(line, []byte("data:"))
		if !found {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage Usage  `json:"usage"`
			} `json:"message"`
			ContentBlock Block `json:"content_block"`
			Delta        struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage Usage `json:"usage"`
		}
		if json.Unmarshal(bytes.TrimSpace(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			ok = true
			r.ID, r.Model = ev.Message.ID, ev.Message.Model
			r.Usage.merge(ev.Message.Usage)
		case "content_block_start":
			for len(r.Blocks) <= ev.Index {
				r.Blocks = append(r.Blocks, Block{})
			}
			b := ev.ContentBlock
			if b.Type == "tool_use" {
				b.Input = nil // the start carries {}; the deltas carry the input
				inputs[ev.Index] = &strings.Builder{}
			}
			r.Blocks[ev.Index] = b
		case "content_block_delta":
			if ev.Index >= len(r.Blocks) {
				continue
			}
			b := &r.Blocks[ev.Index]
			switch ev.Delta.Type {
			case "text_delta":
				b.Text += ev.Delta.Text
			case "thinking_delta":
				b.Thinking += ev.Delta.Thinking
			case "input_json_delta":
				if sb := inputs[ev.Index]; sb != nil {
					sb.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				r.StopReason = ev.Delta.StopReason
			}
			r.Usage.merge(ev.Usage)
		}
	}
	for i, sb := range inputs {
		s := strings.TrimSpace(sb.String())
		if s == "" {
			s = "{}"
		}
		if json.Valid([]byte(s)) {
			r.Blocks[i].Input = json.RawMessage(s)
		}
	}
	return r, ok
}

// ToolResultText joins a tool_result's text content.
func ToolResultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []Block
	_ = json.Unmarshal(raw, &bs)
	var parts []string
	for _, b := range bs {
		if b.Type == "text" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// PromptText is the user's text in a message: its text blocks without the
// <system-reminder> blocks Claude Code injects (host machinery).
func PromptText(m Message) string {
	var parts []string
	for _, b := range m.Blocks() {
		if b.Type != "text" {
			continue
		}
		t := stripReminders(b.Text)
		if strings.TrimSpace(t) != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func stripReminders(s string) string {
	for {
		i := strings.Index(s, "<system-reminder>")
		if i < 0 {
			return s
		}
		j := strings.Index(s[i:], "</system-reminder>")
		if j < 0 {
			return s[:i]
		}
		s = s[:i] + s[i+j+len("</system-reminder>"):]
	}
}

// HasToolResult reports a user message carrying tool results.
func HasToolResult(m Message) bool {
	for _, b := range m.Blocks() {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}
