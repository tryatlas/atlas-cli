package run

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tryatlas/atlas-cli/internal/agents/acp"
	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	memrecord "github.com/tryatlas/atlas-cli/internal/memory/record"
	"github.com/tryatlas/atlas-cli/internal/memory/server"
	"github.com/tryatlas/atlas-cli/internal/project"
)

// MemoryServerName is the name agents see (SPEC §5.5).
const MemoryServerName = "atlas_memory"

// View renders a session and answers the agent's questions: the chat TUI,
// or the headless runner. Update must not block.
type View interface {
	Update(sessionID string, u acp.Update)
	Permission(ctx context.Context, req acp.PermissionRequest) (string, bool)
	Elicit(ctx context.Context, e acp.Elicitation) acp.ElicitationAnswer
	ElicitationComplete(id string)
}

// Session is one ACP session with capture and memory attached. It is the
// agent's acp.Host.
type Session struct {
	AgentID string
	Cwd     string
	Agent   *acp.Agent
	ID      string // the ACP session id
	Modes   *acp.Modes
	Config  []acp.ConfigOption

	host  *Host
	view  View
	token string
	grant server.Grant

	mu       sync.Mutex
	active   bool // a prompt of this process is in flight
	runs     []*msgRun
	cur      *msgRun
	runN     int
	msgIDs   map[string]int
	calls    map[string]record.Tool // tool call id → what every sighting so far said
	turnSeen bool
}

type msgRun struct {
	mode string // text | thinking
	id   string
	body strings.Builder
	at   time.Time
}

// NewSession wires a session to a host and a view. Connect the agent with
// the session as its acp.Host, then call Open.
func NewSession(agentID, cwd string, h *Host, v View) *Session {
	return &Session{AgentID: agentID, Cwd: cwd, host: h, view: v, msgIDs: map[string]int{}, calls: map[string]record.Tool{}}
}

// mcpServers is the atlas_memory entry when sharing is on: HTTP with the
// bearer when the agent advertises HTTP MCP, else the stdio bridge with
// ATLAS_MCP_TOKEN in its env (SPEC §8.4; the token is never in argv).
func (s *Session) mcpServers() []acp.MCPServer {
	if s.host == nil || s.host.Listener == nil || !project.MemorySharing(s.memoryGateDir()) {
		return nil
	}
	if s.token == "" {
		s.token = s.host.Listener.Tokens.Mint(server.Grant{Agent: s.AgentID, Cwd: s.Cwd})
	}
	url := s.host.Listener.MCPURL()
	if s.Agent.Init.AgentCapabilities.MCPCapabilities.HTTP {
		return []acp.MCPServer{{Type: "http", Name: MemoryServerName, URL: url,
			Headers: []acp.NameValue{{Name: "Authorization", Value: "Bearer " + s.token}}}}
	}
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	return []acp.MCPServer{{Name: MemoryServerName, Command: exe, Args: []string{"mcp-bridge", url},
		Env: []acp.NameValue{{Name: "ATLAS_MCP_TOKEN", Value: s.token}}}}
}

func (s *Session) memoryGateDir() string {
	if p, err := project.Discover(context.Background(), s.Cwd); err == nil {
		return p.Root
	}
	return s.Cwd
}

// bind attaches the token and the memory session to the ACP session id.
func (s *Session) bind(ctx context.Context, id string) {
	s.ID = id
	s.grant = server.Grant{SessionID: id, Agent: s.AgentID, Cwd: s.Cwd}
	if s.token != "" {
		s.host.Listener.Tokens.Bind(s.token, s.grant)
	}
	if s.host != nil && s.host.Listener != nil {
		s.host.Listener.Memory.SessionStarted(ctx, s.grant)
	}
}

// Open creates a session (session/new), running the agent's sign-in when it
// answers AuthRequired. login runs a terminal auth method.
func (s *Session) Open(ctx context.Context, login func(context.Context, acp.AuthMethod) error) error {
	p := acp.SessionParams{Cwd: s.Cwd, MCPServers: s.mcpServers()}
	r, err := s.Agent.NewSession(ctx, p)
	if acp.IsAuthRequired(err) {
		if err := s.signIn(ctx, login); err != nil {
			s.revoke()
			return err
		}
		r, err = s.Agent.NewSession(ctx, p)
	}
	if err != nil {
		s.revoke()
		return err
	}
	s.adopt(ctx, r)
	return nil
}

// Reopen loads (history replayed) or resumes an existing ACP session by
// capability.
func (s *Session) Reopen(ctx context.Context, id string) error {
	p := acp.SessionParams{Cwd: s.Cwd, MCPServers: s.mcpServers(), SessionID: id}
	var r *acp.NewSessionResult
	var err error
	switch {
	case s.Agent.Init.AgentCapabilities.LoadSession:
		r, err = s.Agent.LoadSession(ctx, p)
	case s.Agent.Init.AgentCapabilities.CanResume():
		r, err = s.Agent.ResumeSession(ctx, p)
	default:
		err = fmt.Errorf("%s cannot load or resume sessions", s.AgentID)
	}
	if err != nil {
		s.revoke()
		return err
	}
	s.adopt(ctx, r)
	return nil
}

func (s *Session) adopt(ctx context.Context, r *acp.NewSessionResult) {
	s.bind(ctx, r.SessionID)
	s.Modes = r.Modes
	s.Config = acp.ParseConfigOptions(r.RawConfigOptions)
}

// ApplyDefaultMode sends set_mode when the installed entry names a mode the
// agent offers; a failure is rolled back locally.
func (s *Session) ApplyDefaultMode(ctx context.Context, mode string) {
	if mode == "" || s.Modes == nil || s.Modes.CurrentModeID == mode {
		return
	}
	for _, m := range s.Modes.AvailableModes {
		if m.ID == mode {
			prev := s.Modes.CurrentModeID
			s.Modes.CurrentModeID = mode
			if s.Agent.SetMode(ctx, s.ID, mode) != nil {
				s.Modes.CurrentModeID = prev
			}
			return
		}
	}
}

// AuthError is a sign-in the CLI cannot do: no terminal method is offered.
type AuthError struct{ Methods []acp.AuthMethod }

func (e *AuthError) Error() string {
	if len(e.Methods) == 0 {
		return "the agent needs you to sign in, but offers no sign-in method"
	}
	var b strings.Builder
	b.WriteString("the agent needs you to sign in; it offers:")
	for _, m := range e.Methods {
		fmt.Fprintf(&b, "\n  - %s (%s)", m.Name, m.Kind())
		if m.Description != "" {
			b.WriteString(": " + m.Description)
		}
	}
	b.WriteString("\nAtlas runs only terminal sign-in methods and never guesses a login command.")
	return b.String()
}

func (s *Session) signIn(ctx context.Context, login func(context.Context, acp.AuthMethod) error) error {
	for _, m := range s.Agent.Init.AuthMethods {
		if !m.IsTerminal() || login == nil {
			continue
		}
		if err := login(ctx, m); err != nil {
			return err
		}
		return s.Agent.Authenticate(ctx, m)
	}
	return &AuthError{Methods: s.Agent.Init.AuthMethods}
}

func (s *Session) revoke() {
	if s.token != "" && s.host != nil && s.host.Listener != nil {
		s.host.Listener.Tokens.Revoke(s.token)
	}
}

// Close ends the memory session and revokes the token.
func (s *Session) Close(ctx context.Context) {
	s.flush(ctx, false)
	if s.ID != "" && s.host != nil && s.host.Listener != nil {
		s.host.Listener.Memory.SessionEnded(ctx, s.grant)
	}
	s.revoke()
}

// recSession names the session for the recorder.
func (s *Session) recSession() record.Session {
	model := ""
	for _, o := range s.Config {
		if o.Category == "model" && o.Type == "select" {
			model = o.Current()
		}
	}
	return record.Session{Agent: s.AgentID, NativeID: s.ID, Cwd: s.Cwd, Model: model}
}

// Prompt records the prompt, sends it (text first, then images, then
// resource links; nothing prepended), and records the turn's end.
func (s *Session) Prompt(ctx context.Context, text string, images []acp.ContentBlock, links []acp.ContentBlock) (*acp.PromptResult, error) {
	if rec := s.recorder(); rec != nil {
		if _, err := rec.Prompt(ctx, s.recSession(), text); err != nil {
			return nil, fmt.Errorf("recording the prompt: %w", err)
		}
	}
	blocks := []acp.ContentBlock{{Type: "text", Text: text}}
	if s.Agent.Init.AgentCapabilities.PromptCapabilities.Image {
		blocks = append(blocks, images...)
	}
	blocks = append(blocks, links...)
	s.mu.Lock()
	s.active, s.turnSeen = true, true
	s.mu.Unlock()
	r, err := s.Agent.Prompt(ctx, s.ID, blocks)
	if r != nil && r.Usage != nil {
		if rec := s.recorder(); rec != nil {
			u := r.Usage
			_ = rec.Usage(ctx, s.recSession(), "", store.TokenTotals{Input: u.InputTokens, Output: u.OutputTokens,
				CacheRead: deref(u.CachedReadTokens), CacheCreation: deref(u.CachedWriteTokens), Reasoning: deref(u.ThoughtTokens)})
		}
	}
	// A disconnect leaves the turn open (the desktop marks it aborted).
	s.flush(ctx, err == nil)
	return r, err
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Session) recorder() *record.Recorder {
	if s.host == nil {
		return nil
	}
	return s.host.Recorder
}

// flush writes the turn's message runs (created_at = their first chunk)
// and, when finished, completes the turn.
func (s *Session) flush(ctx context.Context, finished bool) {
	s.mu.Lock()
	runs := s.runs
	s.runs, s.cur = nil, nil
	active := s.active
	s.active = false
	s.mu.Unlock()
	rec := s.recorder()
	if rec == nil || !active {
		return
	}
	for _, r := range runs {
		_ = rec.Message(ctx, s.recSession(), r.id, r.mode, r.body.String(), r.at)
	}
	if finished {
		_ = rec.FinishTurn(ctx, s.recSession())
	}
}

// ── acp.Host ─────────────────────────────────────────────────────────────────

// Update is called on the agent's serial queue.
func (s *Session) Update(sessionID string, u acp.Update) {
	if sessionID != s.ID && s.ID != "" {
		return
	}
	s.capture(u)
	s.view.Update(sessionID, u)
}

// Permission forwards to the view.
func (s *Session) Permission(ctx context.Context, req acp.PermissionRequest) (string, bool) {
	return s.view.Permission(ctx, req)
}

// Elicit forwards to the view.
func (s *Session) Elicit(ctx context.Context, e acp.Elicitation) acp.ElicitationAnswer {
	return s.view.Elicit(ctx, e)
}

// ElicitationComplete forwards to the view.
func (s *Session) ElicitationComplete(id string) { s.view.ElicitationComplete(id) }

// capture maps one update to the record (SPEC §8.4 "Map to capture").
func (s *Session) capture(u acp.Update) {
	ctx := context.Background()
	switch u.Kind {
	case "agent_message_chunk", "agent_thought_chunk":
		mode := "text"
		if u.Kind == "agent_thought_chunk" {
			mode = "thinking"
		}
		s.chunk(mode, u.MessageID, u.Block().TextOf())
	case "tool_call", "tool_call_update":
		s.mu.Lock()
		s.cur = nil // a tool call ends a message run
		active := s.active
		t := merge(s.calls[u.ToolCallID], toolOf(u))
		if t.Status == "" {
			t.Status = "pending"
		}
		s.calls[u.ToolCallID] = t
		s.mu.Unlock()
		if !active {
			return
		}
		if rec := s.recorder(); rec != nil {
			_ = rec.ToolCall(ctx, s.recSession(), t)
		}
		if t.Status == "completed" {
			s.foldFileChanged(ctx, t)
		}
	case "plan":
		s.foldPlan(ctx, u.Entries)
	case "usage_update":
		s.mu.Lock()
		active := s.active
		s.mu.Unlock()
		if rec := s.recorder(); rec != nil && active {
			_ = rec.Gauge(ctx, s.recSession(), u.Used, u.Size)
		}
	case "config_option_update":
		s.Config = acp.ParseConfigOptions(u.ConfigOptions)
		if m := acp.ModesFromConfig(s.Config); m != nil && s.Modes != nil {
			s.Modes.CurrentModeID = m.CurrentModeID
		}
	case "current_mode_update":
		if s.Modes != nil {
			s.Modes.CurrentModeID = u.CurrentModeID
		}
	}
}

// chunk appends to the current run; a change of kind or message id starts
// a new one (one message per contiguous run of one kind).
func (s *Session) chunk(mode, messageID, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.active || text == "" {
		return
	}
	if s.cur == nil || s.cur.mode != mode || (messageID != "" && s.cur.id != "" && !strings.HasPrefix(s.cur.id, messageID)) {
		s.runN++
		id := messageID
		if id == "" {
			id = fmt.Sprintf("%s-%d", s.ID, s.runN)
		}
		if n := s.msgIDs[id]; n > 0 {
			s.msgIDs[id] = n + 1
			id = fmt.Sprintf("%s#%d", id, n+1)
		} else {
			s.msgIDs[id] = 1
		}
		s.cur = &msgRun{mode: mode, id: id, at: time.Now()}
		s.runs = append(s.runs, s.cur)
	}
	s.cur.body.WriteString(text)
}

// merge fills a sighting's missing fields from earlier ones: an update may
// carry only what changed.
func merge(prev, t record.Tool) record.Tool {
	if t.Title == "" {
		t.Title = prev.Title
	}
	if t.Kind == "" {
		t.Kind = prev.Kind
	}
	if t.Status == "" {
		t.Status = prev.Status
	}
	if len(t.Arguments) == 0 || string(t.Arguments) == "null" {
		t.Arguments = prev.Arguments
	}
	if len(t.Locations) == 0 {
		t.Locations = prev.Locations
	}
	if len(t.Diffs) == 0 {
		t.Diffs = prev.Diffs
	}
	if t.Result == nil {
		t.Result = prev.Result
	}
	return t
}

// toolOf turns a tool call update into a recorder sighting.
func toolOf(u acp.Update) record.Tool {
	title := ""
	if u.Title != nil {
		title = *u.Title
	}
	t := record.Tool{NativeID: u.ToolCallID, Title: title, Kind: u.ToolKind, Status: status(u.Status), Arguments: u.RawInput}
	for _, l := range u.Locations {
		if l.Path != "" {
			t.Locations = append(t.Locations, l.Path)
		}
	}
	var texts []string
	for _, c := range u.ToolContents() {
		switch c.Type {
		case "diff":
			t.Diffs = append(t.Diffs, record.Diff{Path: c.Path, OldText: c.OldText, NewText: c.NewText})
		case "content":
			if c.Content != nil {
				if s := c.Content.TextOf(); s != "" {
					texts = append(texts, s)
				}
			}
		}
	}
	switch {
	case len(texts) > 0:
		r := strings.Join(texts, "\n")
		t.Result = &r
	case len(u.RawOutput) > 0 && string(u.RawOutput) != "null":
		var s string
		r := string(u.RawOutput)
		if json.Unmarshal(u.RawOutput, &s) == nil {
			r = s
		}
		t.Result = &r
	}
	return t
}

// status maps ACP's tool status onto capture's ("" when the update does
// not say).
func status(s string) string {
	switch s {
	case "in_progress":
		return "running"
	case "pending", "completed", "failed":
		return s
	}
	return ""
}

// foldPlan writes the plan as working memory: "- [status] item" lines,
// at most 600 characters.
func (s *Session) foldPlan(ctx context.Context, raw json.RawMessage) {
	if s.host == nil || s.host.Listener == nil || s.ID == "" {
		return
	}
	var entries []acp.PlanEntry
	if json.Unmarshal(raw, &entries) != nil || len(entries) == 0 {
		return
	}
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "- [%s] %s", e.Status, e.Content)
	}
	text := []rune(b.String())
	if len(text) > 600 {
		text = text[:600]
	}
	_ = s.host.Listener.Memory.Fold(ctx, s.grant, memrecord.Event{Kind: "plan_set", Text: string(text), Status: "active"})
}

// foldFileChanged records one file_changed per path a completed write
// tool touched, summarised by the tool's title.
func (s *Session) foldFileChanged(ctx context.Context, t record.Tool) {
	if s.host == nil || s.host.Listener == nil || s.ID == "" {
		return
	}
	name := record.CanonicalToolName(t.Name, t.Title, t.Kind, t.Arguments)
	if !record.WritesFiles(name) && len(t.Diffs) == 0 {
		return
	}
	root := s.memoryGateDir()
	for _, p := range record.WritePaths(t.Arguments, t.Locations, t.Diffs...) {
		rel, out := record.ProjectPath(root, p)
		if out {
			continue
		}
		summary := t.Title
		if summary == "" {
			summary = name + " " + filepath.Base(rel)
		}
		_ = s.host.Listener.Memory.Fold(ctx, s.grant, memrecord.Event{Kind: memrecord.KindFileChanged, Key: rel, Text: summary})
	}
}
