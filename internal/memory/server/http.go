package server

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// Authenticator turns a request into a grant; ok false answers 401.
type Authenticator func(r *http.Request) (Grant, bool)

// HTTPHandler serves MCP streamable HTTP at /mcp: POST carries one JSON-RPC
// message and gets one JSON answer; Mcp-Session-Id names the session after
// initialize. Server-initiated streams are not offered (GET answers 405).
type HTTPHandler struct {
	Server *Server
	Auth   Authenticator

	mu       sync.Mutex
	sessions map[string]*mcpSession
}

type mcpSession struct {
	grant    Grant
	protocol string
}

// NewHTTPHandler wires a server to an authenticator.
func NewHTTPHandler(s *Server, auth Authenticator) *HTTPHandler {
	return &HTTPHandler{Server: s, Auth: auth, sessions: map[string]*mcpSession{}}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	grant, ok := h.Auth(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodPost:
	case http.MethodDelete:
		h.mu.Lock()
		s := h.sessions[r.Header.Get("Mcp-Session-Id")]
		delete(h.sessions, r.Header.Get("Mcp-Session-Id"))
		h.mu.Unlock()
		if s != nil {
			h.Server.SessionEnded(r.Context(), s.grant)
		}
		w.WriteHeader(http.StatusOK)
		return
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var probe struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &probe)
	sid := r.Header.Get("Mcp-Session-Id")
	if probe.Method == "initialize" {
		sid = newID()
		s := &mcpSession{grant: grant}
		h.mu.Lock()
		h.sessions[sid] = s
		h.mu.Unlock()
	} else {
		h.mu.Lock()
		s := h.sessions[sid]
		h.mu.Unlock()
		if s == nil {
			// Not JSON-RPC on purpose: the bridge replays initialize on this.
			http.Error(w, "unknown MCP session", http.StatusNotFound)
			return
		}
		grant = s.grant
	}
	resp, negotiated := h.Server.Handle(r.Context(), grant, body)
	if negotiated != "" {
		h.mu.Lock()
		if s := h.sessions[sid]; s != nil {
			s.protocol = negotiated
		}
		h.mu.Unlock()
	}
	w.Header().Set("Mcp-Session-Id", sid)
	if resp == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.Copy(w, bytes.NewReader(resp))
}

// ── Bearer tokens ─────────────────────────────────────────────

// Tokens maps per-session bearer tokens to grants. Tokens live in memory only.
type Tokens struct {
	mu     sync.Mutex
	grants map[string]Grant
}

// NewTokens returns an empty table.
func NewTokens() *Tokens { return &Tokens{grants: map[string]Grant{}} }

// Mint returns a 64-hex token (two UUIDv4 in simple form) for a grant.
func (t *Tokens) Mint(g Grant) string {
	tok := uuidSimple() + uuidSimple()
	t.mu.Lock()
	t.grants[tok] = g
	t.mu.Unlock()
	return tok
}

// Revoke drops a token.
func (t *Tokens) Revoke(tok string) {
	t.mu.Lock()
	delete(t.grants, tok)
	t.mu.Unlock()
}

// Auth checks "Authorization: Bearer <token>".
func (t *Tokens) Auth(r *http.Request) (Grant, bool) {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(p) || h[:len(p)] != p {
		return Grant{}, false
	}
	t.mu.Lock()
	g, ok := t.grants[h[len(p):]]
	t.mu.Unlock()
	return g, ok
}

// uuidSimple is a random UUID v4 as 32 lowercase hex characters.
func uuidSimple() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// UUID is a random UUID v4, hyphenated.
func UUID() string {
	s := uuidSimple()
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}
