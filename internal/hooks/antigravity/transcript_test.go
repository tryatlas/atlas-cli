package antigravity

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
)

// fixture is the shape of agy 1.x's transcript_full.jsonl: a captured first
// turn, then a blank line and a second turn (whose write tool is double
// encoded and whose last step is marked truncated).
const fixture = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-20T23:46:29Z","content":"<USER_REQUEST>\nread a.txt and tell me what it says\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-05-20T19:46:29-04:00.\n</ADDITIONAL_METADATA>"}
{"step_index":1,"source":"SYSTEM","type":"CONVERSATION_HISTORY","status":"DONE","created_at":"2026-05-20T23:46:29Z"}
{"step_index":2,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T23:46:29Z","tool_calls":[{"name":"list_dir","args":{"DirectoryPath":"\"/work\"","toolAction":"\"Listing\""}}]}
{"step_index":3,"source":"MODEL","type":"LIST_DIRECTORY","status":"DONE","created_at":"2026-05-20T23:46:31Z","content":"Empty directory"}
{"step_index":5,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T23:46:33Z","thinking":"**Looking**\n\nNothing there.","tool_calls":[{"name":"run_command","args":{"CommandLine":"\"find . -name \\\"a.txt\\\"\"","Cwd":"\"/work\"","WaitMsBeforeAsync":"2000"}}]}
{"step_index":6,"source":"MODEL","type":"RUN_COMMAND","status":"ERROR","created_at":"2026-05-20T23:46:34Z","content":"Permission denied"}
{"step_index":7,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T23:46:35Z","content":"There is no a.txt here."}

{"step_index":8,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-05-20T23:47:00Z","content":"<USER_REQUEST>\ncreate it\n</USER_REQUEST>"}
{"step_index":9,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T23:47:01Z","tool_calls":[{"name":"write_to_file","args":{"TargetFile":"\"/work/a.txt\"","Overwrite":"false","CodeContent":"\"hello\""}}]}
{"step_index":10,"source":"MODEL","type":"WRITE_FILE","status":"DONE","created_at":"2026-05-20T23:47:02Z","content":"Created"}
{"step_index":11,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-05-20T23:47:03Z","content":"Done.","truncated_fields":["content"]}
`

func writeFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	if err := os.WriteFile(path, []byte(fixture), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// live is the shape agy 1.3.3 wrote (captured 2026-10-12): the USER_INPUT
// step lands before PreInvocation fires, PLANNER_RESPONSE steps carry token
// counts, args are no longer double encoded, and the tool output is a
// GENERIC step.
const live = `{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","created_at":"2026-10-11T20:46:50Z","content":"<USER_REQUEST>\nCreate a file named note.txt containing the word hello\n</USER_REQUEST>\n<ADDITIONAL_METADATA>\nThe current local time is: 2026-10-12T02:46:50+06:00.\n</ADDITIONAL_METADATA>\n<USER_SETTINGS_CHANGE>\nThe user changed setting ` + "`Model Selection`" + ` from None to Gemini 3.1 Pro (Low).\n</USER_SETTINGS_CHANGE>"}
{"step_index":1,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-11T20:46:51Z","input_tokens":2028,"cache_read_tokens":10889,"output_tokens":199,"tool_calls":[{"name":"write_to_file","args":{"CodeContent":"hello","Description":"Created note.txt with content 'hello'","Overwrite":false,"TargetFile":"/private/tmp/hooks-agy/note.txt","toolAction":"Creating note.txt","toolSummary":"Create note.txt"}}]}
{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","created_at":"2026-10-11T20:46:55Z","content":"Created At: 2026-10-12T02:46:55+06:00\nCompleted At: 2026-10-12T02:46:55+06:00\nCreated file file:///private/tmp/hooks-agy/note.txt with requested content."}
{"step_index":3,"source":"MODEL","type":"PLANNER_RESPONSE","status":"DONE","created_at":"2026-10-11T20:46:56Z","input_tokens":2343,"cache_read_tokens":10890,"output_tokens":77,"content":"I have created the file ` + "`note.txt`" + `."}
`

func TestPosition(t *testing.T) {
	r := reader{}
	if n, err := r.Position(filepath.Join(t.TempDir(), "missing.jsonl")); err != nil || n != 0 {
		t.Fatalf("missing file: %d, %v", n, err)
	}
	path := writeFixture(t)
	n, err := r.Position(path)
	if err != nil {
		t.Fatal(err)
	}
	if n != 11 {
		t.Fatalf("Position = %d, want 11 non-blank lines", n)
	}
	// The next turn's prompt is already there when PreInvocation fires: the
	// position stays before it (and before agy's bookkeeping step).
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"step_index":12,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nnext\n</USER_REQUEST>"}` + "\n" +
		`{"step_index":13,"source":"SYSTEM","type":"CONVERSATION_HISTORY","status":"DONE"}` + "\n")
	_ = f.Close()
	if n, err = r.Position(path); err != nil || n != 11 {
		t.Fatalf("Position with a pending prompt = %d, %v; want 11", n, err)
	}
	turn, pos, err := r.Read(path, n)
	if err != nil || pos != 13 || len(turn.Prompts) != 1 || turn.Prompts[0] != "next" {
		t.Fatalf("Read from Position = %+v, %d, %v", turn, pos, err)
	}
	// A transcript holding only the prompt is at 0.
	only := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	if err := os.WriteFile(only, []byte(`{"step_index":0,"source":"USER_EXPLICIT","type":"USER_INPUT","content":"hi"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err = r.Position(only); err != nil || n != 0 {
		t.Fatalf("Position of a prompt-only transcript = %d, %v", n, err)
	}
}

func TestReadLiveShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	if err := os.WriteFile(path, []byte(live), 0o644); err != nil {
		t.Fatal(err)
	}
	turn, pos, err := reader{}.Read(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 4 {
		t.Fatalf("pos = %d, want 4", pos)
	}
	if len(turn.Prompts) != 1 || turn.Prompts[0] != "Create a file named note.txt containing the word hello" {
		t.Fatalf("Prompts = %q", turn.Prompts)
	}
	if !turn.HasUsage || turn.Usage.Input != 2028+2343 || turn.Usage.Output != 199+77 || turn.Usage.CacheRead != 10889+10890 || turn.Usage.Reasoning != 0 {
		t.Fatalf("Usage = %+v (has %v)", turn.Usage, turn.HasUsage)
	}
	if turn.Model != "" {
		t.Fatalf("Model = %q, want none from the transcript", turn.Model)
	}
	if len(turn.Items) != 2 {
		t.Fatalf("Items = %+v", turn.Items)
	}
	w := turn.Items[0].Tool
	if w == nil || w.Name != "write_to_file" || w.Kind != "edit" || len(w.Locations) != 1 || w.Locations[0] != "/private/tmp/hooks-agy/note.txt" || w.Status != "completed" || w.Result == nil || !strings.HasPrefix(*w.Result, "Created At:") {
		t.Fatalf("write = %+v", w)
	}
	var args map[string]any
	if err := json.Unmarshal(w.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["Overwrite"] != false || args["CodeContent"] != "hello" {
		t.Fatalf("plain args changed: %s", w.Arguments)
	}
	// What the recorder will file it as.
	if name := record.CanonicalToolName(w.Name, w.Title, w.Kind, w.Arguments); name != "Write" {
		t.Fatalf("canonical name = %q, want Write", name)
	}
	if turn.Items[1].Mode != "text" || turn.Items[1].Body != "I have created the file `note.txt`." {
		t.Fatalf("text = %+v", turn.Items[1])
	}
}

// TestLiveTranscript reads a real transcript_full.jsonl named by
// ATLAS_AGY_TRANSCRIPT when ATLAS_LIVE=1: the turn made by
// agy -p "Create a file named note.txt containing the word hello".
func TestLiveTranscript(t *testing.T) {
	path := os.Getenv("ATLAS_AGY_TRANSCRIPT")
	if os.Getenv("ATLAS_LIVE") != "1" || path == "" {
		t.Skip("set ATLAS_LIVE=1 and ATLAS_AGY_TRANSCRIPT to read a real agy transcript")
	}
	r := reader{}
	n, err := r.Position(path)
	if err != nil {
		t.Fatal(err)
	}
	turn, pos, err := r.Read(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pos != n {
		t.Fatalf("Position = %d but Read ends at %d: the transcript ends with a non-model step", n, pos)
	}
	if len(turn.Prompts) != 1 || !strings.Contains(turn.Prompts[0], "note.txt") {
		t.Fatalf("Prompts = %q", turn.Prompts)
	}
	if !turn.HasUsage || turn.Usage.Input == 0 || turn.Usage.Output == 0 {
		t.Fatalf("Usage = %+v", turn.Usage)
	}
	var wrote, said bool
	for _, it := range turn.Items {
		if it.Tool != nil && writeTools[it.Tool.Name] {
			wrote = it.Tool.Kind == "edit" && len(it.Tool.Locations) == 1 && strings.HasSuffix(it.Tool.Locations[0], "/note.txt") && it.Tool.Result != nil
		}
		if it.Mode == "text" && it.Body != "" {
			said = true
		}
	}
	if !wrote || !said {
		t.Fatalf("wrote=%v said=%v in %+v", wrote, said, turn.Items)
	}
	t.Logf("%d steps, prompt %q, usage %+v, %d items", pos, turn.Prompts[0], turn.Usage, len(turn.Items))
}

func TestReadWholeTranscript(t *testing.T) {
	path := writeFixture(t)
	turn, pos, err := reader{}.Read(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 11 {
		t.Fatalf("pos = %d, want 11", pos)
	}
	if len(turn.Prompts) != 2 || turn.Prompts[0] != "read a.txt and tell me what it says" || turn.Prompts[1] != "create it" {
		t.Fatalf("Prompts = %q", turn.Prompts)
	}
	if turn.HasUsage || turn.Model != "" {
		t.Fatalf("usage/model should be absent: %+v", turn)
	}
	// list_dir, thinking, run_command, text, write_to_file, text.
	if len(turn.Items) != 6 {
		t.Fatalf("Items = %d: %+v", len(turn.Items), turn.Items)
	}
	ls := turn.Items[0].Tool
	if ls == nil || ls.NativeID != "2-0" || ls.Name != "list_dir" || ls.Kind != "read" || ls.Status != "completed" || ls.Result == nil || *ls.Result != "Empty directory" {
		t.Fatalf("item 0 = %+v", turn.Items[0])
	}
	var args map[string]any
	if err := json.Unmarshal(ls.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["DirectoryPath"] != "/work" {
		t.Fatalf("double-encoded arg not decoded: %s", ls.Arguments)
	}
	if turn.Items[1].Mode != "thinking" || turn.Items[1].ID != "5-thinking" || turn.Items[1].Body != "**Looking**\n\nNothing there." {
		t.Fatalf("item 1 = %+v", turn.Items[1])
	}
	run := turn.Items[2].Tool
	if run == nil || run.NativeID != "5-0" || run.Kind != "execute" || run.Status != "failed" || run.Result == nil || *run.Result != "Permission denied" {
		t.Fatalf("item 2 = %+v", turn.Items[2])
	}
	if err := json.Unmarshal(run.Arguments, &args); err != nil {
		t.Fatal(err)
	}
	if args["CommandLine"] != `find . -name "a.txt"` || args["WaitMsBeforeAsync"] != "2000" {
		t.Fatalf("run args = %s", run.Arguments)
	}
	if turn.Items[3].Mode != "text" || turn.Items[3].ID != "7-text" || turn.Items[3].Body != "There is no a.txt here." {
		t.Fatalf("item 3 = %+v", turn.Items[3])
	}
	w := turn.Items[4].Tool
	if w == nil || w.NativeID != "9-0" || w.Name != "write_to_file" || len(w.Locations) != 1 || w.Locations[0] != "/work/a.txt" || w.Result == nil || *w.Result != "Created" {
		t.Fatalf("item 4 = %+v", turn.Items[4])
	}
	if turn.Items[5].Body != "Done." {
		t.Fatalf("truncated step not read: %+v", turn.Items[5])
	}
}

func TestReadFromOffset(t *testing.T) {
	path := writeFixture(t)
	turn, pos, err := reader{}.Read(path, 7)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 11 {
		t.Fatalf("pos = %d, want 11", pos)
	}
	if len(turn.Prompts) != 1 || turn.Prompts[0] != "create it" {
		t.Fatalf("Prompts = %q", turn.Prompts)
	}
	if len(turn.Items) != 2 || turn.Items[0].Tool == nil || turn.Items[0].Tool.Name != "write_to_file" {
		t.Fatalf("Items = %+v", turn.Items)
	}
	// Reading at the end yields an empty turn, not an error.
	turn, pos, err = reader{}.Read(path, 11)
	if err != nil || pos != 11 || len(turn.Prompts) != 0 || len(turn.Items) != 0 {
		t.Fatalf("read at end = %+v, %d, %v", turn, pos, err)
	}
}

func TestPendingPrompt(t *testing.T) {
	if got := pendingPrompt(filepath.Join(t.TempDir(), "missing.jsonl")); got != "" {
		t.Fatalf("missing transcript = %q", got)
	}
	// Both fixture turns are answered: nothing pending.
	path := writeFixture(t)
	if got := pendingPrompt(path); got != "" {
		t.Fatalf("answered transcript = %q", got)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"step_index":12,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"<USER_REQUEST>\nnext\n</USER_REQUEST>"}` + "\n" +
		`{"step_index":13,"source":"SYSTEM","type":"CONVERSATION_HISTORY","status":"DONE"}` + "\n")
	_ = f.Close()
	if got := pendingPrompt(path); got != "next" {
		t.Fatalf("pending = %q, want next", got)
	}
	// The live shape, cut after the prompt step as PreInvocation sees it.
	live1 := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	if err := os.WriteFile(live1, []byte(strings.SplitAfter(live, "\n")[0]), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := pendingPrompt(live1); got != "Create a file named note.txt containing the word hello" {
		t.Fatalf("live pending = %q", got)
	}
}

func TestPromptUnwrap(t *testing.T) {
	if got := prompt("no wrapper here "); got != "no wrapper here" {
		t.Fatalf("prompt = %q", got)
	}
	if got := prompt("<USER_REQUEST>\n  hi\n</USER_REQUEST>\n<X>y</X>"); got != "hi" {
		t.Fatalf("prompt = %q", got)
	}
}

func TestSettleWaitsForLateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript_full.jsonl")
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(path, []byte(fixture), 0o644)
	}()
	start := time.Now()
	reader{}.Settle(context.Background(), path)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("Settle returned before the file appeared")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("Settle took too long")
	}

	// A file that never appears: back within the 2s wait.
	start = time.Now()
	reader{}.Settle(context.Background(), filepath.Join(t.TempDir(), "never.jsonl"))
	if d := time.Since(start); d < 2*time.Second || d > 3*time.Second {
		t.Fatalf("Settle on a missing file took %v", d)
	}

	// A cancelled context returns at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	reader{}.Settle(ctx, filepath.Join(t.TempDir(), "never.jsonl"))
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("Settle ignored the cancelled context")
	}
}
