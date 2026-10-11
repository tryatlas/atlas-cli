package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/memory/record"
	"github.com/tryatlas/atlas-cli/internal/project"
)

// client drives a bridge over pipes like an agent would.
type client struct {
	t   *testing.T
	in  *io.PipeWriter
	out *bufio.Scanner
	id  int
}

func startBridge(t *testing.T, f Forwarder) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan struct{})
	go func() {
		_ = RunBridge(context.Background(), inR, outW, f)
		_ = outW.Close()
		close(done)
	}()
	t.Cleanup(func() { _ = inW.Close(); <-done })
	sc := bufio.NewScanner(outR)
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	return &client{t: t, in: inW, out: sc}
}

func (c *client) send(method string, params any) map[string]any {
	c.t.Helper()
	c.id++
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if _, err := c.in.Write(append(msg, '\n')); err != nil {
		c.t.Fatal(err)
	}
	if !c.out.Scan() {
		c.t.Fatalf("no answer to %s", method)
	}
	var resp map[string]any
	if err := json.Unmarshal(c.out.Bytes(), &resp); err != nil {
		c.t.Fatalf("%s: %v (%s)", method, err, c.out.Text())
	}
	return resp
}

func (c *client) notify(method string) {
	msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	_, _ = c.in.Write(append(msg, '\n'))
}

// tool calls a tool and returns the decoded JSON object or the error text.
func (c *client) tool(name string, args map[string]any) (map[string]any, string) {
	c.t.Helper()
	resp := c.send("tools/call", map[string]any{"name": name, "arguments": args})
	res, _ := resp["result"].(map[string]any)
	if res == nil {
		c.t.Fatalf("%s: %v", name, resp)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if res["isError"] == true {
		return nil, text
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		c.t.Fatalf("%s: not a JSON object: %s", name, text)
	}
	return m, ""
}

func repo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "repo")
	_ = os.MkdirAll(filepath.Join(dir, "src"), 0o755)
	cmd := exec.Command("git", "-C", dir, "init", "-q")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	_ = os.WriteFile(filepath.Join(dir, "src", "ttl.go"), []byte("package src\n\n// TTL is fifteen minutes.\nconst TTL = 15\n"), 0o644)
	c, _ := project.Canonical(dir)
	return c
}

func newServer() *Server {
	s := New("test")
	clock := int64(1_760_000_000_000)
	s.Now = func() time.Time { clock += 1000; return time.UnixMilli(clock) }
	return s
}

func handshake(c *client) map[string]any {
	resp := c.send("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "test", "version": "0"}})
	c.notify("notifications/initialized")
	return resp["result"].(map[string]any)
}

func TestToolsOverTheBridge(t *testing.T) {
	dir := repo(t)
	srv := newServer()
	defer srv.Close()
	a := startBridge(t, NewLocalForwarder(srv, Grant{SessionID: "s-a", Agent: "claude-code", Cwd: dir}))

	init := handshake(a)
	if init["protocolVersion"] != "2025-06-18" || init["instructions"] != Instructions() ||
		!strings.HasPrefix(Instructions(), "Atlas shared memory for this repository") || strings.Count(Instructions(), "\n") != 7 {
		t.Fatalf("initialize: %v", init)
	}
	list := a.send("tools/list", map[string]any{})["result"].(map[string]any)
	if n := len(list["tools"].([]any)); n != 10 || list["ttlMs"] != float64(3600000) || list["cacheScope"] != "private" {
		t.Fatalf("tools/list: %d tools, %v %v", n, list["ttlMs"], list["cacheScope"])
	}

	out, errText := a.tool("memory_remember", map[string]any{"kind": "decision", "key": "ttl", "content": "Access tokens live 15 minutes",
		"evidence": []any{map[string]any{"path": "src/ttl.go", "lines": "3-4", "symbol": "TTL"}}})
	if errText != "" || out["outcome"] != "inserted" {
		t.Fatalf("remember: %v %s", out, errText)
	}
	entry := out["entry"].(map[string]any)
	if entry["validity"] != "valid" || entry["by"] != "claude-code" || entry["sources"].([]any)[0] != "atlas-session:claude-code/s-a" {
		t.Fatalf("entry: %v", entry)
	}
	if _, errText := a.tool("memory_remember", map[string]any{"kind": "plan", "content": "x"}); errText !=
		"not remembered: `plan` is working memory, captured from the session automatically; remember records only decision, fact, failure, architecture or preference" {
		t.Fatalf("plan: %s", errText)
	}
	if _, errText := a.tool("memory_remember", map[string]any{"kind": "fact", "content": "  "}); errText != "not remembered: nothing to remember: content is empty" {
		t.Fatalf("empty: %s", errText)
	}
	if _, errText := a.tool("memory_remember", map[string]any{"kind": "fact", "content": "x",
		"evidence": []any{map[string]any{"path": "src/nope.go", "lines": "1"}}}); errText != "not remembered: `src/nope.go` does not exist" {
		t.Fatalf("missing file: %s", errText)
	}
	if _, errText := a.tool("memory_search", map[string]any{"query": "x", "kinds": []any{"bogus"}}); errText !=
		"unknown kind `bogus`; one of plan, file_changed, decision, fact, failure, architecture, preference" {
		t.Fatalf("unknown kind: %s", errText)
	}

	// A second session (another agent) sees it, cannot overwrite it blind, and
	// gets it through changes and the briefing.
	b := startBridge(t, NewLocalForwarder(srv, Grant{SessionID: "s-b", Agent: "codex", Cwd: dir}))
	handshake(b)
	brief, _ := b.tool("memory_briefing", map[string]any{})
	idx := brief["index"].(map[string]any)["decision"].([]any)
	if len(idx) != 1 || idx[0].(map[string]any)["content"] != "Access tokens live 15 minutes" {
		t.Fatalf("briefing index: %v", brief["index"])
	}
	if _, errText := b.tool("memory_remember", map[string]any{"kind": "decision", "key": "ttl", "content": "Access tokens live 30 minutes"}); !strings.HasPrefix(errText,
		`not remembered: memory 1 is at revision 1 (by claude-code): "Access tokens live 15 minutes". Read both versions`) {
		t.Fatalf("conflict: %s", errText)
	}
	out, _ = b.tool("memory_remember", map[string]any{"kind": "decision", "key": "ttl", "content": "Access tokens live 30 minutes", "expected_revision": 1})
	if out["outcome"] != "replaced" {
		t.Fatalf("replace: %v", out)
	}
	changes, _ := a.tool("memory_changes", map[string]any{})
	if es := changes["entries"].([]any); len(es) != 1 || es[0].(map[string]any)["content"] != "Access tokens live 30 minutes" {
		t.Fatalf("changes: %v", changes)
	}
	again, _ := a.tool("memory_changes", map[string]any{})
	if len(again["entries"].([]any)) != 0 {
		t.Fatalf("clock did not move: %v", again)
	}

	search, _ := b.tool("memory_search", map[string]any{"query": "access tokens"})
	hits := search["entries"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["why"].(map[string]any)["bm25"] != float64(1) || len(search["documents"].([]any)) != 0 {
		t.Fatalf("search: %v", search)
	}
	got, _ := b.tool("memory_get", map[string]any{"id": 1})
	prov := got["entry"].(map[string]any)["provenance"].([]any)
	if len(prov) != 2 {
		t.Fatalf("provenance: %v", prov)
	}
	hist, _ := b.tool("memory_history", map[string]any{"id": 1})
	if revs := hist["revisions"].([]any); len(revs) != 2 || revs[0].(map[string]any)["op"] != "insert" || revs[1].(map[string]any)["op"] != "replace" {
		t.Fatalf("history: %v", hist)
	}
	if _, errText := b.tool("memory_get", map[string]any{"id": 99}); errText != "no memory entry 99" {
		t.Fatalf("get missing: %s", errText)
	}
	fb, _ := b.tool("memory_feedback", map[string]any{"id": 1, "verdict": "useful"})
	if fb["entry"].(map[string]any)["uses"] != float64(1) {
		t.Fatalf("feedback: %v", fb)
	}
	why, _ := b.tool("memory_why", map[string]any{"path": "src/ttl.go"})
	if why["note"] != "Session capture is off here; these are the memories that cite this path." || why["target"].(map[string]any)["path"] != "src/ttl.go" {
		t.Fatalf("why: %v", why)
	}
	if _, errText := b.tool("memory_why", map[string]any{"commit": "xyz"}); errText != "`xyz` is not a commit sha (7 to 40 hex characters)" {
		t.Fatalf("why commit: %s", errText)
	}
	if _, errText := b.tool("memory_why", map[string]any{}); errText != "give exactly one of path or commit" {
		t.Fatalf("why none: %s", errText)
	}
	fg, _ := b.tool("memory_forget", map[string]any{"id": 1})
	if fg["forgotten"] != true {
		t.Fatalf("forget: %v", fg)
	}
	ls, _ := a.tool("memory_list", map[string]any{})
	if len(ls["entries"].([]any)) != 0 {
		t.Fatalf("list after forget: %v", ls)
	}

	// The record: sessions logged, chain intact, project.json written.
	st, _ := record.Open(dir, false)
	defer st.Close()
	if rep, _ := st.VerifyChain(); !rep.OK() || rep.Revisions != 3 { // insert, replace, forget (useful on an active entry writes none)
		t.Fatalf("chain: %+v", rep)
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".atlas", "project.json")); err != nil || !strings.HasPrefix(string(b), `{"projectId":"`) {
		t.Fatalf("project.json: %s %v", b, err)
	}
}

func TestSharingGateOff(t *testing.T) {
	dir := repo(t)
	_ = project.SetMemorySharing(dir, false)
	srv := newServer()
	defer srv.Close()
	c := startBridge(t, NewLocalForwarder(srv, Grant{SessionID: "s", Agent: "codex", Cwd: dir}))
	handshake(c)
	out, _ := c.tool("memory_briefing", map[string]any{})
	if out["note"] != "shared memory is switched off for this project" || len(out["entries"].([]any)) != 0 {
		t.Fatalf("read: %v", out)
	}
	if _, errText := c.tool("memory_remember", map[string]any{"kind": "fact", "content": "x"}); errText != "shared memory is switched off for this project" {
		t.Fatalf("write: %s", errText)
	}
	if _, err := os.Stat(record.Path(dir)); err == nil {
		t.Fatal("record created while sharing is off")
	}
}

func TestBridgeReplaysInitializeAfterARestart(t *testing.T) {
	dir := repo(t)
	tokens := NewTokens()
	srv := newServer()
	defer srv.Close()
	h := NewHTTPHandler(srv, tokens.Auth)
	ts := httptest.NewServer(h)
	defer ts.Close()
	f, err := NewTCPForwarder(ts.URL, tokens.Mint(Grant{SessionID: "s-x", Agent: "gemini", Cwd: dir}))
	if err != nil {
		t.Fatal(err)
	}
	c := startBridge(t, f)
	handshake(c)
	if out, e := c.tool("memory_remember", map[string]any{"kind": "fact", "content": "Over loopback"}); e != "" || out["outcome"] != "inserted" {
		t.Fatalf("%v %s", out, e)
	}
	// The listener forgets every MCP session (a restart); the bridge replays initialize.
	h.mu.Lock()
	h.sessions = map[string]*mcpSession{}
	h.mu.Unlock()
	if out, e := c.tool("memory_search", map[string]any{"query": "loopback"}); e != "" || len(out["entries"].([]any)) != 1 {
		t.Fatalf("after replay: %v %s", out, e)
	}
}

func TestBearerTokensAndLoopbackChecks(t *testing.T) {
	tokens := NewTokens()
	srv := newServer()
	defer srv.Close()
	ts := httptest.NewServer(NewHTTPHandler(srv, tokens.Auth))
	defer ts.Close()
	resp, err := http.Post(ts.URL, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	tok := tokens.Mint(Grant{SessionID: "s", Agent: "codex", Cwd: t.TempDir()})
	if len(tok) != 64 {
		t.Fatalf("token %q", tok)
	}
	f, err := NewTCPForwarder(ts.URL, tok)
	if err != nil {
		t.Fatal(err)
	}
	c := startBridge(t, f)
	if init := handshake(c); init["serverInfo"].(map[string]any)["name"] != Name {
		t.Fatalf("%v", init)
	}
	for _, bad := range []string{"https://127.0.0.1:1/mcp", "http://example.com/mcp", "http://127.0.0.1:1@example.com/mcp", "http://u:p@127.0.0.1/mcp"} {
		if CheckLoopbackURL(bad) == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	for _, good := range []string{"http://127.0.0.1:5/mcp", "http://localhost:5/mcp", "http://[::1]:5/mcp"} {
		if err := CheckLoopbackURL(good); err != nil {
			t.Errorf("refused %s: %v", good, err)
		}
	}
}

func TestBridgeAnswersWhenServerIsUnreachable(t *testing.T) {
	f, _ := NewTCPForwarder("http://127.0.0.1:1/mcp", "x")
	c := startBridge(t, f)
	resp := c.send("ping", map[string]any{})
	if e, ok := resp["error"].(map[string]any); !ok || !strings.Contains(fmt.Sprint(e["message"]), "unreachable") {
		t.Fatalf("%v", resp)
	}
}

func TestTokenBoundAfterInitializeCarriesTheSession(t *testing.T) {
	dir := repo(t)
	tokens := NewTokens()
	srv := newServer()
	defer srv.Close()
	ts := httptest.NewServer(NewHTTPHandler(srv, tokens.Auth))
	defer ts.Close()
	tok := tokens.Mint(Grant{Agent: "fake", Cwd: dir}) // minted before session/new answers
	f, _ := NewTCPForwarder(ts.URL, tok)
	c := startBridge(t, f)
	handshake(c) // the agent connects MCP while session/new is in flight
	tokens.Bind(tok, Grant{SessionID: "acp-1", Agent: "fake", Cwd: dir})
	if out, e := c.tool("memory_remember", map[string]any{"kind": "fact", "content": "Bound later"}); e != "" || out["outcome"] != "inserted" {
		t.Fatalf("%v %s", out, e)
	}
	st, _ := record.Open(dir, false)
	defer st.Close()
	es, _ := st.EntriesBySessions(context.Background(), []string{"acp-1"}, 10)
	if len(es) != 1 {
		t.Fatalf("the write carries the bound session id: %d entries", len(es))
	}
}

func TestBridgeAnswersARequestBeforeInitialize(t *testing.T) {
	srv := New("test")
	defer srv.Close()
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}` + "\n")
	var out bytes.Buffer
	if err := RunBridge(context.Background(), in, &out, NewLocalForwarder(srv, Grant{SessionID: "s", Agent: "agy", Cwd: t.TempDir()})); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"id":1`) || !strings.Contains(lines[0], "-32601") || !strings.Contains(lines[1], `"protocolVersion"`) {
		t.Fatalf("bridge output:\n%s", out.String())
	}
}
