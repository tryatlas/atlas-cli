package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// fixture is a two-turn rollout. Turn two starts at line 10.
var fixture = []string{
	`{"timestamp":"2026-10-12T10:00:00.000Z","type":"session_meta","payload":{"id":"s1","timestamp":"2026-10-12T10:00:00.000Z","cwd":"/repo","originator":"codex_cli_rs","cli_version":"0.153.4","source":"cli"}}`,
	`{"timestamp":"2026-10-12T10:00:00.100Z","type":"turn_context","payload":{"cwd":"/repo","approval_policy":"on-request","model":"gpt-5-codex","effort":"medium"}}`,
	`{"timestamp":"2026-10-12T10:00:00.200Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<recommended_plugins>\nHere is a list of plugins.\n</recommended_plugins>\n<environment_context>\n  <cwd>/repo</cwd>\n</environment_context>"}]}}`,
	`{"timestamp":"2026-10-12T10:00:00.300Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Create hello.txt"}]}}`,
	`{"timestamp":"2026-10-12T10:00:01.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":10,"reasoning_output_tokens":2,"total_tokens":110},"last_token_usage":{"input_tokens":100,"cached_input_tokens":20,"output_tokens":10,"reasoning_output_tokens":2,"total_tokens":110},"model_context_window":272000},"rate_limits":null}}`,
	`{"timestamp":"2026-10-12T10:00:02.000Z","type":"response_item","payload":{"type":"reasoning","summary":[{"type":"summary_text","text":"**Planning the file**"}],"content":null,"encrypted_content":"SECRET-BLOB"}}`,
	`{"timestamp":"2026-10-12T10:00:03.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch\n*** Add File: hello.txt\n+hi\n*** End Patch"}}`,
	`{"timestamp":"2026-10-12T10:00:03.500Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_1","output":"Success. Updated the following files:\nA hello.txt\n"}}`,
	`{"timestamp":"2026-10-12T10:00:04.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Created hello.txt."}]}}`,
	`{"timestamp":"2026-10-12T10:00:04.500Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":300,"cached_input_tokens":120,"cache_write_input_tokens":5,"output_tokens":40,"reasoning_output_tokens":8,"total_tokens":340}},"rate_limits":null}}`,
	`{"timestamp":"2026-10-12T10:01:00.000Z","type":"turn_context","payload":{"cwd":"/repo","model":"gpt-5.1-codex","effort":"high"}}`,
	`{"timestamp":"2026-10-12T10:01:00.100Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Now list files"}]}}`,
	`{"timestamp":"2026-10-12T10:01:01.000Z","type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\"command\":[\"bash\",\"-lc\",\"ls\"],\"workdir\":\"/repo\"}","call_id":"call_2"}}`,
	`{"timestamp":"2026-10-12T10:01:01.500Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_2","output":"hello.txt\n"}}`,
	`{"timestamp":"2026-10-12T10:01:02.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"The directory holds hello.txt."}]}}`,
	`{"timestamp":"2026-10-12T10:01:02.500Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":500,"cached_input_tokens":300,"cache_write_input_tokens":25,"output_tokens":70,"reasoning_output_tokens":20,"total_tokens":570}},"rate_limits":null}}`,
}

func writeFixture(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rollout-2026-10-12T10-00-00-s1.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(fixture, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadSecondTurn(t *testing.T) {
	p := writeFixture(t)
	r := reader{}
	pos, err := r.Position(p)
	if err != nil || pos != len(fixture) {
		t.Fatalf("pos %d err %v", pos, err)
	}
	turn, newPos, err := r.Read(p, 10)
	if err != nil || newPos != len(fixture) {
		t.Fatalf("pos %d err %v", newPos, err)
	}
	if len(turn.Prompts) != 1 || turn.Prompts[0] != "Now list files" {
		t.Fatalf("prompts %v", turn.Prompts)
	}
	if turn.Model != "gpt-5.1-codex" {
		t.Fatalf("model %q", turn.Model)
	}
	if len(turn.Items) != 2 {
		t.Fatalf("items %+v", turn.Items)
	}
	tool := turn.Items[0].Tool
	if tool == nil || tool.NativeID != "call_2" || tool.Name != "shell" || tool.Status != "completed" {
		t.Fatalf("tool %+v", tool)
	}
	if tool.Result == nil || *tool.Result != "hello.txt\n" {
		t.Fatalf("result %v", tool.Result)
	}
	var args map[string]any
	if err := json.Unmarshal(tool.Arguments, &args); err != nil || args["workdir"] != "/repo" {
		t.Fatalf("arguments %s", tool.Arguments)
	}
	if record.CanonicalToolName(tool.Name, tool.Title, tool.Kind, tool.Arguments) != "Bash" {
		t.Fatal("shell should canonicalise to Bash")
	}
	if turn.Items[1].Mode != "text" || turn.Items[1].Body != "The directory holds hello.txt." || turn.Items[1].ID == "" {
		t.Fatalf("text %+v", turn.Items[1])
	}
	// Delta from the baseline at line 9 (300/120/40/8) to the last snapshot
	// (500/300/70/20): input 200 of which 180 cached.
	if !turn.HasUsage || turn.Usage.Input != 20 || turn.Usage.CacheRead != 180 || turn.Usage.Output != 30 || turn.Usage.Reasoning != 12 || turn.Usage.CacheCreation != 20 {
		t.Fatalf("usage %+v", turn.Usage)
	}
}

func TestReadWholeFile(t *testing.T) {
	p := writeFixture(t)
	turn, _, err := reader{}.Read(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(turn.Prompts, "|") != "Create hello.txt|Now list files" {
		t.Fatalf("prompts %v", turn.Prompts)
	}
	if len(turn.Items) != 5 {
		t.Fatalf("items %d: %+v", len(turn.Items), turn.Items)
	}
	th := turn.Items[0]
	if th.Mode != "thinking" || th.Body != "**Planning the file**" || strings.Contains(th.Body, "SECRET") {
		t.Fatalf("thinking %+v", th)
	}
	tool := turn.Items[1].Tool
	if tool == nil || tool.Name != "apply_patch" || tool.NativeID != "call_1" {
		t.Fatalf("tool %+v", tool)
	}
	if len(tool.Locations) != 1 || tool.Locations[0] != "hello.txt" {
		t.Fatalf("locations %v", tool.Locations)
	}
	var args map[string]string
	if err := json.Unmarshal(tool.Arguments, &args); err != nil || !strings.HasPrefix(args["patch"], "*** Begin Patch") {
		t.Fatalf("arguments %s", tool.Arguments)
	}
	if tool.Result == nil || !strings.HasPrefix(*tool.Result, "Success.") {
		t.Fatalf("result %v", tool.Result)
	}
	// The recorder sees an Edit of hello.txt with the patch as its diff.
	if record.CanonicalToolName(tool.Name, "", "", tool.Arguments) != "Edit" {
		t.Fatal("apply_patch should canonicalise to Edit")
	}
	if paths := record.WritePaths(tool.Arguments, tool.Locations); len(paths) != 1 || paths[0] != "hello.txt" {
		t.Fatalf("write paths %v", paths)
	}
	if record.EditPatch(tool.Arguments, "hello.txt") != args["patch"] {
		t.Fatal("the patch should be the edit")
	}
	if turn.Items[2].Body != "Created hello.txt." || turn.Items[3].Tool == nil || turn.Items[4].Body != "The directory holds hello.txt." {
		t.Fatalf("order %+v", turn.Items)
	}
	if turn.Model != "gpt-5.1-codex" {
		t.Fatalf("model %q", turn.Model)
	}
	// No baseline: the last snapshot is the total.
	if !turn.HasUsage || turn.Usage.Input != 200 || turn.Usage.CacheRead != 300 || turn.Usage.Output != 70 || turn.Usage.Reasoning != 20 || turn.Usage.CacheCreation != 25 {
		t.Fatalf("usage %+v", turn.Usage)
	}
	for _, raw := range fixture {
		if strings.Contains(raw, "SECRET-BLOB") {
			b, _ := json.Marshal(turn)
			if strings.Contains(string(b), "SECRET-BLOB") {
				t.Fatal("encrypted content leaked into the turn")
			}
		}
	}
}

func TestReadNoNewSnapshot(t *testing.T) {
	p := writeFixture(t)
	// From the end: nothing new, so no usage is claimed.
	turn, pos, err := reader{}.Read(p, len(fixture))
	if err != nil || pos != len(fixture) || turn.HasUsage || len(turn.Items) != 0 || len(turn.Prompts) != 0 {
		t.Fatalf("%+v pos %d err %v", turn, pos, err)
	}
}

func TestDroppedPrompt(t *testing.T) {
	dropped := []string{
		"<environment_context>\n  <cwd>/repo</cwd>\n</environment_context>",
		"<recommended_plugins>\nnone\n</recommended_plugins>\n<environment_context>\n</environment_context>\n",
		"<user_instructions>\nBe terse.\n</user_instructions>",
		"<turn_aborted>The user interrupted.</turn_aborted>",
		"# AGENTS.md instructions for /repo\n\nDo things.",
	}
	for _, s := range dropped {
		if !droppedPrompt.MatchString(s) {
			t.Errorf("should drop %q", s)
		}
	}
	kept := []string{
		"say hi",
		"Fix <b>bold</b> rendering",
		"<div>what does this do",
		"why does </html> appear twice",
	}
	for _, s := range kept {
		if droppedPrompt.MatchString(s) {
			t.Errorf("should keep %q", s)
		}
	}
}

func TestOutputText(t *testing.T) {
	cases := map[string]string{
		`"plain"`:             "plain",
		`{"content":"inner"}`: "inner",
		`{"content":[{"type":"output_text","text":"a"},{"type":"output_text","text":"b"}]}`: "a\nb",
		`[{"type":"input_text","text":"x"}]`:                                                "x",
		`{"other":1}`:                                                                       `{"other":1}`,
		``:                                                                                  "",
	}
	for in, want := range cases {
		if got := outputText(json.RawMessage(in)); got != want {
			t.Errorf("%s: got %q want %q", in, got, want)
		}
	}
}

func TestSettleReturns(t *testing.T) {
	p := writeFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	reader{}.Settle(ctx, p)
	if time.Since(start) > 3*time.Second {
		t.Fatal("settle took too long on a quiet file")
	}
}

// TestLiveRollout reads a real Codex rollout when ATLAS_LIVE=1: the file
// named by ATLAS_CODEX_ROLLOUT, else the newest under $CODEX_HOME/sessions.
func TestLiveRollout(t *testing.T) {
	if os.Getenv("ATLAS_LIVE") != "1" {
		t.Skip("set ATLAS_LIVE=1 to read a real rollout")
	}
	p := os.Getenv("ATLAS_CODEX_ROLLOUT")
	if p == "" {
		home := os.Getenv("CODEX_HOME")
		if home == "" {
			h, _ := os.UserHomeDir()
			home = filepath.Join(h, ".codex")
		}
		var newest string
		var newestAt time.Time
		_ = filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasPrefix(d.Name(), "rollout-") {
				return nil
			}
			if fi, err := d.Info(); err == nil && fi.ModTime().After(newestAt) {
				newest, newestAt = path, fi.ModTime()
			}
			return nil
		})
		p = newest
	}
	if p == "" {
		t.Skip("no rollout found")
	}
	t.Logf("rollout: %s", p)
	lines, n, err := hooks.LinesFrom(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	payloadTypes := map[string]int{}
	for _, raw := range lines {
		var l line
		if json.Unmarshal(raw, &l) != nil {
			continue
		}
		types[l.Type]++
		var pt struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(l.Payload, &pt) == nil && pt.Type != "" {
			k := l.Type + "/" + pt.Type
			if pt.Role != "" {
				k += ":" + pt.Role
			}
			payloadTypes[k]++
		}
	}
	keys := make([]string, 0, len(payloadTypes))
	for k := range payloadTypes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	t.Logf("%d lines, line types %v", n, types)
	for _, k := range keys {
		t.Logf("  %-45s %d", k, payloadTypes[k])
	}
	turn, pos, err := reader{}.Read(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("pos=%d model=%q prompts=%q usage=%+v hasUsage=%v", pos, turn.Model, turn.Prompts, turn.Usage, turn.HasUsage)
	for i, it := range turn.Items {
		if it.Tool != nil {
			res := ""
			if it.Tool.Result != nil {
				res = *it.Tool.Result
			}
			t.Logf("  item %d tool %s %s args=%s locations=%v result=%.80q", i, it.Tool.Name, it.Tool.NativeID, it.Tool.Arguments, it.Tool.Locations, res)
		} else {
			t.Logf("  item %d %s %s %.120q", i, it.Mode, it.ID, it.Body)
		}
	}
	if b, _ := json.Marshal(turn); strings.Contains(string(b), "encrypted_content") {
		t.Fatal("encrypted content leaked into the turn")
	}
}

// TestReadCodeMode covers Codex 0.153's code mode, where the rollout holds
// an exec custom_tool_call whose JavaScript calls tools.apply_patch.
func TestReadCodeMode(t *testing.T) {
	lines := []string{
		fixture[0],
		`{"timestamp":"2026-10-12T02:39:27.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_Sn9","name":"exec","input":"const r = await tools.apply_patch(\"*** Begin Patch\\n*** Add File: /private/tmp/hooks-codex/note2.txt\\n+hello\\n*** End Patch\"); text(typeof r === \"string\" ? r : JSON.stringify(r));"}}`,
		`{"timestamp":"2026-10-12T02:39:28.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_Sn9","output":[{"type":"input_text","text":"Script completed\nWall time 0.6 seconds\nOutput:\n"},{"type":"input_text","text":"{}"}]}}`,
		`{"timestamp":"2026-10-12T02:39:29.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_mix","name":"exec","input":"await tools.shell({command: 'go test'}); await tools.apply_patch(\"*** Begin Patch\\n*** Update File: a.go\\n*** End Patch\");"}}`,
		`{"timestamp":"2026-10-12T02:39:30.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_mix","output":"ok"}}`,
		`{"timestamp":"2026-10-12T02:39:31.000Z","type":"response_item","payload":{"type":"custom_tool_call","status":"completed","call_id":"call_cmd","name":"exec","input":"const r = await tools.exec_command({\"cmd\":\"cat rt.txt\",\"workdir\":\"/private/tmp/hooks-codex\",\"yield_time_ms\":10000,\"max_output_tokens\":2000});\ntext(r.output);\n"}}`,
		`{"timestamp":"2026-10-12T02:39:32.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_cmd","output":[{"type":"input_text","text":"Script completed\nWall time 0.2 seconds\nOutput:\n"},{"type":"input_text","text":"realtime\n"}]}}`,
	}
	p := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	turn, _, err := reader{}.Read(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Items) != 4 {
		t.Fatalf("items %d: %+v", len(turn.Items), turn.Items)
	}
	// A script that only edits is the edit, with the script's output.
	edit := turn.Items[0].Tool
	if edit == nil || edit.Name != "apply_patch" || edit.NativeID != "call_Sn9" || len(edit.Locations) != 1 || edit.Locations[0] != "/private/tmp/hooks-codex/note2.txt" {
		t.Fatalf("edit %+v", edit)
	}
	if record.CanonicalToolName(edit.Name, "", "", edit.Arguments) != "Edit" {
		t.Fatal("code-mode apply_patch should canonicalise to Edit")
	}
	if edit.Result == nil || !strings.HasPrefix(*edit.Result, "Script completed") {
		t.Fatalf("result %v", edit.Result)
	}
	// A mixed script keeps the exec call (with its output) and adds the edit.
	ex, ed := turn.Items[1].Tool, turn.Items[2].Tool
	if ex == nil || ex.Name != "exec" || ex.NativeID != "call_mix" || ex.Result == nil || *ex.Result != "ok" {
		t.Fatalf("exec %+v", ex)
	}
	if ed == nil || ed.Name != "apply_patch" || ed.NativeID != "call_mix#patch1" || len(ed.Locations) != 1 || ed.Locations[0] != "a.go" || ed.Result != nil {
		t.Fatalf("patch %+v", ed)
	}
	// A script that only runs one command is that command, with cmd at the
	// top level (the hook reports the same text as command) and the output.
	cmd := turn.Items[3].Tool
	if cmd == nil || cmd.Name != "exec_command" || cmd.NativeID != "call_cmd" || !strings.Contains(string(cmd.Arguments), `"cmd":"cat rt.txt"`) || cmd.Result == nil || !strings.HasSuffix(*cmd.Result, "realtime\n") {
		t.Fatalf("command %+v", cmd)
	}
	if record.CanonicalToolName(cmd.Name, "", "", cmd.Arguments) != "Bash" {
		t.Fatal("exec_command should canonicalise to Bash")
	}
}
