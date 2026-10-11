package copilot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadATurnWithAToolThatReportsFiles(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session.start","data":{"sessionId":"s1","version":1},"id":"1","timestamp":"2026-03-03T00:00:00Z","parentId":null}`,
		`{"type":"session.model_change","data":{"source":"startup","newModel":"auto"},"id":"2","timestamp":"2026-03-03T00:00:00Z","parentId":"1"}`,
		`{"type":"user.message","data":{"content":"Create hello.txt","transformedContent":"Create hello.txt","interactionId":"i1"},"id":"3","timestamp":"2026-03-03T00:00:01Z","parentId":"2"}`,
		`{"type":"assistant.turn_start","data":{"turnId":"0","interactionId":"i1"},"id":"4","timestamp":"2026-03-03T00:00:02Z","parentId":"3"}`,
		`{"type":"assistant.message","data":{"messageId":"m1","content":"I'll create that file.","toolRequests":[{"toolCallId":"tc1"}],"outputTokens":12},"id":"5","timestamp":"2026-03-03T00:00:03Z","parentId":"4"}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"tc1","toolName":"create","arguments":{"path":"/repo/hello.txt","content":"hi"}},"id":"6","timestamp":"2026-03-03T00:00:04Z","parentId":"5"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tc1","success":true,"result":{"content":"Created."},"toolTelemetry":{"properties":{"filePaths":"[\"/repo/hello.txt\"]"},"metrics":{"linesAdded":1,"linesRemoved":0}}},"id":"7","timestamp":"2026-03-03T00:00:05Z","parentId":"6"}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"tc2","toolName":"bash","arguments":{"command":"ls"}},"id":"8","timestamp":"2026-03-03T00:00:06Z","parentId":"7"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tc2","success":false,"result":"ls: no such file","error":{"message":"exit 2"},"toolTelemetry":{"restrictedProperties":{"filePaths":"[\"/repo/a.txt\",\"/repo/b.txt\",\"/repo/a.txt\"]"}}},"id":"9","timestamp":"2026-03-03T00:00:07Z","parentId":"8"}`,
		`{"type":"assistant.message","data":{"messageId":"m2","content":"Created hello.txt.","model":"claude-sonnet-4.6","toolRequests":[],"outputTokens":8},"id":"10","timestamp":"2026-03-03T00:00:08Z","parentId":"9"}`,
		`{"type":"assistant.turn_end","data":{"turnId":"0"},"id":"11","timestamp":"2026-03-03T00:00:09Z","parentId":"10"}`,
	)
	r := reader{}
	pos, err := r.Position(path)
	if err != nil || pos != 11 {
		t.Fatalf("pos=%d err=%v", pos, err)
	}
	r.Settle(context.Background(), path)
	turn, newPos, err := r.Read(path, 2)
	if err != nil || newPos != 11 {
		t.Fatalf("pos=%d err=%v", newPos, err)
	}
	if len(turn.Prompts) != 1 || turn.Prompts[0] != "Create hello.txt" {
		t.Fatalf("%+v", turn.Prompts)
	}
	if turn.Model != "claude-sonnet-4.6" {
		t.Fatalf("model %q: the answering message beats model_change's auto", turn.Model)
	}
	if !turn.HasUsage || turn.Usage.Output != 20 || turn.Usage.Input != 0 {
		t.Fatalf("%+v", turn.Usage)
	}
	it := turn.Items
	if len(it) != 4 {
		t.Fatalf("%d items: %+v", len(it), it)
	}
	if it[0].Tool != nil || it[0].ID != "m1" || it[0].Mode != "text" || it[0].Body != "I'll create that file." {
		t.Fatalf("%+v", it[0])
	}
	tc1 := it[1].Tool
	if tc1 == nil || tc1.NativeID != "tc1" || tc1.Name != "create" || tc1.Status != "completed" ||
		string(tc1.Arguments) != `{"path":"/repo/hello.txt","content":"hi"}` ||
		tc1.Result == nil || *tc1.Result != `{"content":"Created."}` ||
		len(tc1.Locations) != 1 || tc1.Locations[0] != "/repo/hello.txt" {
		t.Fatalf("%+v", tc1)
	}
	tc2 := it[2].Tool
	if tc2 == nil || tc2.NativeID != "tc2" || tc2.Name != "bash" || tc2.Status != "failed" ||
		tc2.Result == nil || *tc2.Result != "ls: no such file" ||
		len(tc2.Locations) != 2 || tc2.Locations[0] != "/repo/a.txt" || tc2.Locations[1] != "/repo/b.txt" {
		t.Fatalf("%+v", tc2)
	}
	if it[3].Tool != nil || it[3].ID != "m2" || it[3].Body != "Created hello.txt." {
		t.Fatalf("%+v", it[3])
	}
}

func TestReadWithoutUsageOrStart(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session.model_change","data":{"newModel":"gpt-5"},"id":"1"}`,
		`{"type":"user.message","data":{"content":"  "},"id":"2"}`,
		`not json`,
		``,
		`{"type":"tool.execution_complete","data":{"toolCallId":"tc9","toolName":"edit","toolTelemetry":{"properties":{"filePaths":"not-json"}}},"id":"3"}`,
		`{"type":"assistant.message","data":{"content":"Done"},"id":"m-no-id"}`,
	)
	turn, pos, err := reader{}.Read(path, 0)
	if err != nil || pos != 6 {
		t.Fatalf("pos=%d err=%v", pos, err)
	}
	if turn.HasUsage || turn.Usage.Output != 0 {
		t.Fatalf("no outputTokens means no usage: %+v", turn.Usage)
	}
	if turn.Model != "gpt-5" || len(turn.Prompts) != 0 {
		t.Fatalf("%+v", turn)
	}
	if len(turn.Items) != 2 {
		t.Fatalf("%+v", turn.Items)
	}
	tool := turn.Items[0].Tool
	if tool == nil || tool.NativeID != "tc9" || tool.Name != "edit" || tool.Status != "completed" || tool.Result != nil || tool.Locations != nil {
		t.Fatalf("a completion without its start still counts: %+v", tool)
	}
	if turn.Items[1].ID != "m-no-id" || turn.Items[1].Body != "Done" {
		t.Fatalf("%+v", turn.Items[1])
	}
}

// The shape Copilot CLI 1.0.95 writes: usage in session.usage_record before
// each message, messages with no outputTokens, and the written files in the
// completion's fileEdits as well as restrictedProperties.filePaths.
func TestReadCurrentReleaseShape(t *testing.T) {
	path := writeTranscript(t,
		`{"type":"session.start","data":{"sessionId":"s1","version":1,"copilotVersion":"1.0.95"},"id":"1"}`,
		`{"type":"session.model_change","data":{"source":"startup","newModel":"auto"},"id":"2"}`,
		`{"type":"session.auto_mode_resolved","data":{"chosenModel":"gpt-5.6-luna"},"id":"3"}`,
		`{"type":"user.message","data":{"content":"Create a file named note.txt containing the word hello"},"id":"4"}`,
		`{"type":"assistant.turn_start","data":{"turnId":"0"},"id":"5"}`,
		`{"type":"session.usage_record","data":{"usage":{"model":"gpt-5.6-luna","inputTokens":15869,"outputTokens":29,"cacheReadTokens":15772,"cacheWriteTokens":94,"reasoningTokens":0}},"id":"6"}`,
		`{"type":"assistant.message","data":{"messageId":"m1","model":"gpt-5.6-luna","toolRequests":[{"toolCallId":"custom_call_1","name":"apply_patch"}],"turnId":"0"},"id":"7"}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"custom_call_1","toolName":"apply_patch","arguments":"*** Begin Patch\n*** Add File: note.txt\n+hello\n*** End Patch\n","turnId":"0","model":"gpt-5.6-luna"},"id":"8"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"custom_call_1","model":"gpt-5.6-luna","turnId":"0","fileEdits":[{"path":"/repo/note.txt","kind":"create"}],"success":true,"result":{"content":"Added 1 file(s): /repo/note.txt","detailedContent":"diff …"},"toolTelemetry":{"properties":{"fileExtension":"[\".txt\"]"},"restrictedProperties":{"filePaths":"[\"/repo/note.txt\"]"}}},"id":"9"}`,
		`{"type":"assistant.turn_end","data":{"turnId":"0"},"id":"10"}`,
		`{"type":"session.usage_record","data":{"usage":{"model":"gpt-5.6-luna","inputTokens":15925,"outputTokens":17,"cacheReadTokens":15866,"cacheWriteTokens":56,"reasoningTokens":3}},"id":"11"}`,
		`{"type":"assistant.message","data":{"messageId":"m2","model":"gpt-5.6-luna","content":"Created note.txt.","toolRequests":[],"phase":"final_answer"},"id":"12"}`,
	)
	turn, pos, err := reader{}.Read(path, 3)
	if err != nil || pos != 12 {
		t.Fatalf("pos=%d err=%v", pos, err)
	}
	if !turn.HasUsage || turn.Usage.Input != 31794 || turn.Usage.Output != 46 ||
		turn.Usage.CacheRead != 31638 || turn.Usage.CacheCreation != 150 || turn.Usage.Reasoning != 3 {
		t.Fatalf("usage_record totals: %+v", turn.Usage)
	}
	if turn.Model != "gpt-5.6-luna" {
		t.Fatalf("model %q", turn.Model)
	}
	if len(turn.Prompts) != 1 || len(turn.Items) != 2 {
		t.Fatalf("%+v %+v", turn.Prompts, turn.Items)
	}
	tool := turn.Items[0].Tool
	if tool == nil || tool.Name != "apply_patch" || tool.Status != "completed" ||
		len(tool.Locations) != 1 || tool.Locations[0] != "/repo/note.txt" ||
		tool.Result == nil || !strings.HasPrefix(*tool.Result, `{"content":"Added 1 file(s)`) {
		t.Fatalf("%+v", tool)
	}
	if turn.Items[1].Tool != nil || turn.Items[1].Body != "Created note.txt." {
		t.Fatalf("an empty-content message is no item; the final one is: %+v", turn.Items[1])
	}
}

func TestFileEditsWinOverTelemetry(t *testing.T) {
	te := toolEvent{FileEdits: []fileEdit{{Path: "/a"}, {Path: ""}, {Path: "/a"}, {Path: "/b"}}}
	te.Telemetry.RestrictedProperties.FilePaths = []byte(`"[\"/c\"]"`)
	if got := te.files(); len(got) != 2 || got[0] != "/a" || got[1] != "/b" {
		t.Fatalf("%v", got)
	}
	te.FileEdits = nil
	if got := te.files(); len(got) != 1 || got[0] != "/c" {
		t.Fatalf("%v", got)
	}
}
