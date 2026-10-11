package acp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type allowHost struct{ answer string }

func (allowHost) Update(string, Update) {}
func (h allowHost) Permission(_ context.Context, req PermissionRequest) (string, bool) {
	for _, o := range req.Options {
		if o.Kind == h.answer {
			return o.OptionID, true
		}
	}
	return "", false
}
func (allowHost) Elicit(_ context.Context, e Elicitation) ElicitationAnswer {
	return ElicitationAnswer{Action: "accept", Content: map[string]any{"name": e.Message}}
}
func (allowHost) ElicitationComplete(string) {}

func testAgent(t *testing.T, answer string) (*handler, string) {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	a := &Agent{host: allowHost{answer: answer}, roots: map[string][]string{"s": {root}}, cancels: map[string]*cancelState{}}
	a.terms = newTerminals(a)
	return &handler{a: a}, root
}

func call(t *testing.T, h *handler, method string, params map[string]any) (map[string]any, *RPCError) {
	t.Helper()
	b, _ := json.Marshal(params)
	res, rerr := h.Request(context.Background(), method, b)
	if rerr != nil {
		return nil, rerr
	}
	out := map[string]any{}
	rb, _ := json.Marshal(res)
	_ = json.Unmarshal(rb, &out)
	return out, nil
}

func TestReadTextFile(t *testing.T) {
	h, root := testAgent(t, "allow_once")
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("1\n2\n3\n4\n"), 0o644)
	if r, e := call(t, h, "fs/read_text_file", map[string]any{"sessionId": "s", "path": filepath.Join(root, "a.txt"), "line": 2, "limit": 2}); e != nil || r["content"] != "2\n3\n" {
		t.Fatalf("%v %v", r, e)
	}
	if _, e := call(t, h, "fs/read_text_file", map[string]any{"sessionId": "s", "path": filepath.Join(root, "missing.md")}); e == nil || e.Code != CodeNotFound || e.Message != "Resource not found" {
		t.Fatalf("a missing file is ACP's Resource not found: %v", e)
	}
	if _, e := call(t, h, "fs/read_text_file", map[string]any{"sessionId": "s", "path": root}); e == nil || !strings.Contains(e.Details(), "directory") {
		t.Fatalf("%v", e)
	}
	outside := filepath.Join(filepath.Dir(root), "elsewhere.txt")
	if _, e := call(t, h, "fs/read_text_file", map[string]any{"sessionId": "s", "path": outside}); e == nil || !strings.Contains(e.Details(), "outside") {
		t.Fatalf("%v", e)
	}
	// A symlink inside the root that points out is refused.
	_ = os.Symlink(filepath.Dir(root), filepath.Join(root, "up"))
	if _, e := call(t, h, "fs/read_text_file", map[string]any{"sessionId": "s", "path": filepath.Join(root, "up", "x", "y.txt")}); e == nil {
		t.Fatal("symlink escape accepted")
	}
}

func TestWriteTextFileCreatesParents(t *testing.T) {
	h, root := testAgent(t, "allow_once")
	p := filepath.Join(root, "new", "dir", "f.txt")
	if _, e := call(t, h, "fs/write_text_file", map[string]any{"sessionId": "s", "path": p, "content": "x"}); e != nil {
		t.Fatal(e)
	}
	if b, _ := os.ReadFile(p); string(b) != "x" {
		t.Fatal(string(b))
	}
}

func TestTerminalLifecycle(t *testing.T) {
	h, root := testAgent(t, "allow_once")
	r, e := call(t, h, "terminal/create", map[string]any{"sessionId": "s", "command": "sh", "args": []string{"-c", "echo hi; echo $DYLD_X"},
		"env": []map[string]string{{"name": "DYLD_X", "value": "bad"}, {"name": "OK", "value": "1"}}, "cwd": root, "outputByteLimit": 100})
	if e != nil {
		t.Fatal(e)
	}
	id := r["terminalId"].(string)
	if len(id) != 36 {
		t.Fatalf("uuid terminal id %q", id)
	}
	w, e := call(t, h, "terminal/wait_for_exit", map[string]any{"sessionId": "s", "terminalId": id})
	if e != nil || w["exitCode"] != float64(0) {
		t.Fatalf("%v %v", w, e)
	}
	o, _ := call(t, h, "terminal/output", map[string]any{"sessionId": "s", "terminalId": id})
	if o["output"] != "hi\n\n" || o["truncated"] != false || o["exitStatus"] == nil {
		t.Fatalf("DYLD_* dropped: %v", o)
	}
	if _, e := call(t, h, "terminal/release", map[string]any{"sessionId": "s", "terminalId": id}); e != nil {
		t.Fatal(e)
	}
	if _, e := call(t, h, "terminal/output", map[string]any{"sessionId": "s", "terminalId": id}); e == nil {
		t.Fatal("released terminal still known")
	}
	h2, root2 := testAgent(t, "reject_once")
	if _, e := call(t, h2, "terminal/create", map[string]any{"sessionId": "s", "command": "true", "cwd": root2}); e == nil {
		t.Fatal("ran without the user's allow")
	}
}

func TestTerminalOutputLimitKeepsTheTail(t *testing.T) {
	h, root := testAgent(t, "allow_once")
	r, _ := call(t, h, "terminal/create", map[string]any{"sessionId": "s", "command": "sh", "args": []string{"-c", "printf 0123456789"}, "cwd": root, "outputByteLimit": 4})
	id := r["terminalId"].(string)
	_, _ = call(t, h, "terminal/wait_for_exit", map[string]any{"sessionId": "s", "terminalId": id})
	o, _ := call(t, h, "terminal/output", map[string]any{"sessionId": "s", "terminalId": id})
	if o["output"] != "6789" || o["truncated"] != true {
		t.Fatalf("%v", o)
	}
}

func TestPermissionAndElicitation(t *testing.T) {
	h, _ := testAgent(t, "allow_always")
	r, _ := call(t, h, "session/request_permission", map[string]any{"sessionId": "s", "toolCall": map[string]any{"toolCallId": "t"},
		"options": []map[string]string{{"optionId": "a", "name": "Once", "kind": "allow_once"}, {"optionId": "b", "name": "Always", "kind": "allow_always"}}})
	if jsonOf(r) != `{"outcome":{"optionId":"b","outcome":"selected"}}` {
		t.Fatal(jsonOf(r))
	}
	h2, _ := testAgent(t, "none")
	r, _ = call(t, h2, "session/request_permission", map[string]any{"sessionId": "s", "options": []map[string]string{{"optionId": "a", "kind": "allow_once"}}})
	if jsonOf(r) != `{"outcome":{"outcome":"cancelled"}}` {
		t.Fatal(jsonOf(r))
	}
	for _, m := range []string{"session/request_elicitation", "elicitation/create"} {
		r, _ = call(t, h, m, map[string]any{"sessionId": "s", "mode": "form", "message": "Name?", "requestedSchema": map[string]any{"type": "object"}})
		if jsonOf(r) != `{"action":"accept","content":{"name":"Name?"}}` {
			t.Fatal(m, jsonOf(r))
		}
	}
	r, _ = call(t, h, "session/request_elicitation", map[string]any{"sessionId": "s", "mode": "url", "message": "x", "url": "file:///etc/passwd"})
	if jsonOf(r) != `{"action":"decline"}` {
		t.Fatal("only http(s) URLs with a host")
	}
}
