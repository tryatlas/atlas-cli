package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

const sessionUUID = "4b7a3c2e-1f0d-4e5a-9b8c-7d6e5f4a3b2c"

func sse(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var m map[string]any
		_ = json.Unmarshal([]byte(e), &m)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", m["type"], e)
	}
	return b.String()
}

// replies is the fake upstream's script, one per call.
type upstream struct {
	mu       sync.Mutex
	replies  []func(w http.ResponseWriter)
	gotAuth  []string
	gotPaths []string
}

func (u *upstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	u.mu.Lock()
	u.gotAuth = append(u.gotAuth, r.Header.Get("x-api-key")+"|"+r.Header.Get("Authorization")+"|"+r.Header.Get("anthropic-beta"))
	u.gotPaths = append(u.gotPaths, r.URL.String())
	reply := u.replies[0]
	u.replies = u.replies[1:]
	u.mu.Unlock()
	reply(w)
}

func sseReply(body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Upstream", "yes")
		_, _ = io.WriteString(w, body)
	}
}

func setup(t *testing.T) (*httptest.Server, *upstream, *sql.DB, string, chan struct{}) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "w")
	_ = os.MkdirAll(root, 0o755)
	_ = exec.Command("git", "-C", root, "init", "-q").Run()
	root, _ = filepath.EvalSymlinks(root)
	st, err := store.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.PutBinding(context.Background(), store.Binding{WorkspaceID: root, Root: root, Mode: store.ModeLocal, Enabled: true}, time.Now())
	up := &upstream{}
	us := httptest.NewServer(up)
	t.Cleanup(us.Close)
	u, _ := url.Parse(us.URL)
	capt := NewAnthropic(record.New(st, root), NewHooks(), root)
	done := make(chan struct{}, 16)
	px := httptest.NewServer(Handler("/proxy/anthropic", u, signalling{capt, done}))
	t.Cleanup(px.Close)
	return px, up, st.DB(), root, done
}

func post(t *testing.T, px *httptest.Server, done chan struct{}, body string) (string, http.Header) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/proxy/anthropic/v1/messages?beta=true", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "sk-ant-test")
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("exchange not observed")
	}
	return string(b), resp.Header
}

func reqBody(msgs ...string) string {
	return `{"model":"claude-sonnet-4-5","stream":true,"system":[{"type":"text","text":"You are Claude Code"}],` +
		`"tools":[{"name":"Write"}],"metadata":{"user_id":"user_abc_account_def_session_` + sessionUUID + `"},` +
		`"messages":[` + strings.Join(msgs, ",") + `]}`
}

const prompt1 = `{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"Create hello.txt"}]}`

func TestProxyRecordsAClaudeTurn(t *testing.T) {
	px, up, db, root, done := setup(t)
	resp1 := sse(
		`{"type":"message_start","message":{"id":"msg_01","model":"claude-sonnet-4-5","usage":{"input_tokens":100,"cache_read_input_tokens":2000,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Need a file."}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Writing it."}}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"Write","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"`+filepath.Join(root, "hello.txt")+`\","}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"content\":\"hi\\n\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
		`{"type":"message_stop"}`)
	resp2 := sse(
		`{"type":"message_start","message":{"id":"msg_02","model":"claude-sonnet-4-5","usage":{"input_tokens":10,"output_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done."}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`)
	up.replies = []func(http.ResponseWriter){sseReply(resp1), sseReply(resp2), sseReply(resp2)}

	got, hdr := post(t, px, done, reqBody(prompt1))
	if got != resp1 || hdr.Get("X-Upstream") != "yes" {
		t.Fatal("response bytes or headers were changed")
	}
	_ = os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi\n"), 0o644) // the agent writes the file
	assistant := `{"role":"assistant","content":[{"type":"text","text":"Writing it."},{"type":"tool_use","id":"toolu_1","name":"Write","input":{}}]}`
	toolResult := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"File created"}]}`
	post(t, px, done, reqBody(prompt1, assistant, toolResult))
	// A helper request without tools is not recorded.
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/proxy/anthropic/v1/messages", strings.NewReader(`{"model":"claude-haiku","messages":[{"role":"user","content":"title this"}]}`))
	r, _ := http.DefaultClient.Do(req)
	_, _ = io.ReadAll(r.Body)
	_ = r.Body.Close()
	<-done

	if up.gotAuth[0] != "sk-ant-test||oauth-2025-04-20" || !strings.HasSuffix(up.gotPaths[0], "/v1/messages?beta=true") {
		t.Fatalf("upstream got %v %v", up.gotAuth, up.gotPaths)
	}
	one := func(q string) string {
		var s sql.NullString
		if err := db.QueryRow(q).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s.String
	}
	if v := one(`SELECT native_session_id || '|' || source || '|' || agent || '|' || title || '|' || model FROM agent_session`); v != sessionUUID+"|acp|claude-code|Create hello.txt|claude-sonnet-4-5" {
		t.Fatal(v)
	}
	if v := one(`SELECT count(*) FROM agent_session`); v != "1" {
		t.Fatalf("%s sessions", v)
	}
	if v := one(`SELECT group_concat(role || ':' || mode || ':' || native_message_id || ':' || body, ' | ') FROM (SELECT * FROM agent_message ORDER BY seq)`); !strings.Contains(v, "assistant:thinking:msg_01:Need a file.") ||
		!strings.Contains(v, "assistant:text:msg_01-1:Writing it.") || !strings.Contains(v, "assistant:text:msg_02:Done.") || !strings.HasPrefix(v, "user:text:prompt-1-") {
		t.Fatal(v)
	}
	if v := one(`SELECT tool_name || '|' || status || '|' || kind || '|' || result || '|' || locations FROM tool_call`); v != `Write|completed|edit|File created|[{"path":"`+filepath.Join(root, "hello.txt")+`"}]` {
		t.Fatal(v)
	}
	if v := one(`SELECT path || '|' || sha256_after FROM file_touch`); v != "hello.txt|"+store.SHA256Hex([]byte("hi\n")) {
		t.Fatal(v)
	}
	if v := one(`SELECT input_tokens || '/' || output_tokens || '/' || cache_read_tokens FROM usage_delta`); v != "110/45/2000" {
		t.Fatal(v)
	}
	if v := one(`SELECT state FROM turn`); v != "completed" {
		t.Fatal(v)
	}
	if v := one(`SELECT body FROM agent_message WHERE role = 'user'`); v != "Create hello.txt" {
		t.Fatalf("prompt %q", v)
	}
}

func TestSidechainsAreLeftOut(t *testing.T) {
	px, up, db, _, done := setup(t)
	end := sse(`{"type":"message_start","message":{"id":"msg_a","model":"m","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)
	up.replies = []func(http.ResponseWriter){sseReply(end), sseReply(end)}
	post(t, px, done, reqBody(prompt1))
	sub := `{"role":"user","content":"Search the repo for TODOs"}`
	asst := `{"role":"assistant","content":"looking"}`
	tr := `{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"r"}]}`
	post(t, px, done, reqBody(sub, asst, tr)) // a sub-agent's conversation in the same session
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM agent_message`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d messages; the sidechain was recorded", n)
	}
}

func TestGzipResponseIsParsed(t *testing.T) {
	px, up, db, _, done := setup(t)
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write([]byte(`{"id":"msg_z","type":"message","model":"m","content":[{"type":"text","text":"zipped"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`))
	_ = zw.Close()
	raw := gz.Bytes()
	up.replies = []func(http.ResponseWriter){func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(raw)
	}}
	post(t, px, done, strings.Replace(reqBody(prompt1), `"stream":true,`, "", 1))
	var body string
	_ = db.QueryRow(`SELECT body FROM agent_message WHERE role = 'assistant'`).Scan(&body)
	if body != "zipped" {
		t.Fatalf("%q", body)
	}
}

// Claude Code 2.1 puts a system-role message after the user's prompt.
func TestPromptFollowedByASystemMessage(t *testing.T) {
	px, up, db, _, done := setup(t)
	end := sse(`{"type":"message_start","message":{"id":"msg_s","model":"m","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)
	up.replies = []func(http.ResponseWriter){sseReply(end)}
	sys := `{"role":"system","content":[{"type":"text","text":"You are Claude Code"}]}`
	post(t, px, done, reqBody(prompt1, sys))
	var title, body string
	_ = db.QueryRow(`SELECT title FROM agent_session`).Scan(&title)
	_ = db.QueryRow(`SELECT body FROM agent_message WHERE role = 'user'`).Scan(&body)
	if title != "Create hello.txt" || body != "Create hello.txt" {
		t.Fatalf("title %q body %q", title, body)
	}
}

// signalling tells the test when a response half has been handled.
type signalling struct {
	a    *Anthropic
	done chan struct{}
}

func (s signalling) Request(ex Exchange)  { s.a.Request(ex) }
func (s signalling) Response(ex Exchange) { s.a.Response(ex); s.done <- struct{}{} }

func TestHooksMatchBridgeToSession(t *testing.T) {
	h := NewHooks()
	// Two Claude sessions (pids 500 and 600) under one shell (pid 100).
	h.SessionStart("sess-a", "/w", []int{501, 500, 100, 1})
	h.SessionStart("sess-b", "/w", []int{601, 600, 100, 1})
	shell := func(pid int) bool { return pid == 100 }
	if id, ok := h.Match([]int{500, 100}, "/w", time.Now(), shell); !ok || id != "sess-a" {
		t.Fatalf("by pid: %s %v", id, ok)
	}
	if id, ok := h.Match([]int{600, 100}, "/w", time.Now(), shell); !ok || id != "sess-b" {
		t.Fatalf("by pid: %s %v", id, ok)
	}
	// Sharing only the shell is no match, and two sessions in one cwd are ambiguous.
	if _, ok := h.Match([]int{700, 100}, "/w", time.Now(), shell); ok {
		t.Fatal("matched through the shell")
	}
	// One session in a cwd, started just now: the time fallback.
	h2 := NewHooks()
	h2.SessionStart("only", "/x", nil)
	if id, ok := h2.Match([]int{9}, "/x", time.Now(), shell); !ok || id != "only" {
		t.Fatalf("cwd fallback: %s %v", id, ok)
	}
	if _, ok := h2.Match([]int{9}, "/x", time.Now().Add(10*time.Minute), shell); ok {
		t.Fatal("matched a session that started long before")
	}
}

func TestBrotliResponseIsParsed(t *testing.T) {
	px, up, db, _, done := setup(t)
	var br bytes.Buffer
	bw := brotli.NewWriter(&br)
	_, _ = bw.Write([]byte(`{"id":"msg_b","type":"message","model":"m","content":[{"type":"text","text":"squeezed"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`))
	_ = bw.Close()
	raw := br.Bytes()
	up.replies = []func(http.ResponseWriter){func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		_, _ = w.Write(raw)
	}}
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/proxy/anthropic/v1/messages", strings.NewReader(strings.Replace(reqBody(prompt1), `"stream":true,`, "", 1)))
	req.Header.Set("Accept-Encoding", "br") // the client asked for br: bytes come back untouched
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	<-done
	if !bytes.Equal(got, raw) {
		t.Fatal("brotli bytes were changed on the way to the agent")
	}
	var body string
	_ = db.QueryRow(`SELECT body FROM agent_message WHERE role = 'assistant'`).Scan(&body)
	if body != "squeezed" {
		t.Fatalf("%q", body)
	}
}
