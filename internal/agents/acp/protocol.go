package acp

import (
	"encoding/json"
	"strings"
)

// Wire shapes (evidence-acp.md §3). Every struct is tolerant: unknown fields
// are ignored, and raw JSON is kept where the recorder or the UI needs it.

// ProtocolVersion is what the client sends and the minimum it accepts.
const ProtocolVersion = 1

// InitializeParams is exactly what the desktop sends (§3.1).
func InitializeParams(version string) map[string]any {
	return map[string]any{
		"protocolVersion": ProtocolVersion,
		"clientInfo":      map[string]any{"name": "atlas", "version": version},
		"clientCapabilities": map[string]any{
			"fs":          map[string]any{"readTextFile": true, "writeTextFile": true},
			"terminal":    true,
			"auth":        map[string]any{"terminal": true},
			"session":     map[string]any{"configOptions": map[string]any{"boolean": map[string]any{}}},
			"elicitation": map[string]any{"form": map[string]any{}, "url": map[string]any{}},
			"_meta":       map[string]any{"terminal_output": true, "terminal-auth": true},
		},
	}
}

// Capabilities are the agentCapabilities fields the client consults.
type Capabilities struct {
	LoadSession     bool `json:"loadSession"`
	MCPCapabilities struct {
		HTTP bool `json:"http"`
		SSE  bool `json:"sse"`
	} `json:"mcpCapabilities"`
	SessionCapabilities struct {
		List                  json.RawMessage `json:"list"`
		Delete                json.RawMessage `json:"delete"`
		Resume                json.RawMessage `json:"resume"`
		Close                 json.RawMessage `json:"close"`
		AdditionalDirectories json.RawMessage `json:"additionalDirectories"`
	} `json:"sessionCapabilities"`
	Auth struct {
		Logout json.RawMessage `json:"logout"`
	} `json:"auth"`
	PromptCapabilities struct {
		Image           bool `json:"image"`
		Audio           bool `json:"audio"`
		EmbeddedContext bool `json:"embeddedContext"`
	} `json:"promptCapabilities"`
}

// advertised: a capability object is present (and not false or null).
func advertised(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s != "" && s != "null" && s != "false"
}

// CanList reads sessionCapabilities.list.
func (c Capabilities) CanList() bool { return advertised(c.SessionCapabilities.List) }

// CanDelete reads sessionCapabilities.delete.
func (c Capabilities) CanDelete() bool { return advertised(c.SessionCapabilities.Delete) }

// CanResume reads sessionCapabilities.resume.
func (c Capabilities) CanResume() bool { return advertised(c.SessionCapabilities.Resume) }

// CanClose reads sessionCapabilities.close.
func (c Capabilities) CanClose() bool { return advertised(c.SessionCapabilities.Close) }

// CanLogout reads auth.logout.
func (c Capabilities) CanLogout() bool { return advertised(c.Auth.Logout) }

// ExtraDirs reads sessionCapabilities.additionalDirectories.
func (c Capabilities) ExtraDirs() bool {
	return advertised(c.SessionCapabilities.AdditionalDirectories)
}

// AgentInfo names the agent.
type AgentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

// InitializeResult is what initialize returns.
type InitializeResult struct {
	ProtocolVersion   int             `json:"protocolVersion"`
	AgentInfo         *AgentInfo      `json:"agentInfo"`
	AuthMethods       []AuthMethod    `json:"-"`
	RawAuthMethods    json.RawMessage `json:"authMethods"`
	AgentCapabilities Capabilities    `json:"agentCapabilities"`
	RawCapabilities   json.RawMessage `json:"-"`
}

// TerminalAuthSpec is _meta["terminal-auth"].
type TerminalAuthSpec struct {
	Label   string            `json:"label"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// AuthMethod is one entry of authMethods.
type AuthMethod struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Type        string            `json:"type"` // raw; "" reads as "agent"
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	Meta        struct {
		TerminalAuth *TerminalAuthSpec `json:"terminal-auth"`
	} `json:"_meta"`
}

// Kind is the auth kind token: env_var, terminal, else agent.
func (m AuthMethod) Kind() string {
	switch m.Type {
	case "env_var", "terminal":
		return m.Type
	}
	return "agent"
}

// IsTerminal reports the typed terminal variant or a _meta["terminal-auth"] spec.
func (m AuthMethod) IsTerminal() bool { return m.Type == "terminal" || m.Meta.TerminalAuth != nil }

// parseAuthMethods keeps every entry that parses.
func parseAuthMethods(raw json.RawMessage) []AuthMethod {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []AuthMethod
	for _, it := range items {
		var m AuthMethod
		if json.Unmarshal(it, &m) == nil && m.ID != "" {
			out = append(out, m)
		}
	}
	return out
}

// NameValue is the {name, value} pair of headers and env.
type NameValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// MCPServer is one mcpServers entry (§4).
type MCPServer struct {
	Type    string      `json:"type,omitempty"` // "http" for the HTTP entry; absent for stdio
	Name    string      `json:"name"`
	URL     string      `json:"url,omitempty"`
	Headers []NameValue `json:"headers,omitempty"`
	Command string      `json:"command,omitempty"`
	Args    []string    `json:"args,omitempty"`
	Env     []NameValue `json:"env,omitempty"`
}

// MarshalJSON writes the stdio entry with args and env always present, as
// the schema requires them.
func (s MCPServer) MarshalJSON() ([]byte, error) {
	if s.Type == "http" {
		type http struct {
			Type    string      `json:"type"`
			Name    string      `json:"name"`
			URL     string      `json:"url"`
			Headers []NameValue `json:"headers"`
		}
		h := s.Headers
		if h == nil {
			h = []NameValue{}
		}
		return json.Marshal(http{s.Type, s.Name, s.URL, h})
	}
	type stdio struct {
		Name    string      `json:"name"`
		Command string      `json:"command"`
		Args    []string    `json:"args"`
		Env     []NameValue `json:"env"`
	}
	a, e := s.Args, s.Env
	if a == nil {
		a = []string{}
	}
	if e == nil {
		e = []NameValue{}
	}
	return json.Marshal(stdio{s.Name, s.Command, a, e})
}

// SessionMode is one mode.
type SessionMode struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Modes is the session's mode state.
type Modes struct {
	CurrentModeID  string        `json:"currentModeId"`
	AvailableModes []SessionMode `json:"availableModes"`
}

// ConfigOption is one session config option.
type ConfigOption struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	Category     string          `json:"category"`
	Type         string          `json:"type"` // select | boolean
	CurrentValue json.RawMessage `json:"currentValue"`
	Options      json.RawMessage `json:"options"` // flat [{value,name}] or groups [{group,name,options}]
}

// SelectValue is one choice of a select option.
type SelectValue struct {
	Value       string `json:"value"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Group       string `json:"-"`
}

// Choices flattens a select's options (grouped or not).
func (o ConfigOption) Choices() []SelectValue {
	var items []struct {
		SelectValue
		Group   string        `json:"group"`
		Options []SelectValue `json:"options"`
	}
	if json.Unmarshal(o.Options, &items) != nil {
		return nil
	}
	var out []SelectValue
	for _, it := range items {
		if len(it.Options) > 0 {
			for _, v := range it.Options {
				v.Group = it.Name
				out = append(out, v)
			}
			continue
		}
		if it.Value != "" {
			out = append(out, it.SelectValue)
		}
	}
	return out
}

// Current is a select's current value id ("" when not a string).
func (o ConfigOption) Current() string {
	var s string
	_ = json.Unmarshal(o.CurrentValue, &s)
	return s
}

// ParseConfigOptions keeps every option that parses.
func ParseConfigOptions(raw json.RawMessage) []ConfigOption {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return nil
	}
	var out []ConfigOption
	for _, it := range items {
		var o ConfigOption
		if json.Unmarshal(it, &o) == nil && o.ID != "" {
			out = append(out, o)
		}
	}
	return out
}

// ModesFromConfig synthesises the mode state from a category "mode" select
// (the OpenCode shape, §3.3).
func ModesFromConfig(opts []ConfigOption) *Modes {
	for _, o := range opts {
		if o.Category != "mode" || o.Type != "select" {
			continue
		}
		m := &Modes{CurrentModeID: o.Current()}
		for _, c := range o.Choices() {
			m.AvailableModes = append(m.AvailableModes, SessionMode{ID: c.Value, Name: c.Name, Description: c.Description})
		}
		return m
	}
	return nil
}

// NewSessionResult is session/new's (and load/resume's) answer.
type NewSessionResult struct {
	SessionID        string          `json:"sessionId"`
	Modes            *Modes          `json:"modes"`
	RawConfigOptions json.RawMessage `json:"configOptions"`
}

// ContentBlock is a prompt or message content block.
type ContentBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
	URI      string `json:"uri,omitempty"`
	Name     string `json:"name,omitempty"`
	Resource *struct {
		URI  string `json:"uri"`
		Text string `json:"text"`
	} `json:"resource,omitempty"`
}

// TextOf is the readable text of a block.
func (b ContentBlock) TextOf() string {
	switch b.Type {
	case "text":
		return b.Text
	case "resource":
		if b.Resource != nil {
			return b.Resource.Text
		}
	case "resource_link":
		return b.URI
	}
	return ""
}

// PromptUsage is the prompt response's usage (per turn for claude-agent-acp,
// evidence-acp.md §6; absent for codex-acp).
type PromptUsage struct {
	InputTokens       int64  `json:"inputTokens"`
	OutputTokens      int64  `json:"outputTokens"`
	CachedReadTokens  *int64 `json:"cachedReadTokens"`
	CachedWriteTokens *int64 `json:"cachedWriteTokens"`
	ThoughtTokens     *int64 `json:"thoughtTokens"`
	TotalTokens       int64  `json:"totalTokens"`
}

// PromptResult is session/prompt's answer. StopReason is the schema token.
type PromptResult struct {
	StopReason string       `json:"stopReason"`
	Usage      *PromptUsage `json:"usage"`
}

// Stop reasons (schema tokens, never hand-formatted).
const (
	StopEndTurn         = "end_turn"
	StopMaxTokens       = "max_tokens"
	StopMaxTurnRequests = "max_turn_requests"
	StopRefusal         = "refusal"
	StopCancelled       = "cancelled"
)

// Location is a tool call location.
type Location struct {
	Path string `json:"path"`
	Line *int   `json:"line,omitempty"`
}

// ToolContent is one tool call content item.
type ToolContent struct {
	Type       string        `json:"type"` // content | diff | terminal
	Content    *ContentBlock `json:"content,omitempty"`
	Path       string        `json:"path,omitempty"`
	OldText    *string       `json:"oldText,omitempty"`
	NewText    string        `json:"newText,omitempty"`
	TerminalID string        `json:"terminalId,omitempty"`
}

// Update is one session/update, every variant in one tolerant struct.
type Update struct {
	Kind      string          `json:"sessionUpdate"`
	Content   json.RawMessage `json:"content"` // a ContentBlock for chunks, a []ToolContent for tool calls
	MessageID string          `json:"messageId"`

	ToolCallID string          `json:"toolCallId"`
	Title      *string         `json:"title"`
	ToolKind   string          `json:"kind"`
	Status     string          `json:"status"`
	Locations  []Location      `json:"locations"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
	Meta       json.RawMessage `json:"_meta"`

	Entries           json.RawMessage `json:"entries"`
	AvailableCommands json.RawMessage `json:"availableCommands"`
	CurrentModeID     string          `json:"currentModeId"`
	ConfigOptions     json.RawMessage `json:"configOptions"`
	Size              *int64          `json:"size"`
	Used              *int64          `json:"used"`
	Cost              *struct {
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
	} `json:"cost"`
}

// Block is a chunk's content block.
func (u Update) Block() ContentBlock {
	var b ContentBlock
	_ = json.Unmarshal(u.Content, &b)
	return b
}

// ToolContents is a tool call's content list.
func (u Update) ToolContents() []ToolContent {
	var out []ToolContent
	_ = json.Unmarshal(u.Content, &out)
	return out
}

// SessionNotification is session/update's params.
type SessionNotification struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

// PlanEntry is one plan item.
type PlanEntry struct {
	Content  string `json:"content"`
	Priority string `json:"priority"`
	Status   string `json:"status"`
}

// SlashCommand is one available slash command.
type SlashCommand struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Input       *struct {
		Hint string `json:"hint"`
	} `json:"input"`
}

// PermissionOption is one choice offered with a permission request.
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"` // allow_once | allow_always | reject_once | reject_always
}

// PermissionRequest is session/request_permission's params.
type PermissionRequest struct {
	SessionID string             `json:"sessionId"`
	ToolCall  json.RawMessage    `json:"toolCall"`
	Options   []PermissionOption `json:"options"`
}

// ListedSession is one session/list row.
type ListedSession struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updatedAt"`
}
