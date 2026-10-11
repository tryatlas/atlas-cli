package hooks

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

// fakeAgent is a Claude-shaped agent whose transcript is a plain JSONL of
// {"kind":"prompt"|"text"|"tool","body":…} lines.
type fakeAgent struct{}

func (fakeAgent) ID() string         { return "fake" }
func (fakeAgent) Name() string       { return "Fake" }
func (fakeAgent) Binary() string     { return "fake" }
func (fakeAgent) Verbs() []string    { return []string{"start", "prompt", "stop", "end", "tool"} }
func (fakeAgent) Reader() Reader     { return fakeReader{} }
func (fakeAgent) Detect(string) bool { return true }
func (a fakeAgent) Install(root string, force bool) (bool, error) {
	return a.shaped(root).Install(force)
}
func (a fakeAgent) Uninstall(root string) error    { return a.shaped(root).Uninstall() }
func (a fakeAgent) State(root string) InstallState { return a.shaped(root).State() }
func (fakeAgent) ResumeCommand(id string) string   { return "fake " + id }
func (fakeAgent) Response(k Kind, msg string) []byte {
	if k == SessionStart {
		return []byte(msg)
	}
	return nil
}
func (fakeAgent) shaped(root string) ClaudeShaped {
	return ClaudeShaped{Path: filepath.Join(root, ".fake", "settings.json"), Types: []ClaudeHook{
		{Type: "Start", Command: Wrap(Command("fake", "start"))},
		{Type: "Stop", Command: Wrap(Command("fake", "stop"))},
	}}
}
func (fakeAgent) Parse(verb string, payload []byte) (*Event, error) {
	var p struct {
		Session, Transcript, Prompt string
		Files                       []string
		Tool                        *record.Tool
	}
	_ = json.Unmarshal(payload, &p)
	ev := &Event{SessionID: p.Session, Transcript: p.Transcript, Prompt: p.Prompt, Files: p.Files, Tool: p.Tool}
	switch verb {
	case "start":
		ev.Kind = SessionStart
	case "prompt":
		ev.Kind = TurnStart
	case "stop":
		ev.Kind = TurnEnd
	case "end":
		ev.Kind = SessionEnd
	case "tool":
		ev.Kind = ToolUse
	default:
		return nil, nil
	}
	return ev, nil
}

type fakeReader struct{}

func (fakeReader) Position(path string) (int, error) { return CountLines(path) }
func (fakeReader) Settle(context.Context, string)    {}
func (fakeReader) Read(path string, from int) (*Turn, int, error) {
	lines, pos, err := LinesFrom(path, from)
	if err != nil {
		return nil, from, err
	}
	t := &Turn{}
	for i, l := range lines {
		var e struct{ Kind, Body, Path string }
		_ = json.Unmarshal(l, &e)
		switch e.Kind {
		case "prompt":
			t.Prompts = append(t.Prompts, e.Body)
		case "text":
			t.Items = append(t.Items, Item{ID: "m" + string(rune('a'+i)), Mode: "text", Body: e.Body})
		case "tool":
			args, _ := json.Marshal(map[string]string{"file_path": e.Path, "content": e.Body})
			t.Items = append(t.Items, Item{Tool: &record.Tool{NativeID: "t" + string(rune('a'+i)), Name: "Write", Arguments: args, Status: "completed"}})
		}
	}
	return t, pos, nil
}

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

func enabledStore(t *testing.T, root string) *store.Store {
	t.Helper()
	st, err := store.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.PutBinding(context.Background(), store.Binding{WorkspaceID: root, Root: root, Mode: store.ModeLocal, Enabled: true}, time.Now()); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestWrapperOwnership(t *testing.T) {
	cmd := Wrap(Command("claude-code", "stop"))
	if !strings.Contains(cmd, "exec atlas hooks claude-code stop'") || !Owned(cmd) {
		t.Fatalf("wrap: %s", cmd)
	}
	if !Owned("atlas hooks x y") || Owned("atlas status") || Owned("sh -c 'exec entire hooks x y'") {
		t.Fatal("ownership is by the atlas hooks prefix only")
	}
	if m := WrapWithMessage("atlas hooks a b", MissingMessage); !strings.Contains(m, `printf "%s\n"`) || !Owned(m) {
		t.Fatalf("message wrapper: %s", m)
	}
}

func TestClaudeShapedInstallKeepsUserHooksAndPrunesStale(t *testing.T) {
	root := t.TempDir()
	a := fakeAgent{}
	p := filepath.Join(root, ".fake", "settings.json")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	user := `{"theme":"dark","hooks":{"Stop":[{"matcher":"","hooks":[{"type":"command","command":"echo mine"},{"type":"command","command":"atlas hooks fake old-verb"}]}],"Other":[{"hooks":[{"type":"command","command":"atlas hooks fake gone"}]}]}}`
	_ = os.WriteFile(p, []byte(user), 0o600)
	if a.State(root) != Outdated {
		t.Fatalf("state with stale entries = %v", a.State(root))
	}
	changed, err := a.Install(root, false)
	if err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	for _, want := range []string{`"theme": "dark"`, `echo mine`, `exec atlas hooks fake start'`, `exec atlas hooks fake stop'`} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in\n%s", want, s)
		}
	}
	for _, gone := range []string{"old-verb", "fake gone", `"Other"`, "u003e"} {
		if strings.Contains(s, gone) {
			t.Fatalf("still has %q in\n%s", gone, s)
		}
	}
	if a.State(root) != Current {
		t.Fatalf("state after install = %v", a.State(root))
	}
	if changed, _ := a.Install(root, false); changed {
		t.Fatal("a second install must change nothing")
	}
	if err := a.Uninstall(root); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "atlas hooks") || !strings.Contains(string(b), "echo mine") {
		t.Fatalf("uninstall: %s", b)
	}
	if a.State(root) != Absent {
		t.Fatal("state after uninstall")
	}
}

func TestClaudeShapedRefusesANonObjectFile(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, ".fake", "settings.json")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(`[1,2]`), 0o600)
	a := fakeAgent{}
	if _, err := a.Install(root, false); err == nil {
		t.Fatal("a malformed settings file must not be overwritten")
	}
}

// runHook drives one hook as the command would.
func runHook(t *testing.T, d *Dispatcher, verb string, payload map[string]any) []byte {
	t.Helper()
	a := fakeAgent{}
	b, _ := json.Marshal(payload)
	ev, err := a.Parse(verb, b)
	if err != nil {
		t.Fatal(err)
	}
	out, err := d.Run(context.Background(), a, ev)
	if err != nil {
		t.Fatalf("%s: %v", verb, err)
	}
	return out
}

func TestDispatcherRecordsATurnDirectly(t *testing.T) {
	root := gitRepo(t)
	st := enabledStore(t, root)
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(transcript, []byte(`{"kind":"prompt","body":"earlier turn"}`+"\n"), 0o600)
	d := &Dispatcher{Root: root, Version: "test", Now: time.Now, Deliver: Deliver("test", nil), Banner: "Atlas: on"}
	sess := map[string]any{"Session": "s-1", "Transcript": transcript}

	if out := runHook(t, d, "start", sess); string(out) != "Atlas: on" {
		t.Fatalf("banner = %q", out)
	}
	runHook(t, d, "prompt", map[string]any{"Session": "s-1", "Transcript": transcript, "Prompt": "write a file"})
	stt, _ := LoadState(root, "fake", "s-1")
	if stt == nil || !stt.InTurn || stt.Offset != 1 || stt.Prompt != "write a file" {
		t.Fatalf("state after prompt: %+v", stt)
	}
	// The agent works: a tool writes one file, a shell command another.
	_ = os.WriteFile(filepath.Join(root, "new.txt"), []byte("hello\n"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "by-shell.txt"), []byte("x\n"), 0o644)
	f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"kind":"prompt","body":"write a file"}` + "\n" +
		`{"kind":"text","body":"Sure."}` + "\n" +
		`{"kind":"tool","path":"` + filepath.Join(root, "new.txt") + `","body":"hello\n"}` + "\n" +
		`{"kind":"text","body":"Done. token sk-abcdefghijklmnopqrstuvwxyz012345"}` + "\n")
	_ = f.Close()
	runHook(t, d, "stop", sess)
	stt, _ = LoadState(root, "fake", "s-1")
	if stt.InTurn || stt.Turn != 1 || stt.Offset != 5 {
		t.Fatalf("state after stop: %+v", stt)
	}

	db := st.DB()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM agent_message WHERE role='user' AND body='write a file'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("prompt rows = %d %v", n, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM agent_message WHERE role='assistant'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("assistant rows = %d", n)
	}
	var body string
	_ = db.QueryRow(`SELECT body FROM agent_message WHERE role='assistant' ORDER BY seq DESC LIMIT 1`).Scan(&body)
	if strings.Contains(body, "sk-abcdef") {
		t.Fatalf("secret stored: %s", body)
	}
	var tool, status string
	if err := db.QueryRow(`SELECT tool_name, status FROM tool_call WHERE native_call_id='tc'`).Scan(&tool, &status); err != nil || tool != "Write" || status != "completed" {
		t.Fatalf("tool call: %s %s %v", tool, status, err)
	}
	rows, err := db.Query(`SELECT path, existed_before FROM file_touch ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var touched []string
	for rows.Next() {
		var p string
		var existed bool
		_ = rows.Scan(&p, &existed)
		touched = append(touched, p)
	}
	if strings.Join(touched, ",") != "by-shell.txt,new.txt" {
		t.Fatalf("touches = %v", touched)
	}
	var turns int
	if err := db.QueryRow(`SELECT count(*) FROM turn WHERE ended_at IS NOT NULL`).Scan(&turns); err != nil || turns != 1 {
		t.Fatalf("completed turns = %d %v", turns, err)
	}

	runHook(t, d, "end", sess)
	stt, _ = LoadState(root, "fake", "s-1")
	if stt == nil || !stt.Ended {
		t.Fatalf("state after end: %+v", stt)
	}
	states, _ := ListStates(root, "")
	if len(states) != 1 {
		t.Fatalf("states = %d", len(states))
	}
	Sweep(root, time.Now().Add(StaleAfter+time.Hour))
	if states, _ = ListStates(root, ""); len(states) != 0 {
		t.Fatal("sweep must retire the stale state")
	}
}

func TestPerEventDeliveryOpensAndClosesOneTurn(t *testing.T) {
	root := gitRepo(t)
	st := enabledStore(t, root)
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(transcript, nil, 0o600)
	d := &Dispatcher{Root: root, Version: "test", Now: time.Now, Deliver: Deliver("test", nil)}
	sess := map[string]any{"Session": "s-4", "Transcript": transcript}
	runHook(t, d, "start", sess)
	runHook(t, d, "prompt", map[string]any{"Session": "s-4", "Transcript": transcript, "Prompt": "make a file"})
	db := st.DB()
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM turn WHERE state='open'`).Scan(&n)
	if n != 1 {
		t.Fatalf("open turns after the prompt = %d, want 1", n)
	}
	_ = db.QueryRow(`SELECT count(*) FROM agent_message WHERE role='user'`).Scan(&n)
	if n != 1 {
		t.Fatalf("prompt rows = %d", n)
	}
	// The post-tool hook reports the write as it completes.
	_ = os.WriteFile(filepath.Join(root, "made.txt"), []byte("x\n"), 0o644)
	args, _ := json.Marshal(map[string]string{"file_path": filepath.Join(root, "made.txt"), "content": "x\n"})
	tool := &record.Tool{NativeID: "tc", Name: "Write", Arguments: args, Status: "completed"}
	runHook(t, d, "tool", map[string]any{"Session": "s-4", "Transcript": transcript, "Tool": tool})
	var status string
	if err := db.QueryRow(`SELECT status FROM tool_call WHERE native_call_id='tc'`).Scan(&status); err != nil || status != "completed" {
		t.Fatalf("tool after the hook: %q %v", status, err)
	}
	_ = db.QueryRow(`SELECT count(*) FROM file_touch WHERE path='made.txt'`).Scan(&n)
	if n != 1 {
		t.Fatalf("touches after the hook = %d", n)
	}
	// The transcript names the same call again, plus the reply; the close
	// must not duplicate the call.
	f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"kind":"prompt","body":"make a file"}` + "\n" +
		`{"kind":"tool","path":"` + filepath.Join(root, "made.txt") + `","body":"x\n"}` + "\n" +
		`{"kind":"text","body":"Made it."}` + "\n")
	_ = f.Close()
	runHook(t, d, "stop", sess)
	_ = db.QueryRow(`SELECT count(*) FROM turn`).Scan(&n)
	if n != 1 {
		t.Fatalf("turns = %d, want 1", n)
	}
	_ = db.QueryRow(`SELECT count(*) FROM turn WHERE state='completed'`).Scan(&n)
	if n != 1 {
		t.Fatal("the turn must be completed")
	}
	_ = db.QueryRow(`SELECT count(*) FROM tool_call`).Scan(&n)
	if n != 1 {
		t.Fatalf("tool calls = %d, want 1 (no duplicate from the transcript)", n)
	}
	_ = db.QueryRow(`SELECT count(*) FROM file_touch`).Scan(&n)
	if n != 1 {
		t.Fatalf("touches = %d, want 1", n)
	}
	_ = db.QueryRow(`SELECT count(*) FROM agent_message`).Scan(&n)
	if n != 2 {
		t.Fatalf("messages = %d, want the prompt and the reply", n)
	}
}

func TestATurnThatOnlyReadRecordsNothing(t *testing.T) {
	root := gitRepo(t)
	st := enabledStore(t, root)
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(transcript, nil, 0o600)
	d := &Dispatcher{Root: root, Version: "test", Now: time.Now, Deliver: Deliver("test", nil)}
	sess := map[string]any{"Session": "s-2", "Transcript": transcript}
	runHook(t, d, "start", sess)
	runHook(t, d, "prompt", sess)
	runHook(t, d, "stop", sess)
	var n int
	_ = st.DB().QueryRow(`SELECT count(*) FROM turn`).Scan(&n)
	if n != 0 {
		t.Fatalf("turns = %d, want none", n)
	}
}

func TestSpoolWhenTheStoreIsLockedAndDrainLater(t *testing.T) {
	root := gitRepo(t)
	st := enabledStore(t, root)
	lock, err := store.Lock(root) // the desktop app holds the store
	if err != nil {
		t.Fatal(err)
	}
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(transcript, []byte(`{"kind":"prompt","body":"hi"}`+"\n"+`{"kind":"text","body":"hello"}`+"\n"), 0o600)
	d := &Dispatcher{Root: root, Version: "test", Now: time.Now, Deliver: Deliver("test", nil)}
	sess := map[string]any{"Session": "s-3", "Transcript": transcript}
	runHook(t, d, "start", sess)
	runHook(t, d, "prompt", map[string]any{"Session": "s-3", "Transcript": transcript, "Prompt": "hi"})
	// The prompt hook took the offset at 2 lines; nothing new, so give it
	// content by resetting the offset as a slower agent would see it.
	stt, _ := LoadState(root, "fake", "s-3")
	stt.Offset = 0
	_ = stt.Save(root)
	runHook(t, d, "stop", sess)
	stt, _ = LoadState(root, "fake", "s-3")
	if stt.Pending != 3 { // the session start, the opening prompt and the turn
		t.Fatalf("pending = %d, want 3 (spooled)", stt.Pending)
	}
	ents, _ := os.ReadDir(SpoolDir(root))
	if len(ents) != 3 { // the start, the prompt and the turn
		t.Fatalf("spool entries = %d", len(ents))
	}
	_ = lock.Release()

	// The next direct delivery drains the spool first.
	runHook(t, d, "end", sess)
	var n int
	_ = st.DB().QueryRow(`SELECT count(*) FROM agent_message`).Scan(&n)
	if n != 2 {
		t.Fatalf("messages after drain = %d, want 2", n)
	}
	if ents, _ = os.ReadDir(SpoolDir(root)); len(ents) != 0 {
		t.Fatal("spool not emptied")
	}
	stt, _ = LoadState(root, "fake", "s-3")
	if stt.Pending != 0 {
		t.Fatalf("pending after drain = %d", stt.Pending)
	}
}

func TestBadSessionIDsAreRefused(t *testing.T) {
	d := &Dispatcher{Root: t.TempDir(), Now: time.Now, Deliver: func(context.Context, Delivery) error { return nil }}
	for _, id := range []string{"../x", "a/b", "", ".hidden", strings.Repeat("a", 300)} {
		if _, err := d.Run(context.Background(), fakeAgent{}, &Event{Kind: SessionStart, SessionID: id}); id != "" && err == nil {
			t.Fatalf("%q accepted", id)
		}
	}
}

func TestReadPayloadStopsAfterOneValue(t *testing.T) {
	r := strings.NewReader(`{"a":1}` + "\n" + `{"b":2}`)
	b, err := ReadPayload(r)
	if err != nil || string(b) != `{"a":1}` {
		t.Fatalf("%s %v", b, err)
	}
	if b, err := ReadPayload(strings.NewReader("")); err != nil || b != nil {
		t.Fatalf("empty: %s %v", b, err)
	}
}

func TestGitHooksInstallChainsAndRestores(t *testing.T) {
	root := gitRepo(t)
	ctx := context.Background()
	hooksDir := filepath.Join(root, ".git", "hooks")
	_ = os.MkdirAll(hooksDir, 0o755)
	mine := filepath.Join(hooksDir, "post-commit")
	_ = os.WriteFile(mine, []byte("#!/bin/sh\necho mine\n"), 0o755)
	if GitHooksState(ctx, root) != Absent {
		t.Fatal("state before")
	}
	changed, err := InstallGitHooks(ctx, root)
	if err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	if GitHooksState(ctx, root) != Current {
		t.Fatal("state after install")
	}
	b, _ := os.ReadFile(mine + ".pre-atlas")
	if string(b) != "#!/bin/sh\necho mine\n" {
		t.Fatalf("backup = %q", b)
	}
	b, _ = os.ReadFile(mine)
	if !strings.Contains(string(b), "atlas hooks git post-commit") || !strings.Contains(string(b), "post-commit.pre-atlas") {
		t.Fatalf("hook = %s", b)
	}
	if changed, _ := InstallGitHooks(ctx, root); changed {
		t.Fatal("second install changed something")
	}
	if err := UninstallGitHooks(ctx, root); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(mine)
	if string(b) != "#!/bin/sh\necho mine\n" {
		t.Fatalf("restored = %q", b)
	}
}

func tool(id, name, args string) record.CapturedItem {
	return record.CapturedItem{Tool: &record.Tool{NativeID: id, Name: name, Arguments: json.RawMessage(args), Status: "completed"}}
}

func TestDropDeliveredPrefersIDsAndKeepsDistinctCommands(t *testing.T) {
	// Ids match between hook and transcript: two distinct `sleep 4` calls,
	// one delivered, one whose hook failed; the second must survive.
	delivered := [][]string{toolKeys(*tool("c1", "Bash", `{"command":"sleep 4"}`).Tool)}
	items := []record.CapturedItem{tool("c1", "Bash", `{"command":"sleep 4"}`), tool("c2", "Bash", `{"command":"sleep 4"}`), {ID: "m", Mode: "text", Body: "ok"}}
	kept := dropDelivered(items, delivered)
	if len(kept) != 3 || !kept[0].Tool.NoSample || kept[1].Tool == nil || kept[1].Tool.NativeID != "c2" || kept[1].Tool.NoSample || kept[2].Tool != nil {
		t.Fatalf("kept = %+v", kept)
	}
	// Ids differ (another agent): match by arguments even when the transcript
	// pretty-prints them, each delivered command once.
	delivered = [][]string{toolKeys(*tool("h1", "bash", `{"command":"ls"}`).Tool), toolKeys(*tool("h2", "write", `{"path":"a.txt","contents":"x"}`).Tool)}
	items = []record.CapturedItem{tool("t1", "Shell", `{ "command": "ls" }`), tool("t2", "Shell", `{"command":"ls"}`), tool("t3", "Write", `{"path": "a.txt", "contents": "x"}`)}
	kept = dropDelivered(items, delivered)
	if len(kept) != 1 || kept[0].Tool.NativeID != "t2" {
		t.Fatalf("kept = %+v", kept)
	}
	// Empty arguments never match by arguments; an argv command matches a
	// string one.
	delivered = [][]string{toolKeys(*tool("h3", "mcp__x__briefing", `{}`).Tool), toolKeys(*tool("h4", "shell", `{"command":["bash","-lc","cat a"]}`).Tool)}
	items = []record.CapturedItem{tool("t4", "mcp__y__other", `{}`), tool("t5", "Bash", `{"command":"bash -lc cat a"}`)}
	kept = dropDelivered(items, delivered)
	if len(kept) != 1 || kept[0].Tool.NativeID != "t4" {
		t.Fatalf("kept = %+v", kept)
	}
}

func TestShellChangesLandOnTheShellCall(t *testing.T) {
	root := gitRepo(t)
	st := enabledStore(t, root)
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(transcript, nil, 0o600)
	d := &Dispatcher{Root: root, Version: "test", Now: time.Now, Deliver: Deliver("test", nil)}
	sess := map[string]any{"Session": "s-5", "Transcript": transcript}
	runHook(t, d, "start", sess)
	runHook(t, d, "prompt", map[string]any{"Session": "s-5", "Transcript": transcript, "Prompt": "touch a file with the shell"})
	_ = os.WriteFile(filepath.Join(root, "by-shell.txt"), []byte("x\n"), 0o644)
	args, _ := json.Marshal(map[string]string{"command": "echo x > by-shell.txt"})
	runHook(t, d, "tool", map[string]any{"Session": "s-5", "Transcript": transcript,
		"Tool": &record.Tool{NativeID: "sh1", Name: "Bash", Arguments: args, Status: "completed"}})
	db := st.DB()
	var path, callID string
	if err := db.QueryRow(`SELECT f.path, c.native_call_id FROM file_touch f JOIN tool_call c ON c.id = f.tool_call_id`).Scan(&path, &callID); err != nil || path != "by-shell.txt" || callID != "sh1" {
		t.Fatalf("touch after the shell hook: %q %q %v", path, callID, err)
	}
	f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"kind":"text","body":"Done."}` + "\n")
	_ = f.Close()
	runHook(t, d, "stop", sess)
	var n int
	_ = db.QueryRow(`SELECT count(*) FROM tool_call`).Scan(&n)
	if n != 1 {
		t.Fatalf("tool calls = %d, want only the shell call (no synthetic changes call)", n)
	}
	_ = db.QueryRow(`SELECT count(*) FROM file_touch`).Scan(&n)
	if n != 1 {
		t.Fatalf("touches = %d, want 1", n)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=T", "-c", "user.email=t@e", "-c", "commit.gpgsign=false"}, args...)...)
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
}

func TestACommitMadeDuringTheTurnGetsItsCheckpoint(t *testing.T) {
	for _, perEvent := range []bool{true, false} {
		root := gitRepo(t)
		st := enabledStore(t, root)
		transcript := filepath.Join(t.TempDir(), "s.jsonl")
		_ = os.WriteFile(transcript, nil, 0o600)
		d := &Dispatcher{Root: root, Version: "test", Now: time.Now, Deliver: Deliver("test", nil)}
		sess := map[string]any{"Session": "s-c", "Transcript": transcript}
		runHook(t, d, "start", sess)
		runHook(t, d, "prompt", map[string]any{"Session": "s-c", "Transcript": transcript, "Prompt": "write and commit"})
		// One shell command writes the file and commits it; the
		// post-commit hook would run here and find no touch yet.
		_ = os.WriteFile(filepath.Join(root, "c2.txt"), []byte("two\n"), 0o644)
		gitIn(t, root, "add", "c2.txt")
		gitIn(t, root, "commit", "-q", "-m", "agent commit")
		if perEvent {
			args, _ := json.Marshal(map[string]string{"command": "echo two > c2.txt && git add c2.txt && git commit -m 'agent commit'"})
			runHook(t, d, "tool", map[string]any{"Session": "s-c", "Transcript": transcript,
				"Tool": &record.Tool{NativeID: "sh", Name: "Bash", Arguments: args, Status: "completed"}})
		}
		f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
		_, _ = f.WriteString(`{"kind":"text","body":"Committed."}` + "\n")
		_ = f.Close()
		runHook(t, d, "stop", sess)
		var files string
		if err := st.DB().QueryRow(`SELECT files_touched FROM checkpoint`).Scan(&files); err != nil || !strings.Contains(files, "c2.txt") {
			t.Fatalf("perEvent=%v: checkpoint files = %q %v", perEvent, files, err)
		}
		var live int
		_ = st.DB().QueryRow(`SELECT count(*) FROM file_touch WHERE path = 'c2.txt' AND consumed_by_commit IS NULL`).Scan(&live)
		if live != 0 {
			t.Fatalf("perEvent=%v: the late touch must be consumed by the commit it linked to", perEvent)
		}
	}
}
