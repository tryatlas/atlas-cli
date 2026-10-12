package pi

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/cli/internal/hooks"
)

func TestInstallStateUninstall(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	if got := a.State(root); got != hooks.Absent {
		t.Fatalf("state before install = %v", got)
	}
	changed, err := a.Install(root, false)
	if err != nil || !changed {
		t.Fatalf("install: changed=%v err=%v", changed, err)
	}
	b, err := os.ReadFile(ExtensionPath(root))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "// Atlas extension for Pi — written by atlas agents add; do not edit (rewritten on the next add).\n") {
		t.Fatalf("header missing:\n%s", b[:120])
	}
	for _, want := range []string{"exec atlas-cli hooks pi ${verb}", `"session-start"`, `"before-agent-start"`, `"tool-result"`, `"agent-end"`, `pi.on("tool_result"`, "parentToolCallId", "64 * 1024", "let queue", "ATLAS_PI_NESTED", "command -v atlas", "10_000"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("extension lacks %q", want)
		}
	}
	if strings.Contains(string(b), `"session_shutdown"`) {
		t.Error("extension forwards session_shutdown")
	}
	if fi, _ := os.Stat(ExtensionPath(root)); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if got := a.State(root); got != hooks.Current {
		t.Fatalf("state after install = %v", got)
	}
	// Idempotent.
	if changed, err := a.Install(root, false); err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}
	// An older render: ours, but outdated; rewritten without force.
	if err := os.WriteFile(ExtensionPath(root), []byte("// Atlas extension for Pi — written by atlas agents add; old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := a.State(root); got != hooks.Outdated {
		t.Fatalf("state with old render = %v", got)
	}
	if changed, err := a.Install(root, false); err != nil || !changed {
		t.Fatalf("reinstall over old render: changed=%v err=%v", changed, err)
	}
	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ExtensionDir(root)); !os.IsNotExist(err) {
		t.Fatalf("extension dir remains: %v", err)
	}
	if got := a.State(root); got != hooks.Absent {
		t.Fatalf("state after uninstall = %v", got)
	}
}

func TestInstallRefusesForeignFile(t *testing.T) {
	root := t.TempDir()
	a := Agent{}
	foreign := []byte("export default function (pi) { /* mine */ }\n")
	if err := os.MkdirAll(ExtensionDir(root), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ExtensionPath(root), foreign, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := a.State(root); got != hooks.Absent {
		t.Fatalf("state with foreign file = %v", got)
	}
	if _, err := a.Install(root, false); err == nil {
		t.Fatal("install over a foreign file succeeded")
	}
	if b, _ := os.ReadFile(ExtensionPath(root)); string(b) != string(foreign) {
		t.Fatal("foreign file was changed")
	}
	// Uninstall keeps it too.
	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(ExtensionPath(root)); string(b) != string(foreign) {
		t.Fatal("uninstall removed a foreign file")
	}
	changed, err := a.Install(root, true)
	if err != nil || !changed {
		t.Fatalf("forced install: changed=%v err=%v", changed, err)
	}
	if got := a.State(root); got != hooks.Current {
		t.Fatalf("state after forced install = %v", got)
	}
}

func TestParse(t *testing.T) {
	a := Agent{}
	const file = "/home/u/.pi/agent/sessions/--repo--/2026-10-12T10-00-00.000Z_3f2b1c4d-aaaa-bbbb-cccc-0123456789ab.jsonl"
	ev, err := a.Parse(VerbSessionStart, []byte(`{"type":"session_start","cwd":"/repo","session_file":"`+file+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != hooks.SessionStart || ev.SessionID != "3f2b1c4d-aaaa-bbbb-cccc-0123456789ab" || ev.Transcript != file || ev.Cwd != "/repo" {
		t.Fatalf("session-start = %+v", ev)
	}
	ev, err = a.Parse(VerbBeforeAgentStart, []byte(`{"type":"before_agent_start","cwd":"/repo","session_file":"`+file+`","prompt":"fix the bug"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != hooks.TurnStart || ev.Prompt != "fix the bug" {
		t.Fatalf("before-agent-start = %+v", ev)
	}
	ev, err = a.Parse(VerbAgentEnd, []byte(`{"type":"agent_end","cwd":"/repo","session_file":"`+file+`","session_id":"explicit"}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != hooks.TurnEnd || ev.SessionID != "explicit" {
		t.Fatalf("agent-end = %+v", ev)
	}
	ev, err = a.Parse(VerbToolResult, []byte(`{"type":"tool_result","cwd":"/repo","session_file":"`+file+`","tool_call_id":"call_1","tool_name":"write","args":{"path":"rt.txt","content":"realtime\n"},"result":"Successfully wrote 9 bytes to rt.txt","is_error":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != hooks.ToolUse || ev.Tool == nil || ev.SessionID != "3f2b1c4d-aaaa-bbbb-cccc-0123456789ab" {
		t.Fatalf("tool-result = %+v", ev)
	}
	if ev.Tool.NativeID != "call_1" || ev.Tool.Name != "write" || ev.Tool.Status != "completed" || string(ev.Tool.Arguments) != `{"path":"rt.txt","content":"realtime\n"}` {
		t.Fatalf("tool = %+v", ev.Tool)
	}
	if ev.Tool.Result == nil || *ev.Tool.Result != "Successfully wrote 9 bytes to rt.txt" {
		t.Fatalf("tool result = %v", ev.Tool.Result)
	}
	ev, err = a.Parse(VerbToolResult, []byte(`{"type":"tool_result","session_file":"`+file+`","tool_call_id":"call_2","tool_name":"bash","args":{"command":"cat missing"},"result":"cat: missing: No such file","is_error":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Tool == nil || ev.Tool.Status != "failed" || ev.Tool.NativeID != "call_2" {
		t.Fatalf("failed tool-result = %+v", ev.Tool)
	}
	// A result payload with no call to speak of is dropped.
	if ev, err := a.Parse(VerbToolResult, []byte(`{"type":"tool_result","session_file":"`+file+`","tool_name":"bash"}`)); err != nil || ev != nil {
		t.Fatalf("callless tool-result = %+v, %v", ev, err)
	}
	// No session: nothing to track (pi --no-session).
	if ev, err := a.Parse(VerbAgentEnd, []byte(`{"type":"agent_end","cwd":"/repo"}`)); err != nil || ev != nil {
		t.Fatalf("sessionless = %+v, %v", ev, err)
	}
	if ev, err := a.Parse("session-shutdown", []byte(`{"session_file":"`+file+`"}`)); err != nil || ev != nil {
		t.Fatalf("unknown verb = %+v, %v", ev, err)
	}
	if _, err := a.Parse(VerbAgentEnd, []byte(`{`)); err == nil {
		t.Fatal("bad JSON accepted")
	}
	if got := SessionIDFromPath("/x/plain.jsonl"); got != "plain" {
		t.Errorf("plain name = %q", got)
	}
	if got := a.ResumeCommand("abc"); got != "pi --session abc" {
		t.Errorf("resume = %q", got)
	}
	if a.Response(hooks.SessionStart, "hi") != nil {
		t.Error("response should be nil")
	}
}

// forkFixture is a session where the user forked after the first prompt:
// m2..m4 are the abandoned branch, m5..m8 the active one.
const forkFixture = `{"type":"session","id":"s1","parentId":null,"version":3,"cwd":"/repo"}
{"type":"model_change","id":"mc1","parentId":"s1","provider":"anthropic","modelId":"claude-sonnet-4-6"}
{"type":"message","id":"m1","parentId":"mc1","message":{"role":"user","content":[{"type":"text","text":"Create a file"}]}}
{"type":"message","id":"m2","parentId":"m1","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"toolCall","id":"tc1","name":"write","arguments":{"path":"old.txt","content":"old\n"}}],"usage":{"input":1000,"output":500,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"m3","parentId":"m2","message":{"role":"toolResult","toolCallId":"tc1","toolName":"write","content":[{"type":"text","text":"Written 4 bytes"}],"isError":false}}
{"type":"message","id":"m4","parentId":"m3","message":{"role":"assistant","model":"claude-opus-4-8","content":[{"type":"text","text":"Created old.txt"}],"usage":{"input":2000,"output":300,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"m5","parentId":"m1","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"thinking","thinking":"new.txt it is"},{"type":"toolCall","id":"tc2","name":"write","arguments":{"path":"new.txt","content":"new\n"}}],"usage":{"input":150,"output":60,"cacheRead":5,"cacheWrite":3,"reasoning":7}}}
{"type":"message","id":"m6","parentId":"m5","message":{"role":"toolResult","toolCallId":"tc2","toolName":"write","content":[{"type":"text","text":"Written 4 bytes"}],"isError":false}}
{"type":"message","id":"m7","parentId":"m6","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"toolCall","id":"tc3","name":"bash","arguments":{"command":"cat missing"}}],"usage":{"input":250,"output":40,"cacheRead":0,"cacheWrite":0}}}
{"type":"message","id":"m8","parentId":"m7","message":{"role":"toolResult","toolCallId":"tc3","toolName":"bash","content":[{"type":"text","text":"cat: missing: No such file"}],"isError":true}}
{"type":"message","id":"m9","parentId":"m8","message":{"role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"Created new.txt"}],"usage":{"input":260,"output":20,"cacheRead":10,"cacheWrite":0,"reasoning":4}}}
`

func writeFixture(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "2026-10-12T10-00-00.000Z_3f2b1c4d.jsonl")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadActiveBranch(t *testing.T) {
	p := writeFixture(t, forkFixture)
	r := Agent{}.Reader()
	n, err := r.Position(p)
	if err != nil || n != 11 {
		t.Fatalf("position = %d, %v", n, err)
	}
	turn, pos, err := r.Read(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 11 {
		t.Errorf("pos = %d", pos)
	}
	if len(turn.Prompts) != 1 || turn.Prompts[0] != "Create a file" {
		t.Errorf("prompts = %q", turn.Prompts)
	}
	var names []string
	for _, it := range turn.Items {
		if it.Tool != nil {
			names = append(names, it.Tool.Name+":"+it.Tool.NativeID+":"+it.Tool.Status)
		} else {
			names = append(names, it.Mode+":"+it.Body)
		}
	}
	want := []string{"thinking:new.txt it is", "write:tc2:completed", "bash:tc3:failed", "text:Created new.txt"}
	if strings.Join(names, "|") != strings.Join(want, "|") {
		t.Errorf("items = %q\nwant %q", names, want)
	}
	for _, it := range turn.Items {
		if it.Tool == nil {
			continue
		}
		if it.Tool.Result == nil {
			t.Errorf("%s has no result", it.Tool.Name)
			continue
		}
		if it.Tool.Name == "bash" && *it.Tool.Result != "cat: missing: No such file" {
			t.Errorf("bash result = %q", *it.Tool.Result)
		}
		if it.Tool.Name == "write" && !strings.Contains(string(it.Tool.Arguments), `"path":"new.txt"`) {
			t.Errorf("write arguments = %s", it.Tool.Arguments)
		}
	}
	// Usage: only m5, m7, m9 (the abandoned m2/m4 would add 3000 input).
	if !turn.HasUsage || turn.Usage.Input != 660 || turn.Usage.Output != 120 || turn.Usage.CacheRead != 15 || turn.Usage.CacheCreation != 3 || turn.Usage.Reasoning != 11 {
		t.Errorf("usage = %+v has=%v", turn.Usage, turn.HasUsage)
	}
	if turn.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q", turn.Model)
	}
}

func TestReadFromOffset(t *testing.T) {
	p := writeFixture(t, forkFixture)
	r := Agent{}.Reader()
	// From line index 8 (m7 on): the active branch is still resolved over the whole tree.
	turn, _, err := r.Read(p, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(turn.Prompts) != 0 {
		t.Errorf("prompts = %q", turn.Prompts)
	}
	if len(turn.Items) != 2 || turn.Items[0].Tool == nil || turn.Items[0].Tool.Name != "bash" || turn.Items[1].Body != "Created new.txt" {
		t.Errorf("items = %+v", turn.Items)
	}
	if turn.Usage.Input != 510 || turn.Usage.Output != 60 {
		t.Errorf("usage = %+v", turn.Usage)
	}
	if turn.Model != "claude-sonnet-4-6" {
		t.Errorf("model = %q", turn.Model)
	}
}

func TestReadFlatAndStringContent(t *testing.T) {
	p := writeFixture(t, `{"type":"message","id":"m1","parentId":null,"message":{"role":"user","content":"Hi"}}
{"type":"message","id":"m2","parentId":null,"message":{"role":"assistant","model":"gpt-5.5","content":[{"type":"text","text":"Hello"}]}}
`)
	turn, pos, err := Agent{}.Reader().Read(p, 0)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 2 || len(turn.Prompts) != 1 || turn.Prompts[0] != "Hi" || len(turn.Items) != 1 || turn.Items[0].Body != "Hello" {
		t.Errorf("turn = %+v pos=%d", turn, pos)
	}
	if turn.HasUsage {
		t.Error("usage reported without any usage field")
	}
	if turn.Model != "gpt-5.5" {
		t.Errorf("model = %q", turn.Model)
	}
}

// TestLiveSessionFile runs the reader over a real Pi session file: set
// ATLAS_LIVE=1 and ATLAS_LIVE_PI_SESSION to the file a Pi hook payload named.
func TestLiveSessionFile(t *testing.T) {
	if os.Getenv("ATLAS_LIVE") != "1" {
		t.Skip("set ATLAS_LIVE=1 and ATLAS_LIVE_PI_SESSION=<session .jsonl>")
	}
	path := os.Getenv("ATLAS_LIVE_PI_SESSION")
	if path == "" {
		t.Fatal("ATLAS_LIVE_PI_SESSION is not set")
	}
	if id := SessionIDFromPath(path); !hooks.ValidSessionID(id) {
		t.Fatalf("session id from %s = %q", filepath.Base(path), id)
	}
	turn, pos, err := reader{}.Read(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("lines=%d model=%q prompts=%q usage=%+v", pos, turn.Model, turn.Prompts, turn.Usage)
	if turn.Model == "" || len(turn.Prompts) == 0 || !turn.HasUsage {
		t.Errorf("model, prompts or usage missing: model=%q prompts=%d usage=%v", turn.Model, len(turn.Prompts), turn.HasUsage)
	}
	tools, text := 0, 0
	for _, it := range turn.Items {
		if it.Tool != nil {
			tools++
			res := "<nil>"
			if it.Tool.Result != nil {
				res = *it.Tool.Result
			}
			t.Logf("tool %s %s args=%s result=%q", it.Tool.Name, it.Tool.Status, it.Tool.Arguments, res)
			if it.Tool.Result == nil {
				t.Errorf("tool %s has no result", it.Tool.NativeID)
			}
			continue
		}
		text++
		t.Logf("%s %q", it.Mode, it.Body)
	}
	if tools == 0 || text == 0 {
		t.Errorf("items: %d tool calls, %d text runs", tools, text)
	}
}
