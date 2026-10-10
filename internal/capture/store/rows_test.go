package store

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T, mode string) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	_ = s.PutBinding(context.Background(), Binding{WorkspaceID: s.Root, Root: s.Root, Mode: mode, Enabled: true}, t0)
	return s
}

func str(s string) *string { return &s }

func one[T any](t *testing.T, s *Store, q string, args ...any) T {
	t.Helper()
	var v T
	if err := s.db.QueryRow(q, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return v
}

func TestSessionUpsertRules(t *testing.T) {
	s := newStore(t, ModeLocal)
	ctx := context.Background()
	k := SessionKey{s.Root, "acp", "sess-1"}
	id, err := s.UpsertSession(ctx, k, str("claude-code"), str("claude-sonnet"), str("/w"), str("main"), t0)
	if err != nil || !strings.HasPrefix(id, "as-") || len(id) != 35 {
		t.Fatalf("%q %v", id, err)
	}
	id2, _ := s.UpsertSession(ctx, k, nil, str("claude-opus"), nil, str("feature"), t0.Add(time.Second))
	if id2 != id {
		t.Fatal("upsert made a second row")
	}
	if m := one[string](t, s, `SELECT model || '|' || branch || '|' || agent || '|' || sync_state FROM agent_session WHERE id = ?`, id); m != "claude-opus|main|claude-code|local" {
		t.Fatalf("model new wins, branch first wins: %s", m)
	}
	cloud := newStore(t, ModeCloud)
	cid, _ := cloud.UpsertSession(ctx, SessionKey{cloud.Root, "acp", "x"}, nil, nil, nil, nil, t0)
	if st := one[string](t, cloud, `SELECT sync_state FROM agent_session WHERE id = ?`, cid); st != "pending" {
		t.Fatalf("cloud sync_state %s", st)
	}
}

func TestMessages(t *testing.T) {
	s := newStore(t, ModeLocal)
	ctx := context.Background()
	id, _ := s.UpsertSession(ctx, SessionKey{s.Root, "acp", "s"}, nil, nil, nil, nil, t0)
	ok, err := s.RecordMessage(ctx, Message{SessionID: id, TurnSeq: 1, NativeID: "msg_1", Role: "assistant", Mode: "text", Body: "hello", CreatedAt: t0})
	if err != nil || !ok {
		t.Fatal(err)
	}
	if ok, _ := s.RecordMessage(ctx, Message{SessionID: id, TurnSeq: 1, NativeID: "msg_1", Role: "assistant", Mode: "text", Body: "hello", CreatedAt: t0}); ok {
		t.Fatal("re-recording the same native id must be a no-op")
	}
	big := strings.Repeat("é", SpillBytes) // 2 bytes each: over the spill limit
	_, _ = s.RecordMessage(ctx, Message{SessionID: id, TurnSeq: 1, NativeID: "msg_2", Role: "assistant", Mode: "text", Body: big, CreatedAt: t0})
	var preview string
	var body, ref *string
	var n int
	_ = s.db.QueryRow(`SELECT preview, body, body_ref, body_bytes FROM agent_message WHERE native_message_id = 'msg_2'`).Scan(&preview, &body, &ref, &n)
	if body != nil || ref == nil || n != len(big) || len(preview) != PreviewBytes || !strings.HasPrefix(big, preview) {
		t.Fatalf("spill: body=%v ref=%v n=%d preview=%d", body, ref, n, len(preview))
	}
	blob, err := os.ReadFile(filepath.Join(s.Root, DirName, "blobs", (*ref)[:2], *ref))
	if err != nil || string(blob) != big || SHA256Hex(blob) != *ref {
		t.Fatal("blob missing or wrong")
	}
	if seq := one[int64](t, s, `SELECT value FROM counter WHERE name = 'seq'`); seq != 2 {
		t.Fatalf("seq %d", seq)
	}
}

func TestToolCallUpsertRules(t *testing.T) {
	s := newStore(t, ModeLocal)
	ctx := context.Background()
	sid, _ := s.UpsertSession(ctx, SessionKey{s.Root, "acp", "s"}, nil, nil, nil, nil, t0)
	tc := ToolCall{SessionID: sid, TurnSeq: 1, NativeID: "toolu_1", ToolName: "Edit", Title: str("Edit"), Kind: str("edit"),
		Status: "running", Locations: `[{"path":"/w/a.go"}]`, Arguments: str(`{"file_path":"/w/a.go"}`)}
	id, prev, err := s.UpsertToolCall(ctx, tc, t0)
	if err != nil || prev != "" {
		t.Fatal(err)
	}
	upd := ToolCall{SessionID: sid, TurnSeq: 1, NativeID: "toolu_1", ToolName: "Edit", Status: "completed", Locations: "[]", Result: str("ok")}
	id2, prev, _ := s.UpsertToolCall(ctx, upd, t0)
	if id2 != id || prev != "running" {
		t.Fatalf("%s %s", id2, prev)
	}
	got := one[string](t, s, `SELECT title || '|' || kind || '|' || status || '|' || locations || '|' || arguments || '|' || result FROM tool_call WHERE id = ?`, id)
	if got != `Edit|edit|completed|[{"path":"/w/a.go"}]|{"file_path":"/w/a.go"}|ok` {
		t.Fatalf("%s", got)
	}
	_, _, _ = s.UpsertToolCall(ctx, ToolCall{SessionID: sid, TurnSeq: 1, NativeID: "toolu_2", ToolName: "Bash", Status: "completed", ResultBinary: []byte{0xff, 0xfe}}, t0)
	if b := one[int](t, s, `SELECT result_binary FROM tool_call WHERE native_call_id = 'toolu_2'`); b != 1 {
		t.Fatal("binary result not flagged")
	}
}

func TestUsageIsCumulative(t *testing.T) {
	s := newStore(t, ModeLocal)
	ctx := context.Background()
	sid, _ := s.UpsertSession(ctx, SessionKey{s.Root, "acp", "s"}, nil, str("m-1"), nil, nil, t0)
	_ = s.RecordUsage(ctx, sid, 1, nil, TokenTotals{Input: 100, Output: 10}, t0)
	_ = s.RecordUsage(ctx, sid, 1, nil, TokenTotals{Input: 250, Output: 30, CacheRead: 5}, t0)
	_ = s.RecordUsage(ctx, sid, 2, str("m-2"), TokenTotals{Input: 300, Output: 31, CacheRead: 5}, t0)
	_ = s.RecordUsage(ctx, sid, 2, nil, TokenTotals{Input: 300, Output: 31, CacheRead: 5}, t0) // no change: no delta
	got := one[string](t, s, `SELECT group_concat(turn_seq || ':' || model || ':' || input_tokens || '/' || output_tokens || '/' || cache_read_tokens, ' ') FROM usage_delta`)
	if got != "1:m-1:250/30/5 2:m-2:50/1/0" {
		t.Fatalf("deltas %s", got)
	}
	if tt := one[string](t, s, `SELECT token_totals FROM agent_session WHERE id = ?`, sid); tt != `{"input_tokens":300,"output_tokens":31,"cache_creation_tokens":0,"cache_read_tokens":5,"reasoning_tokens":0}` {
		t.Fatalf("totals %s", tt)
	}
	// A counter that restarts below the cursor counts as new.
	_ = s.RecordUsage(ctx, sid, 3, nil, TokenTotals{Input: 40}, t0)
	if in := one[int64](t, s, `SELECT input_tokens FROM usage_delta WHERE turn_seq = 3`); in != 40 {
		t.Fatalf("restart %d", in)
	}
}

func TestRedactionCountsKeepOrder(t *testing.T) {
	s := newStore(t, ModeLocal)
	ctx := context.Background()
	sid, _ := s.UpsertSession(ctx, SessionKey{s.Root, "acp", "s"}, nil, nil, nil, nil, t0)
	_ = s.AddRedactionCounts(ctx, sid, map[string]int{"credential_value": 2})
	_ = s.AddRedactionCounts(ctx, sid, map[string]int{"entropy": 1, "credential_value": 1, "vendor_rule": 3})
	if rc := one[string](t, s, `SELECT redaction_counts FROM agent_session WHERE id = ?`, sid); rc != `{"credential_value":3,"entropy":1,"vendor_rule":3}` {
		t.Fatalf("%s", rc)
	}
}

func TestTurnsAndTitle(t *testing.T) {
	s := newStore(t, ModeLocal)
	ctx := context.Background()
	sid, _ := s.UpsertSession(ctx, SessionKey{s.Root, "acp", "s"}, nil, nil, nil, nil, t0)
	_ = s.BeginTurn(ctx, sid, 1, t0)
	_ = s.BeginTurn(ctx, sid, 1, t0)
	_ = s.CompleteTurn(ctx, sid, 1, t0.Add(time.Minute))
	if st := one[string](t, s, `SELECT state || '|' || ended_at FROM turn WHERE session_id = ?`, sid); st != "completed|2026-10-10T12:01:00+00:00" {
		t.Fatal(st)
	}
	if n, _ := s.MaxTurnSeq(ctx, sid); n != 1 {
		t.Fatal(n)
	}
	_ = s.SetTitleIfAbsent(ctx, sid, "first")
	_ = s.SetTitleIfAbsent(ctx, sid, "second")
	if ti := one[string](t, s, `SELECT title FROM agent_session WHERE id = ?`, sid); ti != "first" {
		t.Fatal(ti)
	}
}
