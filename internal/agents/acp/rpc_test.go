package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// peer is the agent side of a pipe pair, driven line by line.
type peer struct {
	in  *bufio.Reader // what the client wrote
	out io.WriteCloser
}

func (p *peer) send(t *testing.T, s string) {
	t.Helper()
	if _, err := io.WriteString(p.out, s+"\n"); err != nil {
		t.Fatal(err)
	}
}

func (p *peer) recv(t *testing.T) map[string]any {
	t.Helper()
	line, err := p.in.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("not JSON: %q", line)
	}
	return m
}

type recHandler struct {
	mu      sync.Mutex
	notes   []string
	request func(ctx context.Context, method string, params json.RawMessage) (any, *RPCError)
}

func (h *recHandler) Notify(method string, params json.RawMessage) {
	h.mu.Lock()
	h.notes = append(h.notes, method+" "+string(params))
	h.mu.Unlock()
}

func (h *recHandler) Request(ctx context.Context, method string, params json.RawMessage) (any, *RPCError) {
	if h.request != nil {
		return h.request(ctx, method, params)
	}
	return nil, &RPCError{Code: CodeMethodNotFound, Message: "Method not found"}
}

func pair(t *testing.T, h Handler) (*Conn, *peer) {
	t.Helper()
	cr, pw := io.Pipe() // agent → client
	pr, cw := io.Pipe() // client → agent
	c := NewConn(cr, cw, h)
	t.Cleanup(func() { _ = pw.Close(); _ = cw.Close() })
	return c, &peer{in: bufio.NewReader(pr), out: pw}
}

func TestResponseAfterPrecedingNotifications(t *testing.T) {
	h := &recHandler{}
	c, p := pair(t, h)
	done := make(chan error, 1)
	var out struct {
		StopReason string `json:"stopReason"`
	}
	go func() { done <- c.Call(context.Background(), "session/prompt", map[string]any{"sessionId": "s"}, &out) }()
	req := p.recv(t)
	if req["method"] != "session/prompt" || req["jsonrpc"] != "2.0" {
		t.Fatalf("%v", req)
	}
	for i := range 50 {
		p.send(t, `{"jsonrpc":"2.0","method":"session/update","params":{"n":`+itoa(i)+`}}`)
	}
	p.send(t, `{"jsonrpc":"2.0","id":`+jsonOf(req["id"])+`,"result":{"stopReason":"end_turn"}}`)
	if err := <-done; err != nil || out.StopReason != "end_turn" {
		t.Fatalf("%v %+v", err, out)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.notes) != 50 || !strings.HasSuffix(h.notes[49], `{"n":49}`) {
		t.Fatalf("every update handled before the result: %d", len(h.notes))
	}
}

func TestErrorsAndHugeLines(t *testing.T) {
	c, p := pair(t, &recHandler{})
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "session/new", nil, nil) }()
	req := p.recv(t)
	big := strings.Repeat("x", 12<<20) // past the SDK's 10 MiB line limit
	p.send(t, `{"jsonrpc":"2.0","method":"session/update","params":{"big":"`+big+`"}}`)
	p.send(t, `{"jsonrpc":"2.0","id":`+jsonOf(req["id"])+`,"error":{"code":-32000,"message":"Authentication required"}}`)
	err := <-done
	e, ok := AsRPCError(err)
	if !ok || e.Code != CodeAuthRequired {
		t.Fatalf("%v", err)
	}
}

func TestAgentRequestsAreServedConcurrently(t *testing.T) {
	release := make(chan struct{})
	h := &recHandler{request: func(_ context.Context, method string, _ json.RawMessage) (any, *RPCError) {
		if method == "session/request_permission" {
			<-release // waits for the user
			return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
		}
		if method == "fs/read_text_file" {
			return map[string]any{"content": "hi"}, nil
		}
		return nil, &RPCError{Code: CodeMethodNotFound, Message: "Method not found"}
	}}
	_, p := pair(t, h)
	p.send(t, `{"jsonrpc":"2.0","id":"a","method":"session/request_permission","params":{}}`)
	p.send(t, `{"jsonrpc":"2.0","id":7,"method":"fs/read_text_file","params":{}}`)
	first := p.recv(t)
	if first["id"] != 7.0 || first["result"].(map[string]any)["content"] != "hi" {
		t.Fatalf("a blocked permission prompt must not hold up other requests: %v", first)
	}
	close(release)
	second := p.recv(t)
	if second["id"] != "a" {
		t.Fatalf("%v", second)
	}
	p.send(t, `{"jsonrpc":"2.0","id":8,"method":"nope","params":{}}`)
	if m := p.recv(t); m["error"].(map[string]any)["code"] != float64(CodeMethodNotFound) {
		t.Fatalf("%v", m)
	}
}

func TestCancelledCallSendsCancelRequest(t *testing.T) {
	c, p := pair(t, &recHandler{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Call(ctx, "session/new", nil, nil) }()
	req := p.recv(t)
	cancel()
	m := p.recv(t) // an io.Pipe is unbuffered: read before waiting on Call
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if m["method"] != "$/cancel_request" || jsonOf(m["params"].(map[string]any)["requestId"]) != jsonOf(req["id"]) {
		t.Fatalf("%v", m)
	}
}

func TestAgentCancelRequestCancelsTheHandler(t *testing.T) {
	cancelled := make(chan struct{})
	h := &recHandler{request: func(ctx context.Context, _ string, _ json.RawMessage) (any, *RPCError) {
		<-ctx.Done()
		close(cancelled)
		return nil, &RPCError{Code: CodeCancelled, Message: "Request cancelled"}
	}}
	_, p := pair(t, h)
	p.send(t, `{"jsonrpc":"2.0","id":3,"method":"session/request_permission","params":{}}`)
	time.Sleep(20 * time.Millisecond)
	p.send(t, `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":3}}`)
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("handler not cancelled")
	}
}

func TestPeerExitFailsPendingCalls(t *testing.T) {
	c, p := pair(t, &recHandler{})
	done := make(chan error, 1)
	go func() { done <- c.Call(context.Background(), "initialize", nil, nil) }()
	p.recv(t)
	_ = p.out.Close()
	if err := <-done; !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	<-c.Done()
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

func itoa(n int) string { return jsonOf(n) }
