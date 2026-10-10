package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

// Hooks is what the agent's hooks told the listener: live sessions, their
// cwd, and the prompts typed (the exact text, preferred over the request's).
type Hooks struct {
	mu       sync.Mutex
	sessions map[string]*hookSession
}

type hookSession struct {
	cwd     string
	prompts []string
	seen    time.Time
	started time.Time
	pids    []int // the hook process's ancestors, nearest first
}

// NewHooks returns an empty registry.
func NewHooks() *Hooks { return &Hooks{sessions: map[string]*hookSession{}} }

func (h *Hooks) get(id string) *hookSession {
	s := h.sessions[id]
	if s == nil {
		s = &hookSession{}
		h.sessions[id] = s
	}
	s.seen = time.Now()
	return s
}

// SessionStart registers a session, its cwd and the hook's ancestors.
func (h *Hooks) SessionStart(id, cwd string, pids []int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.get(id)
	if cwd != "" {
		s.cwd = cwd
	}
	if s.started.IsZero() {
		s.started = time.Now()
	}
	if len(pids) > 0 {
		s.pids = pids
	}
}

// Seen notes the ancestors of any hook of a session (a session may be
// joined before its SessionStart arrives).
func (h *Hooks) Seen(id, cwd string, pids []int) {
	h.SessionStart(id, cwd, pids)
}

// Match finds the hook session a bridge belongs to: the nearest common
// ancestor of the bridge (its first two ancestors) and a session's hook
// (first three) must be the agent process, not one of exclude (the listener,
// its ancestors, the shell it started). Failing that, the only session in the
// same cwd that started within two minutes of at.
func (h *Hooks) Match(bridge []int, cwd string, at time.Time, exclude func(pid int) bool) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	head := func(p []int, n int) []int {
		if len(p) > n {
			return p[:n]
		}
		return p
	}
	var byPID, byCwd []string
	for id, s := range h.sessions {
		if c, ok := procsNearest(head(bridge, 2), head(s.pids, 3)); ok && !exclude(c) {
			byPID = append(byPID, id)
		}
		if cwd != "" && s.cwd == cwd && !s.started.IsZero() && absDur(s.started.Sub(at)) <= 2*time.Minute {
			byCwd = append(byCwd, id)
		}
	}
	switch {
	case len(byPID) == 1:
		return byPID[0], true
	case len(byPID) == 0 && len(byCwd) == 1:
		return byCwd[0], true
	}
	return "", false
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func procsNearest(a, b []int) (int, bool) {
	in := map[int]bool{}
	for _, p := range b {
		in[p] = true
	}
	for _, p := range a {
		if in[p] {
			return p, true
		}
	}
	return 0, false
}

// Prompt queues the text the user typed.
func (h *Hooks) Prompt(id, cwd, text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.get(id)
	if cwd != "" {
		s.cwd = cwd
	}
	s.prompts = append(s.prompts, text)
}

// End forgets a session.
func (h *Hooks) End(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.sessions, id)
}

func (h *Hooks) cwd(id string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if s := h.sessions[id]; s != nil {
		return s.cwd
	}
	return ""
}

// takePrompt pops the oldest typed prompt.
func (h *Hooks) takePrompt(id string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.sessions[id]
	if s == nil || len(s.prompts) == 0 {
		return "", false
	}
	p := s.prompts[0]
	s.prompts = s.prompts[1:]
	return p, true
}

// only returns the single live session when exactly one is registered.
func (h *Hooks) only() (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.sessions) != 1 {
		return "", false
	}
	for id := range h.sessions {
		return id, true
	}
	return "", false
}

// Anthropic records Claude Code's Messages traffic.
type Anthropic struct {
	Rec   *record.Recorder
	Hooks *Hooks
	Agent string // claude-code
	Root  string // the Project, the default cwd

	mu      sync.Mutex
	threads map[string]*thread // per native session id
	pending map[uint64]pending // accepted requests awaiting their response
}

// pending is what the request half decided, for the response half.
type pending struct {
	native string
	sess   record.Session
}

// thread is the main conversation of a session.
type thread struct {
	mu       sync.Mutex
	first    string // fingerprint of the main conversation's first user message
	messages int    // messages in the last main request seen
	open     bool   // a turn is open
}

// NewAnthropic wires a recorder and the hook registry.
func NewAnthropic(rec *record.Recorder, hooks *Hooks, root string) *Anthropic {
	return &Anthropic{Rec: rec, Hooks: hooks, Agent: "claude-code", Root: root, threads: map[string]*thread{}, pending: map[uint64]pending{}}
}

var sessionInUserID = regexp.MustCompile(`session_([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// identify picks the session: Claude Code's own id from metadata.user_id,
// else the only hook-registered session, else a hash of the conversation's
// start (stable for the conversation).
func (a *Anthropic) identify(req *Request) string {
	if m := sessionInUserID.FindStringSubmatch(req.Metadata.UserID); m != nil {
		return strings.ToLower(m[1])
	}
	if id, ok := a.Hooks.only(); ok {
		return id
	}
	h := sha256.New()
	_, _ = h.Write(req.System)
	if len(req.Messages) > 0 {
		_, _ = h.Write([]byte(PromptText(req.Messages[0])))
	}
	return "proxy-" + hex.EncodeToString(h.Sum(nil))[:16]
}

func fingerprint(m Message) string {
	s := sha256.Sum256([]byte(PromptText(m)))
	return hex.EncodeToString(s[:8])
}

// Observe records one exchange. Only POST /v1/messages of the main
// conversation is recorded: requests without tools (Claude Code's helper
// calls) and sidechains (sub-agents, whose conversation starts elsewhere) are
// left out, as the desktop importer leaves out sidechain lines.
// Observe handles a whole exchange at once (tests, and callers without the
// two-phase queue).
func (a *Anthropic) Observe(ex Exchange) {
	a.Request(ex)
	a.Response(ex)
}

// Request handles the request half when it arrives: tool results (sampling
// the files those tools wrote, before the agent can act on the reply) and a
// new prompt.
func (a *Anthropic) Request(ex Exchange) {
	if !strings.HasSuffix(strings.TrimRight(ex.Path, "/"), "/v1/messages") || ex.Truncated {
		return
	}
	req, ok := ParseRequest(ex.Request)
	if !ok || len(req.Tools) == 0 {
		return
	}
	native := a.identify(req)
	a.mu.Lock()
	th := a.threads[native]
	if th == nil {
		th = &thread{}
		a.threads[native] = th
	}
	a.mu.Unlock()
	th.mu.Lock()
	defer th.mu.Unlock()

	fp := fingerprint(req.Messages[0])
	// The messages added since the last request of this conversation. Claude
	// Code may put a system-role message after the user's prompt, so the
	// prompt is the newest user message with text and no tool results, not
	// simply the last message.
	start := th.messages
	if fp != th.first || start > len(req.Messages) {
		start = 0
	}
	added := req.Messages[start:]
	promptIdx := -1
	var results []Block
	for i, m := range added {
		if m.Role != "user" {
			continue
		}
		if HasToolResult(m) {
			for _, b := range m.Blocks() {
				if b.Type == "tool_result" {
					results = append(results, b)
				}
			}
		} else if strings.TrimSpace(PromptText(m)) != "" {
			promptIdx = i
		}
	}
	newPrompt := promptIdx >= 0
	switch {
	case th.first == "":
		th.first = fp
	case fp != th.first:
		// Another conversation in the same session: a compaction restarts
		// the main one with a fresh prompt; anything else is a sidechain.
		if !newPrompt || len(results) > 0 || len(req.Messages) > 3 {
			return
		}
		th.first, th.messages = fp, 0
	}
	if len(req.Messages) < th.messages {
		return // an older or retried request
	}

	ctx := context.Background()
	cwd := a.Hooks.cwd(native)
	if cwd == "" {
		cwd = a.Root
	}
	sess := record.Session{Agent: a.Agent, NativeID: native, Cwd: cwd, Model: req.Model}

	if len(req.Messages) > th.messages {
		for _, b := range results {
			status := "completed"
			if b.IsError {
				status = "failed"
			}
			text := ToolResultText(b.Content)
			_ = a.Rec.ToolCall(ctx, sess, record.Tool{NativeID: b.ToolUseID, Status: status, Result: &text})
		}
		if newPrompt && len(results) == 0 {
			if th.open {
				_ = a.Rec.FinishTurn(ctx, sess)
			}
			prompt, ok := a.Hooks.takePrompt(native)
			if !ok {
				prompt = PromptText(added[promptIdx])
			}
			if _, err := a.Rec.Prompt(ctx, sess, prompt); err == nil {
				th.open = true
			}
		}
	}
	th.messages = len(req.Messages)
	a.mu.Lock()
	a.pending[ex.ID] = pending{native: native, sess: sess}
	a.mu.Unlock()
}

// Response handles the response half of an accepted request: the runs, the
// tool calls, usage, and the end of the turn.
func (a *Anthropic) Response(ex Exchange) {
	traceExchange(ex)
	a.mu.Lock()
	p, ok := a.pending[ex.ID]
	delete(a.pending, ex.ID)
	th := a.threads[p.native]
	a.mu.Unlock()
	if !ok || th == nil || ex.Status < 200 || ex.Status > 299 || ex.Truncated {
		return
	}
	resp, ok := ParseResponse(ex.ContentType, ex.Response)
	if !ok {
		return
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	ctx := context.Background()
	sess := p.sess
	if resp.Model != "" {
		sess.Model = resp.Model
	}
	a.recordResponse(ctx, sess, resp)
	_ = a.Rec.Usage(ctx, sess, sess.Model, store.TokenTotals{Input: resp.Usage.Input, Output: resp.Usage.Output,
		CacheCreation: resp.Usage.CacheCreation, CacheRead: resp.Usage.CacheRead})
	if resp.StopReason != "" && resp.StopReason != "tool_use" {
		_ = a.Rec.FinishTurn(ctx, sess)
		th.open = false
	}
}

// recordResponse writes each contiguous text or thinking run as a message
// and each tool_use as a running tool call.
func (a *Anthropic) recordResponse(ctx context.Context, sess record.Session, resp *Response) {
	now := time.Now()
	run, mode, n := strings.Builder{}, "", 0
	flush := func() {
		if run.Len() > 0 {
			id := resp.ID
			if n > 0 {
				id = resp.ID + "-" + itoa(n)
			}
			_ = a.Rec.Message(ctx, sess, id, mode, run.String(), now)
			n++
		}
		run.Reset()
		mode = ""
	}
	for _, b := range resp.Blocks {
		switch b.Type {
		case "text", "thinking":
			m, text := "text", b.Text
			if b.Type == "thinking" {
				m, text = "thinking", b.Thinking
			}
			if mode != "" && mode != m {
				flush()
			}
			mode = m
			run.WriteString(text)
		case "tool_use":
			flush()
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			_ = a.Rec.ToolCall(ctx, sess, record.Tool{NativeID: b.ID, Name: b.Name, Title: b.Name, Status: "running",
				Arguments: input, Locations: locations(input)})
		}
	}
	flush()
}

// locations are the paths a tool input names.
func locations(input json.RawMessage) []string {
	var a map[string]json.RawMessage
	_ = json.Unmarshal(input, &a)
	for _, k := range []string{"file_path", "notebook_path", "path"} {
		var s string
		if json.Unmarshal(a[k], &s) == nil && s != "" {
			return []string{s}
		}
	}
	return nil
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// FinishSession closes a session's open turn (a Stop or SessionEnd hook).
func (a *Anthropic) FinishSession(native string) {
	a.mu.Lock()
	th := a.threads[native]
	a.mu.Unlock()
	if th == nil {
		return
	}
	th.mu.Lock()
	defer th.mu.Unlock()
	if th.open {
		_ = a.Rec.FinishTurn(context.Background(), record.Session{Agent: a.Agent, NativeID: native})
		th.open = false
	}
}
