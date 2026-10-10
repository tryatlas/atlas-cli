// Package acptest is a scripted ACP agent for tests: a test binary re-runs
// itself as the agent (Main), and the scenario comes from the environment.
package acptest

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
)

// EnvScenario selects the agent's behaviour; EnvCaps is extra JSON merged
// into agentCapabilities.
const (
	EnvScenario = "ATLAS_FAKE_AGENT"
	EnvCaps     = "ATLAS_FAKE_AGENT_CAPS"
	EnvLog      = "ATLAS_FAKE_AGENT_LOG" // every received line is appended here
)

// Main runs the agent on stdin/stdout when the scenario variable is set and
// exits; otherwise it returns.
func Main() {
	sc := os.Getenv(EnvScenario)
	if sc == "" {
		return
	}
	a := &agent{scenario: sc, w: os.Stdout, pending: map[string]chan json.RawMessage{}}
	a.serve(os.Stdin)
	os.Exit(0)
}

type msg struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}

type agent struct {
	scenario string
	w        io.Writer
	wmu      sync.Mutex
	mu       sync.Mutex
	pending  map[string]chan json.RawMessage
	next     int
	authed   bool
	cwd      string
}

func (a *agent) send(v any) {
	b, _ := json.Marshal(v)
	a.wmu.Lock()
	_, _ = a.w.Write(append(b, '\n'))
	a.wmu.Unlock()
}

func (a *agent) reply(id json.RawMessage, result any) {
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (a *agent) fail(id json.RawMessage, code int, message, details string) {
	e := map[string]any{"code": code, "message": message}
	if details != "" {
		e["data"] = map[string]any{"details": details}
	}
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": e})
}

func (a *agent) notify(sessionID string, update map[string]any) {
	a.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": update}})
}

// call sends a client request and waits for its result.
func (a *agent) call(method string, params any) json.RawMessage {
	a.mu.Lock()
	a.next++
	id := "c" + strconv.Itoa(a.next)
	ch := make(chan json.RawMessage, 1)
	a.pending[id] = ch
	a.mu.Unlock()
	a.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return <-ch
}

func (a *agent) serve(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	logf, _ := os.OpenFile(os.Getenv(EnvLog), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G703: a test-chosen log path
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if logf != nil {
				_, _ = logf.Write(line)
			}
			var m msg
			if json.Unmarshal(line, &m) == nil {
				a.handle(m)
			}
		}
		if err != nil {
			return
		}
	}
}

func (a *agent) handle(m msg) {
	if m.Method == "" && len(m.ID) > 0 { // a response to our call
		var id string
		_ = json.Unmarshal(m.ID, &id)
		a.mu.Lock()
		ch := a.pending[id]
		delete(a.pending, id)
		a.mu.Unlock()
		if ch != nil {
			if len(m.Error) > 0 {
				ch <- m.Error
			} else {
				ch <- m.Result
			}
		}
		return
	}
	switch m.Method {
	case "initialize":
		caps := map[string]any{"loadSession": true, "mcpCapabilities": map[string]any{"http": true},
			"promptCapabilities":  map[string]any{"image": false},
			"sessionCapabilities": map[string]any{"list": map[string]any{}}}
		if extra := os.Getenv(EnvCaps); extra != "" {
			var x map[string]any
			_ = json.Unmarshal([]byte(extra), &x)
			for k, v := range x {
				caps[k] = v
			}
		}
		methods := []any{map[string]any{"id": "login", "name": "Log in", "type": "terminal", "args": []string{"--login"}}}
		a.reply(m.ID, map[string]any{"protocolVersion": 1, "agentInfo": map[string]any{"name": "fake-agent", "version": "0.0.1"},
			"authMethods": methods, "agentCapabilities": caps})
	case "authenticate":
		a.authed = true
		a.fail(m.ID, -32603, "Internal error", "Method not implemented.")
	case "session/new", "session/load", "session/resume":
		if a.scenario == "auth" && !a.authed {
			a.fail(m.ID, -32000, "Authentication required", "")
			return
		}
		var p struct {
			Cwd       string `json:"cwd"`
			SessionID string `json:"sessionId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		a.cwd = p.Cwd
		id := p.SessionID
		if id == "" {
			id = "sess-1"
		}
		if m.Method == "session/load" {
			a.notify(id, map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "old prompt"}})
			a.notify(id, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "old answer"}})
		}
		a.reply(m.ID, map[string]any{"sessionId": id,
			"configOptions": []any{map[string]any{"id": "model", "name": "Model", "category": "model", "type": "select", "currentValue": "fake-1",
				"options": []any{map[string]any{"value": "fake-1", "name": "Fake 1"}, map[string]any{"value": "fake-2", "name": "Fake 2"}}},
				map[string]any{"id": "mode", "name": "Mode", "category": "mode", "type": "select", "currentValue": "ask",
					"options": []any{map[string]any{"value": "ask", "name": "Ask"}, map[string]any{"value": "code", "name": "Code"}}}}})
	case "session/set_mode", "session/set_config_option":
		a.reply(m.ID, map[string]any{})
	case "session/list":
		a.reply(m.ID, map[string]any{"sessions": []any{map[string]any{"sessionId": "sess-0", "cwd": a.cwd, "title": "Earlier", "updatedAt": "2026-10-10T00:00:00Z"}}})
	case "session/cancel":
	case "session/prompt":
		go a.prompt(m)
	default:
		if len(m.ID) > 0 {
			a.fail(m.ID, -32601, "Method not found", "")
		}
	}
}

func (a *agent) prompt(m msg) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(m.Params, &p)
	sid := p.SessionID
	text := func(kind, id, s string) {
		a.notify(sid, map[string]any{"sessionUpdate": kind, "messageId": id, "content": map[string]any{"type": "text", "text": s}})
	}
	text("agent_thought_chunk", "m0", "Thinking about it.")
	text("agent_message_chunk", "m1", "I'll write ")
	text("agent_message_chunk", "m1", "the notes.")
	path := filepath.Join(a.cwd, "notes.md")
	input := map[string]any{"file_path": path, "content": "# Notes\n"}
	a.notify(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "t1", "title": "Write notes.md", "kind": "edit",
		"status": "pending", "locations": []any{map[string]any{"path": path}}, "rawInput": input})
	res := a.call("session/request_permission", map[string]any{"sessionId": sid,
		"toolCall": map[string]any{"toolCallId": "t1", "title": "Write notes.md"},
		"options": []any{map[string]any{"optionId": "yes", "name": "Allow", "kind": "allow_once"},
			map[string]any{"optionId": "no", "name": "Reject", "kind": "reject_once"}}})
	var out struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	_ = json.Unmarshal(res, &out)
	if out.Outcome.OptionID != "yes" {
		a.notify(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "failed"})
		a.reply(m.ID, map[string]any{"stopReason": "end_turn"})
		return
	}
	a.notify(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "in_progress"})
	a.call("fs/write_text_file", map[string]any{"sessionId": sid, "path": path, "content": "# Notes\n"})
	a.notify(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "t1", "status": "completed",
		"content": []any{map[string]any{"type": "diff", "path": path, "oldText": nil, "newText": "# Notes\n"}}})
	a.notify(sid, map[string]any{"sessionUpdate": "plan", "entries": []any{map[string]any{"content": "Write the notes", "priority": "high", "status": "completed"}}})
	a.notify(sid, map[string]any{"sessionUpdate": "usage_update", "used": 100, "size": 1000})
	a.notify(sid, map[string]any{"sessionUpdate": "some_future_update", "x": 1})
	text("agent_message_chunk", "m2", "Done.")
	a.reply(m.ID, map[string]any{"stopReason": "end_turn", "usage": map[string]any{"inputTokens": 10, "outputTokens": 5, "totalTokens": 15}})
}
