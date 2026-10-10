package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Deadlines (ADR-0008, evidence-acp.md §2.5).
const (
	InitializeTimeout   = 60 * time.Second
	initializeExitGrace = 250 * time.Millisecond
	stderrDrainGrace    = 1 * time.Second
	RequestTimeout      = 120 * time.Second
	CancelGrace         = 5 * time.Second
	ConnectDeadline     = 20 * time.Minute
)

// Host is what the agent's requests and updates reach: the TUI or the
// headless runner. Update runs on the serial queue and must not block.
type Host interface {
	Update(sessionID string, u Update)
	Permission(ctx context.Context, req PermissionRequest) (optionID string, ok bool)
	Elicit(ctx context.Context, req Elicitation) ElicitationAnswer
	ElicitationComplete(id string)
}

// Agent is a connected ACP agent.
type Agent struct {
	ID      string
	Command Command
	Init    InitializeResult

	proc *Process
	conn *Conn
	host Host

	mu      sync.Mutex
	roots   map[string][]string     // sessionId → granted roots (cwd first)
	cancels map[string]*cancelState // sessionId → the turn's cancel state
	terms   *terminals
}

type cancelState struct {
	ch   chan struct{} // closed on session/cancel
	once sync.Once
}

func (cs *cancelState) fire() { cs.once.Do(func() { close(cs.ch) }) }

func (cs *cancelState) fired() bool {
	select {
	case <-cs.ch:
		return true
	default:
		return false
	}
}

// ErrUnsupported is a protocol version below 1.
var ErrUnsupported = errors.New("the agent speaks an ACP version older than 1")

// Connect spawns the command and runs initialize: raced against the
// child's exit and the 60 s timeout; when initialize fails, the exit
// status (with stderr) is preferred if it lands within 250 ms.
func Connect(ctx context.Context, id string, cmd Command, dir, version string, host Host) (*Agent, error) {
	proc, err := Spawn(cmd, dir)
	if err != nil {
		return nil, fmt.Errorf("could not start %s: %w", id, err)
	}
	a := &Agent{ID: id, Command: cmd, proc: proc, host: host, roots: map[string][]string{}, cancels: map[string]*cancelState{}}
	a.terms = newTerminals(a)
	a.conn = NewConn(proc.Stdout, proc.Stdin, &handler{a: a})

	ictx, cancel := context.WithTimeout(ctx, InitializeTimeout)
	defer cancel()
	var raw json.RawMessage
	errc := make(chan error, 1)
	go func() { errc <- a.conn.Call(ictx, "initialize", InitializeParams(version), &raw) }()
	select {
	case err = <-errc:
	case <-proc.Exited():
		proc.Kill()
		return nil, proc.exitedError()
	}
	if err != nil {
		if proc.waitExit(ctx, initializeExitGrace) {
			proc.Kill()
			return nil, proc.exitedError()
		}
		proc.Kill()
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("%s did not answer initialize within %s", id, InitializeTimeout)
		}
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := json.Unmarshal(raw, &a.Init); err != nil {
		proc.Kill()
		return nil, fmt.Errorf("unreadable initialize answer: %w", err)
	}
	a.Init.AuthMethods = parseAuthMethods(a.Init.RawAuthMethods)
	var caps struct {
		AgentCapabilities json.RawMessage `json:"agentCapabilities"`
	}
	_ = json.Unmarshal(raw, &caps)
	a.Init.RawCapabilities = caps.AgentCapabilities
	if a.Init.ProtocolVersion < ProtocolVersion {
		proc.Kill()
		return nil, ErrUnsupported
	}
	return a, nil
}

// Name is agentInfo.name, else the id; Version is agentInfo.version.
func (a *Agent) Name() string {
	if a.Init.AgentInfo != nil && a.Init.AgentInfo.Name != "" {
		return a.Init.AgentInfo.Name
	}
	return a.ID
}

// Done closes when the agent's output ends.
func (a *Agent) Done() <-chan struct{} { return a.conn.Done() }

// Exited is the process exit with stderr, once it has happened.
func (a *Agent) Exited() *ExitedError { return a.proc.exitedError() }

// Close kills the agent's process group and its terminals.
func (a *Agent) Close() {
	a.terms.releaseAll()
	a.proc.Kill()
}

func (a *Agent) call(ctx context.Context, method string, params, out any) error {
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()
	err := a.conn.Call(ctx, method, params, out)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: no answer within %s", method, RequestTimeout)
	}
	return err
}

// IsAuthRequired reports the typed AuthRequired error.
func IsAuthRequired(err error) bool {
	e, ok := AsRPCError(err)
	return ok && e.Code == CodeAuthRequired
}

// Authenticate sends authenticate {methodId}. For a terminal method,
// MethodNotFound or "not implemented" means the login command already did
// the work; the next session/new is the real check.
func (a *Agent) Authenticate(ctx context.Context, m AuthMethod) error {
	err := a.call(ctx, "authenticate", map[string]any{"methodId": m.ID}, nil)
	if err == nil {
		return nil
	}
	e, ok := AsRPCError(err)
	if !ok {
		return err
	}
	notImpl := e.Code == CodeMethodNotFound ||
		(e.Code == CodeInternalError && strings.Contains(strings.ToLower(e.Details()), "not implemented"))
	switch {
	case e.Code == CodeAuthRequired:
		return err
	case notImpl && m.IsTerminal():
		return nil
	case notImpl:
		return errors.New("the agent does not support signing in this way")
	case e.Code == CodeInternalError && e.Details() != "":
		return errors.New(e.Details())
	}
	return err
}

// Logout is sent only when auth.logout is advertised.
func (a *Agent) Logout(ctx context.Context) error {
	if !a.Init.AgentCapabilities.CanLogout() {
		return errors.New("this agent does not support signing out")
	}
	return a.call(ctx, "logout", map[string]any{}, nil)
}

// SessionParams are what session/new, load and resume send.
type SessionParams struct {
	Cwd        string
	ExtraDirs  []string
	MCPServers []MCPServer
	SessionID  string // load and resume
}

func (a *Agent) sessionPayload(p SessionParams) map[string]any {
	servers := []MCPServer{}
	allowed := []string{}
	for _, s := range p.MCPServers {
		if s.Type == "http" && !a.Init.AgentCapabilities.MCPCapabilities.HTTP {
			continue // only transports the agent advertised
		}
		servers = append(servers, s)
		if s.Type == "http" {
			allowed = append(allowed, "mcp__"+s.Name)
		}
	}
	m := map[string]any{"cwd": p.Cwd, "mcpServers": servers}
	if len(p.ExtraDirs) > 0 && a.Init.AgentCapabilities.ExtraDirs() {
		m["additionalDirectories"] = p.ExtraDirs
	}
	if len(allowed) > 0 {
		m["_meta"] = map[string]any{"claudeCode": map[string]any{"options": map[string]any{"allowedTools": allowed}}}
	}
	if p.SessionID != "" {
		m["sessionId"] = p.SessionID
	}
	return m
}

func (a *Agent) grant(sessionID string, p SessionParams) {
	roots := append([]string{p.Cwd}, p.ExtraDirs...)
	a.mu.Lock()
	a.roots[sessionID] = roots
	a.mu.Unlock()
}

// NewSession sends session/new.
func (a *Agent) NewSession(ctx context.Context, p SessionParams) (*NewSessionResult, error) {
	var r NewSessionResult
	if err := a.call(ctx, "session/new", a.sessionPayload(p), &r); err != nil {
		return nil, err
	}
	if r.SessionID == "" {
		return nil, errors.New("session/new returned no sessionId")
	}
	a.grant(r.SessionID, p)
	fillModes(&r)
	return &r, nil
}

// LoadSession replays history: the roots are registered before the call,
// because the replay arrives as session/update while it is in flight.
func (a *Agent) LoadSession(ctx context.Context, p SessionParams) (*NewSessionResult, error) {
	return a.reopen(ctx, "session/load", p)
}

// ResumeSession continues without history.
func (a *Agent) ResumeSession(ctx context.Context, p SessionParams) (*NewSessionResult, error) {
	return a.reopen(ctx, "session/resume", p)
}

func (a *Agent) reopen(ctx context.Context, method string, p SessionParams) (*NewSessionResult, error) {
	a.grant(p.SessionID, p)
	var r NewSessionResult
	if err := a.call(ctx, method, a.sessionPayload(p), &r); err != nil {
		return nil, err
	}
	r.SessionID = p.SessionID
	fillModes(&r)
	return &r, nil
}

func fillModes(r *NewSessionResult) {
	if r.Modes == nil {
		r.Modes = ModesFromConfig(ParseConfigOptions(r.RawConfigOptions))
	}
}

// SetMode sends session/set_mode.
func (a *Agent) SetMode(ctx context.Context, sessionID, modeID string) error {
	return a.call(ctx, "session/set_mode", map[string]any{"sessionId": sessionID, "modeId": modeID}, nil)
}

// SetConfigOption sends session/set_config_option; the answer's
// configOptions replace the local list.
func (a *Agent) SetConfigOption(ctx context.Context, sessionID, configID string, value any) (json.RawMessage, error) {
	var r struct {
		ConfigOptions json.RawMessage `json:"configOptions"`
	}
	err := a.call(ctx, "session/set_config_option", map[string]any{"sessionId": sessionID, "configId": configID, "value": value}, &r)
	return r.ConfigOptions, err
}

// ListSessions pages session/list.
func (a *Agent) ListSessions(ctx context.Context, cwd, cursor string) ([]ListedSession, string, error) {
	if !a.Init.AgentCapabilities.CanList() {
		return nil, "", errors.New("this agent cannot list its sessions")
	}
	params := map[string]any{}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if cursor != "" {
		params["cursor"] = cursor
	}
	var r struct {
		Sessions   []ListedSession `json:"sessions"`
		NextCursor string          `json:"nextCursor"`
	}
	err := a.call(ctx, "session/list", params, &r)
	return r.Sessions, r.NextCursor, err
}

// DeleteSession is sent only when sessionCapabilities.delete.
func (a *Agent) DeleteSession(ctx context.Context, sessionID string) error {
	if !a.Init.AgentCapabilities.CanDelete() {
		return errors.New("this agent cannot delete sessions")
	}
	return a.call(ctx, "session/delete", map[string]any{"sessionId": sessionID}, nil)
}

// CloseSession is sent only when sessionCapabilities.close.
func (a *Agent) CloseSession(ctx context.Context, sessionID string) error {
	if !a.Init.AgentCapabilities.CanClose() {
		return nil
	}
	return a.call(ctx, "session/close", map[string]any{"sessionId": sessionID}, nil)
}

// abortStrings are what some agents answer to a cancelled turn.
var abortStrings = []string{"This operation was aborted", "The user aborted a request"}

// Prompt sends session/prompt with no timeout. After Cancel, the turn waits
// CancelGrace for the agent's own answer and then resolves locally as
// cancelled (abandoning the request sends $/cancel_request).
func (a *Agent) Prompt(ctx context.Context, sessionID string, blocks []ContentBlock) (*PromptResult, error) {
	cs := &cancelState{ch: make(chan struct{})}
	a.mu.Lock()
	a.cancels[sessionID] = cs
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		if a.cancels[sessionID] == cs {
			delete(a.cancels, sessionID)
		}
		a.mu.Unlock()
	}()
	cctx, abandon := context.WithCancel(ctx)
	defer abandon()
	type res struct {
		r   PromptResult
		err error
	}
	done := make(chan res, 1)
	go func() {
		var r PromptResult
		err := a.conn.Call(cctx, "session/prompt", map[string]any{"sessionId": sessionID, "prompt": blocks}, &r)
		done <- res{r, err}
	}()
	var grace <-chan time.Time
	fired := (<-chan struct{})(cs.ch)
	for {
		select {
		case out := <-done:
			if out.err != nil {
				if e, ok := AsRPCError(out.err); ok && e.Code == CodeInternalError && cs.fired() {
					for _, s := range abortStrings {
						if strings.Contains(e.Details(), s) {
							return &PromptResult{StopReason: StopCancelled}, nil
						}
					}
				}
				if e, ok := AsRPCError(out.err); ok && e.Code == CodeInternalError && e.Details() != "" && !IsAuthRequired(out.err) {
					return nil, errors.New(e.Details())
				}
				if errors.Is(out.err, context.Canceled) && cs.fired() {
					return &PromptResult{StopReason: StopCancelled}, nil
				}
				return nil, out.err
			}
			return &out.r, nil
		case <-fired:
			grace = time.After(CancelGrace)
			fired = nil
		case <-grace:
			abandon()
			return &PromptResult{StopReason: StopCancelled}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Cancel sends session/cancel; idempotent. Pending permission prompts of
// the session are answered cancelled.
func (a *Agent) Cancel(sessionID string) {
	_ = a.conn.Notify("session/cancel", map[string]any{"sessionId": sessionID})
	a.mu.Lock()
	cs := a.cancels[sessionID]
	a.mu.Unlock()
	if cs != nil {
		cs.fire()
	}
}

// sessionCancelled is a channel closed when the session's turn is cancelled
// (nil when no turn runs).
func (a *Agent) sessionCancelled(sessionID string) <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	if cs := a.cancels[sessionID]; cs != nil {
		return cs.ch
	}
	return nil
}

// Roots are the session's granted directories.
func (a *Agent) Roots(sessionID string) []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.roots[sessionID]
}
