package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// handler serves the agent's notifications and requests (evidence-acp.md
// §3.8–3.10). Notifications run on the serial queue; requests on their own
// goroutines, so a permission prompt waiting on the user blocks nothing.
type handler struct{ a *Agent }

func (h *handler) Notify(method string, params json.RawMessage) {
	switch method {
	case "session/update":
		var n SessionNotification
		if json.Unmarshal(params, &n) != nil || n.SessionID == "" {
			return
		}
		var u Update
		if json.Unmarshal(n.Update, &u) != nil || !KnownUpdates[u.Kind] {
			return // unknown or broken variants are dropped
		}
		h.a.mu.Lock()
		_, known := h.a.roots[n.SessionID]
		h.a.mu.Unlock()
		if !known {
			return // unknown sessionId: dropped
		}
		h.a.host.Update(n.SessionID, u)
	case "session/complete_elicitation", "elicitation/complete":
		var p struct {
			ElicitationID string `json:"elicitationId"`
		}
		if json.Unmarshal(params, &p) == nil && p.ElicitationID != "" {
			h.a.host.ElicitationComplete(p.ElicitationID)
		}
	}
}

func (h *handler) Request(ctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
	switch method {
	case "fs/read_text_file":
		return h.readTextFile(params)
	case "fs/write_text_file":
		return h.writeTextFile(params)
	case "terminal/create":
		return h.a.terms.create(ctx, params)
	case "terminal/output":
		return h.a.terms.output(params)
	case "terminal/wait_for_exit":
		return h.a.terms.waitForExit(ctx, params)
	case "terminal/kill":
		return h.a.terms.kill(params)
	case "terminal/release":
		return h.a.terms.release(params)
	case "session/request_permission":
		return h.permission(ctx, params)
	case "session/request_elicitation", "elicitation/create":
		return h.elicit(ctx, params)
	}
	return nil, &RPCError{Code: CodeMethodNotFound, Message: "Method not found"}
}

// KnownUpdates are the session/update variants the desktop reads
// (evidence-acp.md §3.8); anything else falls through.
var KnownUpdates = map[string]bool{
	"user_message_chunk": true, "agent_message_chunk": true, "agent_thought_chunk": true,
	"tool_call": true, "tool_call_update": true, "plan": true, "session_info_update": true,
	"available_commands_update": true, "current_mode_update": true, "config_option_update": true,
	"usage_update": true,
}

// ── fs ───────────────────────────────────────────────────────────────────────

// contained resolves p against the session's roots: lexical normalisation,
// then the deepest existing ancestor canonicalised (symlink-safe).
func contained(roots []string, p string) (string, error) {
	if !filepath.IsAbs(p) {
		if len(roots) == 0 {
			return "", errors.New("no session roots")
		}
		p = filepath.Join(roots[0], p)
	}
	p = filepath.Clean(p)
	resolved := canonical(p)
	for _, r := range roots {
		cr := canonical(filepath.Clean(r))
		if resolved == cr || strings.HasPrefix(resolved, cr+string(os.PathSeparator)) {
			return p, nil
		}
	}
	return "", errors.New("path is outside the session's directories: " + p)
}

// canonical resolves the deepest existing ancestor and re-appends the rest.
func canonical(p string) string {
	rest := ""
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			if rest == "" {
				return r
			}
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		if rest == "" {
			rest = filepath.Base(cur)
		} else {
			rest = filepath.Join(filepath.Base(cur), rest)
		}
		cur = parent
	}
}

func (h *handler) readTextFile(params json.RawMessage) (any, *RPCError) {
	var p struct {
		SessionID string `json:"sessionId"`
		Path      string `json:"path"`
		Line      *int   `json:"line"`
		Limit     *int   `json:"limit"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "Invalid params"}
	}
	path, err := contained(h.a.Roots(p.SessionID), p.Path)
	if err != nil {
		return nil, Errorf("%v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, Errorf("%v", err)
	}
	if info.IsDir() {
		return nil, Errorf("%s is a directory", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: contained in the session's roots
	if err != nil {
		return nil, Errorf("%v", err)
	}
	defer f.Close()
	if p.Line == nil && p.Limit == nil {
		b, err := os.ReadFile(path) //nolint:gosec // G304: contained in the session's roots
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return map[string]any{"content": string(b)}, nil
	}
	start := 1
	if p.Line != nil && *p.Line > 1 {
		start = *p.Line
	}
	var b strings.Builder
	r := bufio.NewReader(f)
	n, taken := 0, 0
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			n++
			if n >= start && (p.Limit == nil || taken < *p.Limit) {
				b.WriteString(line)
				taken++
			}
		}
		if err != nil || (p.Limit != nil && taken >= *p.Limit) {
			break
		}
	}
	return map[string]any{"content": b.String()}, nil
}

func (h *handler) writeTextFile(params json.RawMessage) (any, *RPCError) {
	var p struct {
		SessionID string `json:"sessionId"`
		Path      string `json:"path"`
		Content   string `json:"content"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "Invalid params"}
	}
	path, err := contained(h.a.Roots(p.SessionID), p.Path)
	if err != nil {
		return nil, Errorf("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, Errorf("%v", err)
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(p.Content), mode); err != nil {
		return nil, Errorf("%v", err)
	}
	return map[string]any{}, nil
}

// ── permission ───────────────────────────────────────────────────────────────

// permission asks the host. The option's kind is read back from the
// request's own options, never from the host. A cancelled turn answers
// every pending prompt cancelled.
func (h *handler) permission(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var req PermissionRequest
	if json.Unmarshal(params, &req) != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "Invalid params"}
	}
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if ch := h.a.sessionCancelled(req.SessionID); ch != nil {
		go func() {
			select {
			case <-ch:
				cancel()
			case <-pctx.Done():
			}
		}()
	}
	id, ok := h.a.host.Permission(pctx, req)
	if ok && pctx.Err() == nil {
		for _, o := range req.Options {
			if o.OptionID == id {
				return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": id}}, nil
			}
		}
	}
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
}

// ── elicitation ──────────────────────────────────────────────────────────────

// Elicitation is a request for user input: a form, or a URL to open.
type Elicitation struct {
	SessionID       string          `json:"sessionId"`
	ToolCallID      string          `json:"toolCallId"`
	ElicitationID   string          `json:"elicitationId"`
	Mode            string          `json:"mode"` // form | url
	Message         string          `json:"message"`
	RequestedSchema json.RawMessage `json:"requestedSchema"`
	URL             string          `json:"url"`
}

// ElicitationAnswer is accept (with content), decline, or cancel.
type ElicitationAnswer struct {
	Action  string         // accept | decline | cancel
	Content map[string]any // accept only
}

// validURL: http(s) with a host only.
func validURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

func (h *handler) elicit(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var e Elicitation
	if json.Unmarshal(params, &e) != nil {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "Invalid params"}
	}
	switch e.Mode {
	case "form":
	case "url":
		if !validURL(e.URL) {
			return map[string]any{"action": "decline"}, nil
		}
	default:
		return map[string]any{"action": "decline"}, nil
	}
	ans := h.a.host.Elicit(ctx, e)
	switch ans.Action {
	case "accept":
		content := ans.Content
		if content == nil {
			content = map[string]any{}
		}
		return map[string]any{"action": "accept", "content": content}, nil
	case "decline":
		return map[string]any{"action": "decline"}, nil
	}
	return map[string]any{"action": "cancel"}, nil
}
