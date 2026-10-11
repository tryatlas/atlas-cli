package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// Forwarder carries one MCP message to the memory server.
type Forwarder interface {
	Post(ctx context.Context, msg []byte, sessionID, protocol string) (*Reply, error)
	Close(ctx context.Context, sessionID string)
}

// Reply is the server's answer to one message.
type Reply struct {
	Status      int
	SessionID   string
	ContentType string
	Body        []byte
}

// RunBridge reads one JSON-RPC message per line from in, forwards it, and
// writes every message of the answer (a JSON body or each SSE data: line) to
// out as one line, whatever the status (ADR-0019). When the server no longer
// knows the session (a 404 that is not JSON-RPC) it replays initialize and
// notifications/initialized and retries once.
func RunBridge(ctx context.Context, in io.Reader, out io.Writer, f Forwarder) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	w := bufio.NewWriter(out)
	var sid, protocol string
	var initMsg, initializedMsg []byte
	defer func() { f.Close(context.Background(), sid) }()

	send := func(msg []byte) (*Reply, error) {
		p := protocol
		if isMethod(msg, "initialize") {
			p = "" // not sent on initialize
		}
		return f.Post(ctx, msg, sid, p)
	}
	learn := func(msg []byte, r *Reply) {
		if r.SessionID != "" {
			sid = r.SessionID
		}
		if isMethod(msg, "initialize") {
			if v := protocolOf(r); v != "" {
				protocol = v
			}
		}
	}

	for sc.Scan() {
		msg := bytes.TrimSpace(sc.Bytes())
		if len(msg) == 0 {
			continue
		}
		msg = append([]byte(nil), msg...)
		switch {
		case isMethod(msg, "initialize"):
			initMsg = msg
		case isMethod(msg, "notifications/initialized"):
			initializedMsg = msg
		}
		r, err := send(msg)
		if err != nil {
			if werr := writeError(w, msg, err); werr != nil {
				return werr
			}
			continue
		}
		if r.Status == http.StatusNotFound && !isJSONRPC(r.Body) && initMsg != nil && !isMethod(msg, "initialize") {
			sid = ""
			if ir, err := send(initMsg); err == nil {
				learn(initMsg, ir)
				if initializedMsg != nil {
					_, _ = send(initializedMsg)
				}
				if rr, err := send(msg); err == nil {
					r = rr
				}
			}
		}
		learn(msg, r)
		if len(messages(r)) == 0 && requestID(msg) != nil {
			// The server answered with no JSON-RPC body (a bare 4xx): a
			// request must still get an answer or the agent waits forever. A
			// request before initialize (Antigravity probes server/discover
			// first) is "method not found", which makes the client fall back
			// to initialize.
			code, text := -32000, fmt.Sprintf("atlas memory server said %d", r.Status)
			if initMsg == nil {
				code, text = -32601, "method not found before initialize"
			}
			if err := writeRPCError(w, msg, code, text); err != nil {
				return err
			}
			continue
		}
		if err := writeReply(w, r); err != nil {
			return err
		}
	}
	return sc.Err()
}

// requestID is a message's id, or nil for a notification.
func requestID(msg []byte) json.RawMessage {
	var m struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(msg, &m) != nil || len(m.ID) == 0 || string(m.ID) == "null" {
		return nil
	}
	return m.ID
}

// writeRPCError answers a request with a JSON-RPC error.
func writeRPCError(w *bufio.Writer, msg []byte, code int, text string) error {
	id := requestID(msg)
	if id == nil {
		return nil // a notification: nothing to answer
	}
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": text}})
	if _, err := w.Write(append(b, '\n')); err != nil {
		return err
	}
	return w.Flush()
}

func isMethod(msg []byte, method string) bool {
	var m struct {
		Method string `json:"method"`
	}
	return json.Unmarshal(msg, &m) == nil && m.Method == method
}

func isJSONRPC(body []byte) bool {
	var m struct {
		JSONRPC string `json:"jsonrpc"`
	}
	return json.Unmarshal(bytes.TrimSpace(body), &m) == nil && m.JSONRPC == "2.0"
}

func protocolOf(r *Reply) string {
	for _, line := range messages(r) {
		var m struct {
			Result struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"result"`
		}
		if json.Unmarshal(line, &m) == nil && m.Result.ProtocolVersion != "" {
			return m.Result.ProtocolVersion
		}
	}
	return ""
}

// messages splits a reply into JSON-RPC messages: SSE data: lines or the body.
func messages(r *Reply) [][]byte {
	if strings.HasPrefix(r.ContentType, "text/event-stream") {
		var out [][]byte
		for _, line := range bytes.Split(r.Body, []byte("\n")) {
			if d, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:")); ok {
				if d = bytes.TrimSpace(d); len(d) > 0 {
					out = append(out, d)
				}
			}
		}
		return out
	}
	if b := bytes.TrimSpace(r.Body); len(b) > 0 && json.Valid(b) {
		var compact bytes.Buffer
		_ = json.Compact(&compact, b)
		return [][]byte{compact.Bytes()}
	}
	return nil
}

func writeReply(w *bufio.Writer, r *Reply) error {
	for _, m := range messages(r) {
		if _, err := w.Write(append(m, '\n')); err != nil {
			return err
		}
	}
	return w.Flush()
}

// writeError answers a request the bridge could not deliver with a JSON-RPC
// error, so the agent hears about it instead of the bridge ending.
func writeError(w *bufio.Writer, msg []byte, err error) error {
	return writeRPCError(w, msg, -32000, "atlas memory server unreachable: "+err.Error())
}

// ── HTTP forwarder ───────────────────────────────────────────────────────────

// HTTPForwarder posts to a loopback memory server over HTTP. It never uses
// a proxy.
type HTTPForwarder struct {
	URL     string
	Headers http.Header // Authorization: Bearer <ATLAS_MCP_TOKEN>
	Client  *http.Client
}

// CheckLoopbackURL refuses anything but plain http to localhost or a loopback
// IP, and any URL with userinfo (ADR-0019).
func CheckLoopbackURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Scheme != "http" || u.User != nil || u.Host == "" {
		return fmt.Errorf("refusing %q: only plain http to a loopback address", raw)
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("refusing %q: not a loopback address", raw)
}

// NewTCPForwarder targets http://127.0.0.1:<port>/mcp with a bearer token.
func NewTCPForwarder(rawURL, token string) (*HTTPForwarder, error) {
	if err := CheckLoopbackURL(rawURL); err != nil {
		return nil, err
	}
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	return &HTTPForwarder{URL: rawURL, Headers: h,
		Client: &http.Client{Transport: &http.Transport{Proxy: nil}}}, nil
}

// Post sends one message.
func (f *HTTPForwarder) Post(ctx context.Context, msg []byte, sessionID, protocol string) (*Reply, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.URL, bytes.NewReader(msg))
	if err != nil {
		return nil, err
	}
	for k, v := range f.Headers {
		req.Header[k] = v
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	return &Reply{Status: resp.StatusCode, SessionID: resp.Header.Get("Mcp-Session-Id"),
		ContentType: resp.Header.Get("Content-Type"), Body: body}, nil
}

// Close ends the MCP session.
func (f *HTTPForwarder) Close(ctx context.Context, sessionID string) {
	if sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, f.URL, nil)
	if err != nil {
		return
	}
	for k, v := range f.Headers {
		req.Header[k] = v
	}
	req.Header.Set("Mcp-Session-Id", sessionID)
	if resp, err := f.Client.Do(req); err == nil {
		_ = resp.Body.Close()
	}
}

// ── In-process forwarder ─────────────────────────────────────────────────────

// LocalForwarder serves a session in this process (tests, and any caller
// that hosts the server itself).
type LocalForwarder struct{ h *HTTPHandler }

// NewLocalForwarder wraps a server for one grant.
func NewLocalForwarder(s *Server, g Grant) *LocalForwarder {
	return &LocalForwarder{h: NewHTTPHandler(s, func(*http.Request) (Grant, bool) { return g, true })}
}

// Post handles one message in process.
func (f *LocalForwarder) Post(ctx context.Context, msg []byte, sessionID, _ string) (*Reply, error) {
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(msg)).WithContext(ctx)
	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return &Reply{Status: rec.Code, SessionID: rec.Header().Get("Mcp-Session-Id"),
		ContentType: rec.Header().Get("Content-Type"), Body: rec.Body.Bytes()}, nil
}

// Close ends the session.
func (f *LocalForwarder) Close(ctx context.Context, sessionID string) {
	if sessionID == "" {
		return
	}
	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil).WithContext(ctx)
	req.Header.Set("Mcp-Session-Id", sessionID)
	f.h.ServeHTTP(httptest.NewRecorder(), req)
}

// NewUnixForwarder targets a listener's owner-only control socket, where the
// grant travels as query parameters (the socket admits only this user).
func NewUnixForwarder(sock string, g Grant) *HTTPForwarder {
	q := url.Values{"agent": {g.Agent}, "cwd": {g.Cwd}}
	if g.SessionID != "" {
		q.Set("session", g.SessionID)
	}
	return &HTTPForwarder{URL: "http://atlas/mcp?" + q.Encode(), Headers: http.Header{},
		Client: &http.Client{Transport: &http.Transport{Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			}}}}
}
