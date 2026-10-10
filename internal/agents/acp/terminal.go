package acp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unicode/utf8"
)

// Client-side terminals (evidence-acp.md §3.10): created only after the
// user allows it, run in their own process group, output kept in a buffer.

const defaultOutputLimit = 1 << 20

type terminal struct {
	cmd      *exec.Cmd
	mu       sync.Mutex
	buf      []byte
	limit    int
	cut      bool
	done     chan struct{}
	exitCode *int
	signal   *string
}

func (t *terminal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		drop := len(t.buf) - t.limit
		for drop < len(t.buf) && !utf8.RuneStart(t.buf[drop]) {
			drop++ // keep a character boundary
		}
		t.buf = append([]byte(nil), t.buf[drop:]...)
		t.cut = true
	}
	return len(p), nil
}

func (t *terminal) exitStatus() map[string]any {
	select {
	case <-t.done:
	default:
		return nil
	}
	return map[string]any{"exitCode": t.exitCode, "signal": t.signal}
}

type terminals struct {
	a  *Agent
	mu sync.Mutex
	m  map[string]*terminal
}

func newTerminals(a *Agent) *terminals { return &terminals{a: a, m: map[string]*terminal{}} }

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32]
}

// dropLoaderEnv removes DYLD_* and LD_* variables.
func dropLoaderEnv(env []NameValue) []NameValue {
	var out []NameValue
	for _, kv := range env {
		if strings.HasPrefix(kv.Name, "DYLD_") || strings.HasPrefix(kv.Name, "LD_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func (ts *terminals) create(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	var p struct {
		SessionID       string      `json:"sessionId"`
		Command         string      `json:"command"`
		Args            []string    `json:"args"`
		Env             []NameValue `json:"env"`
		Cwd             string      `json:"cwd"`
		OutputByteLimit *int        `json:"outputByteLimit"`
	}
	if json.Unmarshal(params, &p) != nil || p.Command == "" {
		return nil, &RPCError{Code: CodeInvalidParams, Message: "Invalid params"}
	}
	roots := ts.a.Roots(p.SessionID)
	cwd := p.Cwd
	if cwd == "" && len(roots) > 0 {
		cwd = roots[0]
	}
	dir, err := contained(roots, cwd)
	if err != nil {
		return nil, Errorf("%v", err)
	}
	line := strings.TrimSpace(p.Command + " " + strings.Join(p.Args, " "))
	title := "Run `" + line + "` in " + dir
	toolCall, _ := json.Marshal(map[string]any{"toolCallId": "terminal-" + newUUID(), "title": title, "kind": "execute"})
	id, ok := ts.a.host.Permission(ctx, PermissionRequest{SessionID: p.SessionID, ToolCall: toolCall, Options: []PermissionOption{
		{OptionID: "allow_once", Name: "Allow", Kind: "allow_once"},
		{OptionID: "reject_once", Name: "Reject", Kind: "reject_once"},
	}})
	if !ok || id != "allow_once" {
		return nil, Errorf("the user did not allow running %s", line)
	}
	cmd := exec.Command(p.Command, p.Args...) //nolint:gosec // G204: the user allowed this command
	cmd.Dir = dir
	extra := map[string]string{}
	for _, kv := range dropLoaderEnv(p.Env) {
		extra[kv.Name] = kv.Value
	}
	cmd.Env = childEnv(extra)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	limit := defaultOutputLimit
	if p.OutputByteLimit != nil && *p.OutputByteLimit > 0 {
		limit = *p.OutputByteLimit
	}
	t := &terminal{cmd: cmd, limit: limit, done: make(chan struct{})}
	cmd.Stdout, cmd.Stderr = t, t
	if err := cmd.Start(); err != nil {
		return nil, Errorf("%v", err)
	}
	go func() {
		err := cmd.Wait()
		t.mu.Lock()
		var ee *exec.ExitError
		switch {
		case err == nil:
			code := 0
			t.exitCode = &code
		case errors.As(err, &ee):
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				s := ws.Signal().String()
				t.signal = &s
			} else {
				code := ee.ExitCode()
				t.exitCode = &code
			}
		}
		t.mu.Unlock()
		close(t.done)
	}()
	tid := newUUID()
	ts.mu.Lock()
	ts.m[tid] = t
	ts.mu.Unlock()
	return map[string]any{"terminalId": tid}, nil
}

func (ts *terminals) get(params json.RawMessage) (*terminal, string, *RPCError) {
	var p struct {
		TerminalID string `json:"terminalId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil, "", &RPCError{Code: CodeInvalidParams, Message: "Invalid params"}
	}
	ts.mu.Lock()
	t := ts.m[p.TerminalID]
	ts.mu.Unlock()
	if t == nil {
		return nil, p.TerminalID, Errorf("unknown terminal %s", p.TerminalID)
	}
	return t, p.TerminalID, nil
}

func (ts *terminals) output(params json.RawMessage) (any, *RPCError) {
	t, _, rerr := ts.get(params)
	if rerr != nil {
		return nil, rerr
	}
	t.mu.Lock()
	out := map[string]any{"output": string(t.buf), "truncated": t.cut}
	t.mu.Unlock()
	if st := t.exitStatus(); st != nil {
		out["exitStatus"] = st
	}
	return out, nil
}

func (ts *terminals) waitForExit(ctx context.Context, params json.RawMessage) (any, *RPCError) {
	t, _, rerr := ts.get(params)
	if rerr != nil {
		return nil, rerr
	}
	select {
	case <-t.done:
	case <-ctx.Done():
		return nil, &RPCError{Code: CodeCancelled, Message: "Request cancelled"}
	}
	return t.exitStatus(), nil
}

func (t *terminal) killGroup() {
	if t.cmd.Process != nil {
		_ = syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL)
	}
}

func (ts *terminals) kill(params json.RawMessage) (any, *RPCError) {
	t, _, rerr := ts.get(params)
	if rerr != nil {
		return nil, rerr
	}
	t.killGroup()
	return map[string]any{}, nil
}

func (ts *terminals) release(params json.RawMessage) (any, *RPCError) {
	t, id, rerr := ts.get(params)
	if rerr != nil {
		return nil, rerr
	}
	t.killGroup()
	ts.mu.Lock()
	delete(ts.m, id)
	ts.mu.Unlock()
	return map[string]any{}, nil
}

func (ts *terminals) releaseAll() {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	for id, t := range ts.m {
		t.killGroup()
		delete(ts.m, id)
	}
}
