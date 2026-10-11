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
	}
	_ = json.Unmarshal(payload, &p)
	ev := &Event{SessionID: p.Session, Transcript: p.Transcript, Prompt: p.Prompt, Files: p.Files}
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
	if stt.Pending != 2 { // the session start and the turn
		t.Fatalf("pending = %d, want 2 (spooled)", stt.Pending)
	}
	ents, _ := os.ReadDir(SpoolDir(root))
	if len(ents) != 2 { // the start and the turn
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
