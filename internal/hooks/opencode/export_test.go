package opencode

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// fixture is an export shaped like OpenCode 1.19.0's: two turns, the second
// with reasoning, a glob, a write, an apply_patch that names its files, and
// a failed bash call.
const fixture = `{
  "info": {"id": "ses_1", "title": "t", "model": {"id": "gemini-3.7-flash", "providerID": "google"}},
  "messages": [
    {"info": {"id": "msg_u1", "role": "user", "sessionID": "ses_1"},
     "parts": [{"id": "prt_u1", "type": "text", "text": "say hi"}]},
    {"info": {"id": "msg_a1", "role": "assistant", "modelID": "gemini-3.7-flash", "providerID": "google",
              "tokens": {"total": 16226, "input": 16172, "output": 9, "reasoning": 45, "cache": {"write": 0, "read": 0}}},
     "parts": [{"id": "prt_s1", "type": "step-start"},
               {"id": "prt_t1", "type": "text", "text": "Hi! How can I help you today?"},
               {"id": "prt_f1", "type": "step-finish", "reason": "stop"},
               {"id": "prt_p1", "type": "patch", "files": ["/x/run.jsonl"]}]},
    {"info": {"id": "msg_u2", "role": "user", "sessionID": "ses_1"},
     "parts": [{"id": "prt_u2", "type": "text", "text": "<system-reminder>be nice</system-reminder>\nwrite hello.txt"},
               {"id": "prt_u3", "type": "text", "text": "<system-reminder>only this</system-reminder>"}]},
    {"info": {"id": "msg_a2", "role": "assistant", "modelID": "gemini-3.7-flash",
              "tokens": {"input": 100, "output": 10, "reasoning": 5, "cache": {"write": 7, "read": 20}}},
     "parts": [{"id": "prt_r1", "type": "reasoning", "text": "Plan the write."},
               {"type": "tool", "tool": "glob", "callID": "call_g", "state": {"status": "completed", "input": {"pattern": "hello.txt"}, "output": "No files found", "metadata": {"count": 0, "truncated": false}}},
               {"type": "tool", "tool": "write", "callID": "call_w", "state": {"status": "completed", "input": {"filePath": "/x/hello.txt", "content": "hello\n"}, "output": "Wrote file successfully.", "metadata": {"filepath": "/x/hello.txt", "exists": false}}}]},
    {"info": {"id": "msg_a3", "role": "assistant", "modelID": "gpt-5.3-codex",
              "tokens": {"input": 50, "output": 20, "reasoning": 0, "cache": {"write": 0, "read": 0}}},
     "parts": [{"id": "prt_ap", "type": "tool", "tool": "apply_patch", "callID": "call_p", "state": {"status": "completed", "input": {"patchText": "*** Begin Patch"}, "output": "ok", "metadata": {"files": [{"filePath": "/x/a.go", "relativePath": "a.go"}, {"filePath": "/x/b.go"}]}}},
               {"type": "tool", "tool": "bash", "callID": "call_b", "state": {"status": "error", "input": {"command": "false"}, "error": "exit 1", "metadata": {"files": 3}}},
               {"type": "tool", "tool": "todowrite", "callID": "call_t", "state": {"status": "running", "input": {}}},
               {"id": "prt_t2", "type": "text", "text": "done."},
               {"id": "prt_t3", "type": "text", "text": "   "}]}
  ]
}`

func TestTurnOf(t *testing.T) {
	s, err := parseExport([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 5 {
		t.Fatalf("%d messages", len(s.Messages))
	}
	turn := turnOf(s.Messages[2:])
	if len(turn.Prompts) != 1 || turn.Prompts[0] != "write hello.txt" {
		t.Fatalf("prompts %q", turn.Prompts)
	}
	if turn.Model != "gpt-5.3-codex" {
		t.Fatalf("model %q", turn.Model)
	}
	if !turn.HasUsage || turn.Usage.Input != 150 || turn.Usage.Output != 30 || turn.Usage.Reasoning != 5 || turn.Usage.CacheRead != 20 || turn.Usage.CacheCreation != 7 {
		t.Fatalf("usage %+v", turn.Usage)
	}
	if len(turn.Items) != 7 {
		t.Fatalf("%d items", len(turn.Items))
	}
	if it := turn.Items[0]; it.Mode != "thinking" || it.Body != "Plan the write." || it.ID != "prt_r1" {
		t.Fatalf("reasoning %+v", it)
	}
	glob := turn.Items[1].Tool
	if glob == nil || glob.NativeID != "call_g" || glob.Name != "glob" || glob.Status != "completed" || string(glob.Arguments) != `{"pattern": "hello.txt"}` || glob.Result == nil || *glob.Result != "No files found" || glob.Locations != nil {
		t.Fatalf("glob %+v", glob)
	}
	write := turn.Items[2].Tool
	if write == nil || write.Name != "write" || write.Locations != nil || !strings.Contains(string(write.Arguments), `"filePath": "/x/hello.txt"`) {
		t.Fatalf("write %+v", write)
	}
	patch := turn.Items[3].Tool
	if patch == nil || patch.NativeID != "prt_ap" || strings.Join(patch.Locations, ",") != "/x/a.go,/x/b.go" {
		t.Fatalf("apply_patch %+v", patch)
	}
	bash := turn.Items[4].Tool
	if bash == nil || bash.Status != "failed" || bash.Result == nil || *bash.Result != "exit 1" || bash.Locations != nil {
		t.Fatalf("bash %+v", bash)
	}
	if todo := turn.Items[5].Tool; todo == nil || todo.Status != "running" || todo.Result != nil {
		t.Fatalf("todowrite %+v", todo)
	}
	if it := turn.Items[6]; it.Mode != "text" || it.Body != "done." {
		t.Fatalf("text %+v", it)
	}

	first := turnOf(s.Messages[:2])
	if len(first.Prompts) != 1 || first.Prompts[0] != "say hi" || len(first.Items) != 1 || first.Items[0].Body != "Hi! How can I help you today?" || first.Usage.Input != 16172 {
		t.Fatalf("first turn %+v", first)
	}
	if empty := turnOf(nil); empty.HasUsage || len(empty.Items) != 0 {
		t.Fatalf("empty %+v", empty)
	}
}

func TestStripReminders(t *testing.T) {
	cases := map[string]string{
		"plain":                                "plain",
		"<system-reminder>a</system-reminder>": "",
		"<system-reminder>a</system-reminder>real<system-reminder>b</system-reminder>": "real",
		"  x <system-reminder>unclosed":                             "x <system-reminder>unclosed",
		"<system-reminder>\nmulti\nline\n</system-reminder>\n\nask": "ask",
	}
	for in, want := range cases {
		if got := StripReminders(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}

// rd is the reader under test.
var rd reader

// fakeExport replaces runExport with one that writes body for the expected
// id and counts its calls.
func fakeExport(t *testing.T, id string, body *string, calls *int) {
	t.Helper()
	prev := runExport
	runExport = func(_ context.Context, got string, out *os.File) error {
		*calls++
		if got != id {
			return errors.New("session not found")
		}
		_, err := out.WriteString(*body)
		return err
	}
	t.Cleanup(func() { runExport = prev })
}

func TestReaderPositionAndRead(t *testing.T) {
	root := t.TempDir()
	const id = "ses_1"
	path := CachePath(root, id)
	body := fixture
	calls := 0
	fakeExport(t, id, &body, &calls)

	// No cache: Position exports and stops before the prompt just sent.
	pos, err := rd.Position(path)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 5 || calls != 1 {
		t.Fatalf("position %d after %d exports", pos, calls)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("cache: %v %v", fi, err)
	}
	// A cache: Position trusts it, no export.
	if pos, err := rd.Position(path); err != nil || pos != 5 || calls != 1 {
		t.Fatalf("cached position %d %v after %d exports", pos, err, calls)
	}
	// Read exports afresh and returns what follows the position.
	body = fixture[:strings.LastIndex(fixture, "]")] + `,
    {"info": {"id": "msg_u3", "role": "user"}, "parts": [{"id": "prt_u9", "type": "text", "text": "thanks"}]},
    {"info": {"id": "msg_a4", "role": "assistant", "modelID": "gemini-3.7-flash", "tokens": {"input": 1, "output": 2, "reasoning": 0, "cache": {"read": 0, "write": 0}}},
     "parts": [{"id": "prt_t9", "type": "text", "text": "welcome"}]}
  ]
}`
	turn, next, err := rd.Read(path, pos)
	if err != nil {
		t.Fatal(err)
	}
	if next != 7 || calls != 2 || len(turn.Prompts) != 1 || turn.Prompts[0] != "thanks" || len(turn.Items) != 1 || turn.Items[0].Body != "welcome" || turn.Usage.Output != 2 {
		t.Fatalf("read: next %d calls %d turn %+v", next, calls, turn)
	}
	if pos, err := rd.Position(path); err != nil || pos != 7 {
		t.Fatalf("position after read %d %v", pos, err)
	}

	// A trailing user message at a fresh export is the prompt in flight.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	body = `{"info":{},"messages":[{"info":{"id":"u","role":"user"},"parts":[]}]}`
	if pos, err := rd.Position(path); err != nil || pos != 0 {
		t.Fatalf("in-flight prompt counted: %d %v", pos, err)
	}

	// A failed export keeps the good cache and reports the failure.
	body = `{"info":{},"messages":[]}`
	if _, _, err := rd.Read(path, 0); err != nil {
		t.Fatal(err)
	}
	prev := runExport
	runExport = func(context.Context, string, *os.File) error { return errors.New("error: session not found") }
	if _, _, err := rd.Read(path, 0); err == nil {
		t.Fatal("a failed export passed")
	}
	runExport = func(_ context.Context, _ string, out *os.File) error {
		_, err := out.WriteString("{truncated")
		return err
	}
	if _, _, err := rd.Read(path, 0); err == nil {
		t.Fatal("a truncated export passed")
	}
	runExport = prev
	if s, err := load(path); err != nil || len(s.Messages) != 0 {
		t.Fatalf("cache lost: %v %v", s, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
	if _, err := fetch("../x", filepath.Join(root, "x.json")); !errors.Is(err, hooks.ErrBadSessionID) {
		t.Fatalf("an unusable id was exported: %v", err)
	}
}

func TestErrorLine(t *testing.T) {
	if got := errorLine("Exporting session: x\n\x1b[31mError: Session not found\x1b[0m\n"); !strings.Contains(got, "Session not found") {
		t.Fatalf("%q", got)
	}
	if errorLine("Exporting session: x\n") != "" {
		t.Fatal("no error line expected")
	}
}
