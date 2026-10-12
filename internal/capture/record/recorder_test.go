package record

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/cli/internal/capture/store"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "w")
	_ = os.MkdirAll(dir, 0o755)
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@e", "-c", "commit.gpgsign=false"}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	run("init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(dir, "old.txt"), []byte("old\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "init")
	r, _ := filepath.EvalSymlinks(dir)
	return r
}

func newRecorder(t *testing.T, enabled bool) (*Recorder, *sql.DB, string) {
	t.Helper()
	root := gitRepo(t)
	st, err := store.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.PutBinding(context.Background(), store.Binding{WorkspaceID: root, Root: root, Mode: store.ModeLocal, Enabled: enabled}, time.Now())
	r := New(st, root)
	clock := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	r.Now = func() time.Time { clock = clock.Add(time.Second); return clock }
	return r, st.DB(), root
}

func q(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(query).Scan(&s); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return s.String
}

func TestOneTurn(t *testing.T) {
	r, db, root := newRecorder(t, true)
	ctx := context.Background()
	s := Session{Agent: "claude-code", NativeID: "6f1c…sess", Cwd: root, Model: "claude-sonnet-4-5"}

	turn, err := r.Prompt(ctx, s, "Add a hello file\n\n═══ Atlas next-steps ═══\nhidden")
	if err != nil || turn != 1 {
		t.Fatalf("%d %v", turn, err)
	}
	if got := q(t, db, `SELECT title || '|' || source || '|' || agent || '|' || model || '|' || branch FROM agent_session`); got != "Add a hello file|acp|claude-code|claude-sonnet-4-5|main" {
		t.Fatal(got)
	}
	if got := q(t, db, `SELECT body FROM agent_message WHERE role = 'user'`); got != "Add a hello file\n\n" {
		t.Fatalf("%q", got)
	}
	_ = r.Message(ctx, s, "msg_01", "thinking", "I should write it.", r.Now())
	_ = r.Message(ctx, s, "msg_01-1", "text", "Writing hello.txt with API_KEY=supersecretvalue123.", r.Now())

	args := json.RawMessage(`{"file_path":"` + filepath.Join(root, "hello.txt") + `","content":"hello\nworld\n"}`)
	_ = r.ToolCall(ctx, s, Tool{NativeID: "toolu_1", Name: "Write", Status: "running", Arguments: args})
	_ = os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hello\nworld\n"), 0o644)
	ok := "File created"
	_ = r.ToolCall(ctx, s, Tool{NativeID: "toolu_1", Name: "Write", Status: "completed", Result: &ok})

	if got := q(t, db, `SELECT tool_name || '|' || kind || '|' || status || '|' || title FROM tool_call WHERE native_call_id = 'toolu_1'`); got != "Write|edit|completed|Write" {
		t.Fatal(got)
	}
	if got := q(t, db, `SELECT path || '|' || existed_before || '|' || deleted || '|' || (sha256_after IS NOT NULL) || '|' || (sketch_after IS NOT NULL) FROM file_touch`); got != "hello.txt|0|0|1|1" {
		t.Fatal(got)
	}
	if got := q(t, db, `SELECT patch FROM agent_edit`); got != "--- before\n+++ after\n+hello\n+world\n+" {
		t.Fatalf("%q", got)
	}
	// A shell command that changes a tracked file: sampled against the prompt anchor.
	_ = os.WriteFile(filepath.Join(root, "old.txt"), []byte("new\n"), 0o644)
	_ = r.ToolCall(ctx, s, Tool{NativeID: "toolu_2", Name: "Bash", Status: "completed", Arguments: json.RawMessage(`{"command":"sed -i s/old/new/ old.txt"}`)})
	if got := q(t, db, `SELECT group_concat(path || ':' || existed_before) FROM file_touch WHERE tool_call_id = (SELECT id FROM tool_call WHERE native_call_id = 'toolu_2')`); got != "old.txt:1" {
		t.Fatalf("shell touches %q", got)
	}
	_ = r.Usage(ctx, s, "claude-sonnet-4-5", store.TokenTotals{Input: 10, Output: 5, CacheRead: 100})
	_ = r.Usage(ctx, s, "claude-sonnet-4-5", store.TokenTotals{Input: 20, Output: 7})
	if got := q(t, db, `SELECT input_tokens || '/' || output_tokens || '/' || cache_read_tokens FROM usage_delta`); got != "30/12/100" {
		t.Fatal(got)
	}
	_ = r.FinishTurn(ctx, s)
	if got := q(t, db, `SELECT state FROM turn`); got != "completed" {
		t.Fatal(got)
	}
	body := q(t, db, `SELECT body FROM agent_message WHERE native_message_id = 'msg_01-1'`)
	if strings.Contains(body, "supersecret") || !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("not redacted: %q", body)
	}
	if rc := q(t, db, `SELECT redaction_counts FROM agent_session`); rc == "{}" {
		t.Fatal("redaction counts not merged")
	}
	// A second prompt continues the turn counter.
	if turn, _ := r.Prompt(ctx, s, "and another"); turn != 2 {
		t.Fatalf("turn %d", turn)
	}
	// A fresh recorder (a restarted shell) seeds from the store.
	r2 := New(r.Store, root)
	if turn, _ := r2.Prompt(ctx, s, "after restart"); turn != 3 {
		t.Fatalf("seeded turn %d", turn)
	}
}

func TestCaptureOffWritesNothing(t *testing.T) {
	r, db, root := newRecorder(t, false)
	ctx := context.Background()
	if turn, err := r.Prompt(ctx, Session{Agent: "codex", NativeID: "x", Cwd: root}, "hi"); turn != 0 || err != nil {
		t.Fatal(turn, err)
	}
	if n := q(t, db, `SELECT count(*) FROM agent_session`); n != "0" {
		t.Fatal(n)
	}
}

func TestFailedCallRecordsNoWrites(t *testing.T) {
	r, db, root := newRecorder(t, true)
	ctx := context.Background()
	s := Session{Agent: "claude-code", NativeID: "s", Cwd: root}
	_, _ = r.Prompt(ctx, s, "go")
	args := json.RawMessage(`{"file_path":"x.go","old_string":"a","new_string":"b"}`)
	_ = r.ToolCall(ctx, s, Tool{NativeID: "t", Name: "Edit", Status: "failed", Arguments: args})
	_ = r.ToolCall(ctx, s, Tool{NativeID: "t", Name: "Edit", Status: "completed", Arguments: args})
	if n := q(t, db, `SELECT count(*) FROM file_touch`); n != "0" {
		t.Fatal(n)
	}
}
