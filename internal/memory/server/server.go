package server

import (
	"context"
	"crypto/sha256"
	_ "embed" // tools.json
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tryatlas/atlas-cli/internal/memory/record"
	"github.com/tryatlas/atlas-cli/internal/project"
)

// Name is the server name the desktop uses; agent config files register the
// CLI's entry as "atlas" (D10) and the server answers to either.
const Name = "atlas_memory"

//go:embed tools.json
var toolsJSON []byte

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

var defs struct {
	Instructions string    `json:"instructions"`
	Tools        []toolDef `json:"tools"`
}

func init() {
	if err := json.Unmarshal(toolsJSON, &defs); err != nil {
		panic("memory server: tools.json: " + err.Error())
	}
}

// Instructions is the server's instructions text, verbatim (SPEC §5.5).
func Instructions() string { return defs.Instructions }

// Grant names the caller of one session: who, from where.
type Grant struct {
	SessionID string
	Agent     string
	Cwd       string // the launch dir the agent runs in
}

// Server serves shared memory to agent sessions.
type Server struct {
	Version string
	Now     func() time.Time

	mu     sync.Mutex
	stores map[string]*record.Store // by scope root
	clocks map[string]int64         // per session: newest updated_at seen
	read   map[string]bool          // sessions that have read memory
}

// New returns a server.
func New(version string) *Server {
	return &Server{Version: version, Now: time.Now, stores: map[string]*record.Store{},
		clocks: map[string]int64{}, read: map[string]bool{}}
}

// Close closes every open record.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, st := range s.stores {
		_ = st.Close()
		delete(s.stores, k)
	}
}

func (s *Server) now() int64 { return s.Now().UnixMilli() }

// store resolves cwd → scope root → the record, opened once per scope.
func (s *Server) store(ctx context.Context, cwd string) (*record.Store, error) {
	scope := project.ScopeRoot(ctx, cwd)
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.stores[scope]; ok {
		return st, nil
	}
	st, err := record.Open(scope, true)
	if err != nil {
		return nil, err
	}
	s.stores[scope] = st
	return st, nil
}

// sharingOn reads the gate of the Project the session runs in (the nearest
// binding above its cwd, else the cwd), every call.
func sharingOn(ctx context.Context, cwd string) bool {
	if p, err := project.Discover(ctx, cwd); err == nil {
		return project.MemorySharing(p.Root)
	}
	return project.MemorySharing(cwd)
}

// SessionStarted records the session in the record (session_start event and
// sessions row) when sharing is on.
func (s *Server) SessionStarted(ctx context.Context, g Grant) {
	if g.SessionID == "" || !sharingOn(ctx, g.Cwd) {
		return
	}
	if st, err := s.store(ctx, g.Cwd); err == nil {
		_ = st.SessionStart(ctx, g.SessionID, g.Agent, s.now())
	}
}

// Fold folds a captured event (plan_set, file_changed) into the record when
// sharing is on for the session's cwd.
func (s *Server) Fold(ctx context.Context, g Grant, ev record.Event) error {
	if !sharingOn(ctx, g.Cwd) {
		return nil
	}
	st, err := s.store(ctx, g.Cwd)
	if err != nil {
		return err
	}
	writeProjectJSON(g.Cwd)
	ev.Agent, ev.Session = g.Agent, g.SessionID
	if ev.At == 0 {
		ev.At = s.now()
	}
	_, err = st.Fold(ctx, ev)
	return err
}

// SessionEnded records session_end and drops the session's clock.
func (s *Server) SessionEnded(ctx context.Context, g Grant) {
	s.mu.Lock()
	delete(s.clocks, g.SessionID)
	delete(s.read, g.SessionID)
	s.mu.Unlock()
	if g.SessionID == "" || !sharingOn(ctx, g.Cwd) {
		return
	}
	if st, err := s.store(ctx, g.Cwd); err == nil {
		_ = st.SessionEnd(ctx, g.SessionID, g.Agent, s.now())
	}
}

// writeProjectJSON writes <launch dir>/.atlas/project.json on the first
// record write if it is absent: {"projectId":"<12 hex of sha256(cwd)>"}.
func writeProjectJSON(cwd string) {
	dir := filepath.Join(cwd, ".atlas")
	p := filepath.Join(dir, "project.json")
	if _, err := os.Stat(p); err == nil {
		return
	}
	sum := sha256.Sum256([]byte(strings.TrimRight(cwd, "/")))
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(p, []byte(`{"projectId":"`+hex.EncodeToString(sum[:])[:12]+`"}`), 0o644) //nolint:gosec // G306: a label, not a secret
}

// ── JSON-RPC ─────────────────────────────────────────────────────────────────

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Protocol versions the server speaks, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Negotiate picks the client's version when supported, else the newest.
func Negotiate(requested string) string {
	for _, v := range protocolVersions {
		if v == requested {
			return v
		}
	}
	return protocolVersions[0]
}

// Handle processes one JSON-RPC message for a session and returns the
// response, or nil for a notification. initialize also reports the version
// it negotiated.
func (s *Server) Handle(ctx context.Context, g Grant, msg []byte) (resp []byte, negotiated string) {
	var req rpcRequest
	if err := json.Unmarshal(msg, &req); err != nil || req.Method == "" {
		out, _ := json.Marshal(rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{-32700, "parse error"}})
		return out, ""
	}
	if len(req.ID) == 0 || string(req.ID) == "null" { // a notification
		return nil, ""
	}
	r := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		negotiated = Negotiate(p.ProtocolVersion)
		r.Result = map[string]any{
			"protocolVersion": negotiated,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": Name, "version": s.Version},
			"instructions":    defs.Instructions,
		}
		s.SessionStarted(ctx, g)
	case "ping":
		r.Result = map[string]any{}
	case "tools/list":
		r.Result = map[string]any{"tools": defs.Tools, "ttlMs": 3600000, "cacheScope": "private"}
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			r.Error = &rpcError{-32602, "invalid params"}
			break
		}
		r.Result = s.call(ctx, g, p.Name, p.Arguments)
	default:
		r.Error = &rpcError{-32601, "method not found: " + req.Method}
	}
	out, _ := json.Marshal(r)
	return out, negotiated
}

// toolResult is one text block holding a JSON object, or an error message.
func toolResult(v any) map[string]any {
	b, _ := marshal(v)
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}
}

func toolError(msg string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": msg}}, "isError": true}
}

// marshal encodes without HTML escaping.
func marshal(v any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(b.String(), "\n")), nil
}

var readTools = map[string]bool{
	"memory_briefing": true, "memory_changes": true, "memory_search": true, "memory_get": true,
	"memory_list": true, "memory_history": true, "memory_why": true,
}

const offNote = "shared memory is switched off for this project"

// call runs one tool with the desktop's per-call rules (SPEC §5.5).
func (s *Server) call(ctx context.Context, g Grant, name string, args json.RawMessage) map[string]any {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	isRead := readTools[name]
	if isRead {
		s.mu.Lock()
		s.read[g.SessionID] = true
		s.mu.Unlock()
	}
	if !sharingOn(ctx, g.Cwd) {
		if isRead {
			return toolResult(map[string]any{"entries": []any{}, "note": offNote})
		}
		return toolError(offNote)
	}
	st, err := s.store(ctx, g.Cwd)
	if err != nil {
		if isRead {
			return toolError("memory unavailable: " + err.Error())
		}
		return toolError("not remembered: " + err.Error())
	}
	t := &call{s: s, st: st, g: g, ctx: ctx}
	var out any
	switch name {
	case "memory_briefing":
		out, err = t.briefing()
	case "memory_changes":
		out, err = t.changes()
	case "memory_search":
		out, err = t.search(args)
	case "memory_get":
		out, err = t.get(args)
	case "memory_list":
		out, err = t.list(args)
	case "memory_history":
		out, err = t.history(args)
	case "memory_why":
		out, err = t.why(args)
	case "memory_remember":
		out, err = t.remember(args)
		if err == nil {
			writeProjectJSON(g.Cwd)
		}
	case "memory_feedback":
		out, err = t.feedback(args)
	case "memory_forget":
		out, err = t.forget(args)
	default:
		return toolError("unknown tool " + name)
	}
	if err != nil {
		var te toolErr
		if errors.As(err, &te) {
			return toolError(te.msg)
		}
		switch {
		case isRead:
			return toolError("memory unavailable: " + err.Error())
		case name == "memory_forget":
			return toolError("not forgotten: " + err.Error())
		default:
			return toolError("not remembered: " + err.Error())
		}
	}
	return toolResult(out)
}

// toolErr is an error whose message is returned verbatim.
type toolErr struct{ msg string }

func (e toolErr) Error() string { return e.msg }

func errorf(format string, a ...any) error { return toolErr{fmt.Sprintf(format, a...)} }
