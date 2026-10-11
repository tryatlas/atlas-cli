package acp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
)

// JSON-RPC 2.0 over newline-delimited stdio (ADR-0010). One reader puts
// notifications and responses, in wire order, on one unbounded serial queue,
// so a response is seen only after the notifications that preceded it.
// Agent→client requests are dispatched off that queue onto their own
// goroutines. Nothing here logs a message body.

// JSON-RPC and ACP error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
	CodeAuthRequired   = -32000
	CodeNotFound       = -32002 // ACP "Resource not found"
	CodeCancelled      = -32800
)

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if d := e.Details(); d != "" {
		return fmt.Sprintf("%s (%d): %s", e.Message, e.Code, d)
	}
	return fmt.Sprintf("%s (%d)", e.Message, e.Code)
}

// Details is data.details when the agent sent one.
func (e *RPCError) Details() string {
	var d struct {
		Details string `json:"details"`
	}
	if json.Unmarshal(e.Data, &d) == nil {
		return d.Details
	}
	return ""
}

// AsRPCError unwraps an *RPCError.
func AsRPCError(err error) (*RPCError, bool) {
	var e *RPCError
	ok := errors.As(err, &e)
	return e, ok
}

// Errorf builds an internal error for a client method.
func Errorf(format string, a ...any) *RPCError {
	d, _ := json.Marshal(map[string]string{"details": fmt.Sprintf(format, a...)})
	return &RPCError{Code: CodeInternalError, Message: "Internal error", Data: d}
}

// NotFound is ACP's "Resource not found" for a path that does not exist.
func NotFound(path string) *RPCError {
	d, _ := json.Marshal(map[string]string{"uri": "file://" + path})
	return &RPCError{Code: CodeNotFound, Message: "Resource not found", Data: d}
}

// Handler serves what the agent sends. Notify runs on the serial queue and
// must not block; Request runs on its own goroutine.
type Handler interface {
	Notify(method string, params json.RawMessage)
	Request(ctx context.Context, method string, params json.RawMessage) (any, *RPCError)
}

type envelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type reply struct {
	result json.RawMessage
	err    *RPCError
}

// Conn is one JSON-RPC connection.
type Conn struct {
	w       io.Writer
	wmu     sync.Mutex
	handler Handler
	nextID  atomic.Int64

	mu       sync.Mutex
	pending  map[string]chan reply
	inflight map[string]context.CancelFunc
	closed   bool
	err      error
	done     chan struct{}

	qmu   sync.Mutex
	qcond *sync.Cond
	queue []func()
	qdone bool
}

// NewConn starts reading r; writes go to w.
func NewConn(r io.Reader, w io.Writer, h Handler) *Conn {
	c := &Conn{w: w, handler: h, pending: map[string]chan reply{}, inflight: map[string]context.CancelFunc{}, done: make(chan struct{})}
	c.qcond = sync.NewCond(&c.qmu)
	go c.process()
	go c.read(r)
	return c
}

// Done closes when the peer's output ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Conn) enqueue(f func()) {
	c.qmu.Lock()
	c.queue = append(c.queue, f)
	c.qcond.Signal()
	c.qmu.Unlock()
}

func (c *Conn) process() {
	for {
		c.qmu.Lock()
		for len(c.queue) == 0 && !c.qdone {
			c.qcond.Wait()
		}
		if len(c.queue) == 0 && c.qdone {
			c.qmu.Unlock()
			return
		}
		f := c.queue[0]
		c.queue[0] = nil
		c.queue = c.queue[1:]
		c.qmu.Unlock()
		f()
	}
}

func idKey(raw json.RawMessage) string { return string(bytes.TrimSpace(raw)) }

func (c *Conn) read(r io.Reader) {
	br := bufio.NewReaderSize(r, 1<<20)
	var cause error
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			c.dispatch(line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				cause = err
			}
			break
		}
	}
	if cause == nil {
		cause = io.EOF
	}
	// After everything already read has been processed, fail the waiters.
	c.enqueue(func() { c.shutdown(cause) })
	c.qmu.Lock()
	c.qdone = true
	c.qcond.Signal()
	c.qmu.Unlock()
}

func (c *Conn) dispatch(line []byte) {
	trace("in", line)
	var m envelope
	if json.Unmarshal(line, &m) != nil {
		return // not JSON-RPC: dropped, never logged
	}
	switch {
	case m.Method == "$/cancel_request" && len(m.ID) == 0:
		var p struct {
			RequestID json.RawMessage `json:"requestId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		c.mu.Lock()
		cancel := c.inflight[idKey(p.RequestID)]
		c.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	case m.Method != "" && len(m.ID) > 0:
		id := append(json.RawMessage(nil), m.ID...)
		ctx, cancel := context.WithCancel(context.Background())
		c.mu.Lock()
		c.inflight[idKey(id)] = cancel
		c.mu.Unlock()
		method, params := m.Method, m.Params
		c.enqueue(func() {
			go func() {
				defer func() {
					c.mu.Lock()
					delete(c.inflight, idKey(id))
					c.mu.Unlock()
					cancel()
				}()
				res, rerr := c.handler.Request(ctx, method, params)
				c.respond(id, res, rerr)
			}()
		})
	case m.Method != "":
		method, params := m.Method, m.Params
		c.enqueue(func() { c.handler.Notify(method, params) })
	case len(m.ID) > 0:
		key := idKey(m.ID)
		r := reply{result: m.Result, err: m.Error}
		c.enqueue(func() {
			c.mu.Lock()
			ch := c.pending[key]
			delete(c.pending, key)
			c.mu.Unlock()
			if ch != nil {
				ch <- r
			}
		})
	}
}

func (c *Conn) shutdown(cause error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.err = cause
	for k, ch := range c.pending {
		close(ch)
		delete(c.pending, k)
	}
	for _, cancel := range c.inflight {
		cancel()
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Conn) write(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	trace("out", b)
	b = append(b, '\n')
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.w.Write(b)
	return err
}

func (c *Conn) respond(id json.RawMessage, res any, rerr *RPCError) {
	m := map[string]any{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		m["error"] = rerr
	} else {
		if res == nil {
			res = struct{}{}
		}
		m["result"] = res
	}
	_ = c.write(m)
}

// ErrClosed is a call on a connection whose peer went away.
var ErrClosed = errors.New("the agent closed the connection")

// Call sends a request and decodes the result into out (may be nil). A
// cancelled ctx sends $/cancel_request for the abandoned id.
func (c *Conn) Call(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	ch := make(chan reply, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	c.pending[key] = ch
	c.mu.Unlock()
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	if err := c.write(msg); err != nil {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		return err
	}
	select {
	case r, ok := <-ch:
		if !ok {
			return ErrClosed
		}
		if r.err != nil {
			return r.err
		}
		if out != nil && len(r.result) > 0 && string(r.result) != "null" {
			return json.Unmarshal(r.result, out)
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
		_ = c.Notify("$/cancel_request", map[string]any{"requestId": id})
		return ctx.Err()
	}
}

// Notify sends a notification.
func (c *Conn) Notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	return c.write(msg)
}
