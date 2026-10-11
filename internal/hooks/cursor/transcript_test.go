package cursor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixture = `{"role":"user","message":{"content":[{"type":"text","text":"<timestamp>Monday, Aug 24, 2026, 3:01 PM (UTC-5)</timestamp>\n<user_query>\ncreate notes.md\n</user_query>"}]}}
{"role":"assistant","message":{"content":[{"type":"thinking","thinking":"A small file."},{"type":"text","text":"Creating "},{"type":"text","text":"notes.md."},{"type":"tool_use","id":"call_1","name":"Write","input":{"path":"/tmp/proj/notes.md","contents":"line 1\n"}}]}}
{"role":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call_1","content":[{"type":"text","text":"{\"success\":true}"}]}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","name":"StrReplace","input":{"path":"/tmp/proj/notes.md","old_string":"line 1","new_string":"line one"}}]}}
{"role":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call_2","content":"boom","is_error":true}]}}
{"role":"assistant","message":{"content":[{"type":"tool_use","id":"call_2","name":"Shell","input":{"command":"false"}}]}}
{"role":"user","message":{"content":[{"type":"tool_result","tool_use_id":"call_2","content":"boom","is_error":true}]}}
{"role":"assistant","message":{"content":[{"type":"text","text":"Done."}]}}
{"type":"turn_ended","status":"success"}
`

func TestRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	r := reader{}
	n, err := r.Position(path)
	if err != nil || n != 9 {
		t.Fatalf("position %d %v", n, err)
	}
	turn, pos, err := r.Read(path, 0)
	if err != nil || pos != 9 {
		t.Fatalf("read %d %v", pos, err)
	}
	if len(turn.Prompts) != 1 || turn.Prompts[0] != "create notes.md" {
		t.Fatalf("prompts %q", turn.Prompts)
	}
	if turn.HasUsage || turn.Model != "" {
		t.Fatalf("usage or model came from the file: %+v", turn)
	}
	var kinds []string
	for _, it := range turn.Items {
		if it.Tool != nil {
			kinds = append(kinds, "tool:"+it.Tool.Name)
		} else {
			kinds = append(kinds, it.Mode+":"+it.Body)
		}
	}
	want := []string{"thinking:A small file.", "text:Creating notes.md.", "tool:Write", "tool:StrReplace", "tool:Shell", "text:Done."}
	if strings.Join(kinds, "|") != strings.Join(want, "|") {
		t.Fatalf("items %q", kinds)
	}
	w := turn.Items[2].Tool
	if w.NativeID != "call_1" || w.Status != "completed" || w.Result == nil || *w.Result != `{"success":true}` {
		t.Fatalf("write tool %+v", w)
	}
	if !strings.Contains(string(w.Arguments), `"path":"/tmp/proj/notes.md"`) {
		t.Fatalf("arguments %s", w.Arguments)
	}
	if sr := turn.Items[3].Tool; sr.NativeID != "line3-0" || sr.Result != nil {
		t.Fatalf("id-less tool %+v", sr)
	}
	if sh := turn.Items[4].Tool; sh.Status != "failed" || sh.Result == nil || *sh.Result != "boom" {
		t.Fatalf("failed tool %+v", sh)
	}

	// From an offset: only the tail.
	turn, pos, err = r.Read(path, 7)
	if err != nil || pos != 9 || len(turn.Prompts) != 0 || len(turn.Items) != 1 || turn.Items[0].Body != "Done." {
		t.Fatalf("tail %+v %d %v", turn, pos, err)
	}
}

func TestUserText(t *testing.T) {
	cases := map[string]string{
		"plain":                              "plain",
		"<user_query>\nhello\n</user_query>": "hello",
		"<timestamp>Mon</timestamp>\n<user_query>hi</user_query>":    "hi",
		"<timestamp>Mon</timestamp>\n":                               "",
		"<user_query>see <timestamp>x</timestamp> here</user_query>": "see <timestamp>x</timestamp> here",
		"  keep <user_query> inner</user_query> tail ":               "keep <user_query> inner</user_query> tail",
	}
	for in, want := range cases {
		if got := UserText(in); got != want {
			t.Errorf("UserText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSettleWaitsForTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "late.jsonl")
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(path, []byte(fixture), 0o644)
	}()
	start := time.Now()
	reader{}.Settle(context.Background(), path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("settle returned before the file existed")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("settle waited the whole limit")
	}
}

// TestLiveTranscript reads one real Cursor transcript named by
// ATLAS_LIVE_CURSOR_TRANSCRIPT when ATLAS_LIVE=1.
func TestLiveTranscript(t *testing.T) {
	path := os.Getenv("ATLAS_LIVE_CURSOR_TRANSCRIPT")
	if os.Getenv("ATLAS_LIVE") != "1" || path == "" {
		t.Skip("set ATLAS_LIVE=1 and ATLAS_LIVE_CURSOR_TRANSCRIPT")
	}
	turn, pos, err := reader{}.Read(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d lines, %d prompts, %d items", pos, len(turn.Prompts), len(turn.Items))
	if len(turn.Prompts) == 0 || len(turn.Items) == 0 {
		t.Fatalf("nothing read: %+v", turn)
	}
	for _, p := range turn.Prompts {
		if strings.Contains(p, "<user_query>") || strings.HasPrefix(p, "<timestamp>") {
			t.Fatalf("tags left in prompt %q", p)
		}
	}
}
