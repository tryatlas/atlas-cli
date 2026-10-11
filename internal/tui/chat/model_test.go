package chat

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/tryatlas/atlas-cli/internal/agents/acp"
)

type fakeDriver struct {
	mu        sync.Mutex
	prompts   []string
	images    int
	cancelled int
	mode      string
	config    map[string]any
	done      chan struct{}
	info      Info
}

func newFake() *fakeDriver {
	f := &fakeDriver{done: make(chan struct{}), config: map[string]any{}}
	f.info = Info{AgentName: "Fake", SessionID: "s-1", Cwd: "/w", Capturing: true,
		Modes: &acp.Modes{CurrentModeID: "ask", AvailableModes: []acp.SessionMode{{ID: "ask", Name: "Ask"}, {ID: "code", Name: "Code"}}},
		Config: acp.ParseConfigOptions(json.RawMessage(`[{"id":"model","name":"Model","category":"model","type":"select","currentValue":"m1",
			"options":[{"value":"m1","name":"Model One"},{"value":"m2","name":"Model Two"}]}]`))}
	f.info.Caps.PromptCapabilities.Image = true
	_ = json.Unmarshal([]byte(`{"list":{}}`), &f.info.Caps.SessionCapabilities)
	return f
}

func (f *fakeDriver) Info() Info { f.mu.Lock(); defer f.mu.Unlock(); return f.info }
func (f *fakeDriver) Prompt(_ context.Context, text string, images []acp.ContentBlock) (*acp.PromptResult, error) {
	f.mu.Lock()
	f.prompts = append(f.prompts, text)
	f.images += len(images)
	f.mu.Unlock()
	return &acp.PromptResult{StopReason: acp.StopEndTurn, Usage: &acp.PromptUsage{InputTokens: 1200, OutputTokens: 30}}, nil
}
func (f *fakeDriver) Cancel() { f.mu.Lock(); f.cancelled++; f.mu.Unlock() }
func (f *fakeDriver) SetMode(_ context.Context, id string) error {
	f.mu.Lock()
	f.mode = id
	f.info.Modes.CurrentModeID = id
	f.mu.Unlock()
	return nil
}
func (f *fakeDriver) SetConfig(_ context.Context, id string, v any) error {
	f.mu.Lock()
	f.config[id] = v
	f.mu.Unlock()
	return nil
}
func (f *fakeDriver) ListSessions(context.Context) ([]acp.ListedSession, error) {
	return []acp.ListedSession{{SessionID: "old-1", Title: "Earlier work", UpdatedAt: "2026-10-10T10:00:00Z"}}, nil
}
func (f *fakeDriver) Resume(_ context.Context, id string) error {
	f.mu.Lock()
	f.info.SessionID = id
	f.mu.Unlock()
	return nil
}
func (f *fakeDriver) NewSession(context.Context) error { return nil }
func (f *fakeDriver) Restart(context.Context) error    { return nil }
func (f *fakeDriver) Done() <-chan struct{}            { return f.done }
func (f *fakeDriver) Exit() string                     { return "exit status 1: boom" }
func (f *fakeDriver) TerminalOutput(string) string     { return "term output\n" }

var ansi = regexp.MustCompile(`\x1b\[[0-9;:]*[A-Za-z]`)

func screen(m *Model) string { return ansi.ReplaceAllString(m.View().Content, "") }

func press(m *Model, k string) tea.Cmd {
	var msg tea.KeyPressMsg
	switch k {
	case "enter":
		msg = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "esc":
		msg = tea.KeyPressMsg{Code: tea.KeyEscape}
	case "down":
		msg = tea.KeyPressMsg{Code: tea.KeyDown}
	case "tab":
		msg = tea.KeyPressMsg{Code: tea.KeyTab}
	case "space":
		msg = tea.KeyPressMsg{Code: tea.KeySpace, Text: " "}
	default:
		if strings.HasPrefix(k, "ctrl+") {
			msg = tea.KeyPressMsg{Code: rune(k[5]), Mod: tea.ModCtrl}
		} else {
			msg = tea.KeyPressMsg{Code: rune(k[0]), Text: k}
		}
	}
	_, c := m.Update(msg)
	return c
}

func typeText(m *Model, s string) {
	for _, r := range s {
		_, _ = m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
	}
}

// run executes a command and feeds back the screen's own messages, as the
// program would. Library commands (cursor blink ticks) are skipped.
func run(m *Model, c tea.Cmd) {
	if c == nil {
		return
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- c() }()
	var msg tea.Msg
	select {
	case msg = <-ch:
	case <-time.After(200 * time.Millisecond):
		return
	}
	switch msg := msg.(type) {
	case tea.BatchMsg:
		for _, x := range msg {
			run(m, x)
		}
	case promptDoneMsg, opDoneMsg, sessionsMsg, resumedMsg, restartedMsg, openValueMsg:
		_, next := m.Update(msg)
		run(m, next)
	}
}

func upd(t *testing.T, m *Model, js string) {
	t.Helper()
	var u acp.Update
	if err := json.Unmarshal([]byte(js), &u); err != nil {
		t.Fatal(err)
	}
	_, _ = m.Update(updateMsg{u})
}

func newModel(t *testing.T) (*Model, *fakeDriver) {
	t.Helper()
	f := newFake()
	m := New(context.Background(), f)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	return m, f
}

func TestStreamingToolCardsAndUsage(t *testing.T) {
	m, f := newModel(t)
	typeText(m, "Fix it")
	run(m, press(m, "enter"))
	if len(f.prompts) != 1 || f.prompts[0] != "Fix it" {
		t.Fatalf("%v", f.prompts)
	}
	upd(t, m, `{"sessionUpdate":"agent_thought_chunk","messageId":"m0","content":{"type":"text","text":"Pondering"}}`)
	upd(t, m, `{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"Hello "}}`)
	upd(t, m, `{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"world"}}`)
	upd(t, m, `{"sessionUpdate":"tool_call","toolCallId":"t1","title":"Edit a.go","kind":"edit","status":"pending","locations":[{"path":"/w/a.go","line":3}]}`)
	upd(t, m, `{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed","content":[{"type":"diff","path":"/w/a.go","oldText":"x\ny\n","newText":"x\nz\n"}]}`)
	upd(t, m, `{"sessionUpdate":"tool_call","toolCallId":"t2","title":"Run tests","kind":"execute","status":"in_progress","content":[{"type":"terminal","terminalId":"term-1"}]}`)
	upd(t, m, `{"sessionUpdate":"plan","entries":[{"content":"Edit a.go","status":"completed"},{"content":"Test","status":"in_progress"}]}`)
	upd(t, m, `{"sessionUpdate":"usage_update","used":31370,"size":1000000,"cost":{"amount":0.12,"currency":"USD"}}`)
	s := screen(m)
	for _, want := range []string{"› Fix it", "Pondering", "Hello world", "✓ Edit a.go", "edit · completed", "/w/a.go:3", "- y", "+ z",
		"● Run tests", "│ term output", "Plan", "✓ Edit a.go", "● Test", "context 31.4k/1.0M (3%)", "tokens in 1.2k · out 30", "0.12 USD",
		"mode Ask", "model Model One"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestPermissionUsesTheAgentsOptions(t *testing.T) {
	m, _ := newModel(t)
	reply := make(chan permAnswer, 1)
	req := acp.PermissionRequest{SessionID: "s-1", ToolCall: json.RawMessage(`{"title":"Write notes.md"}`), Options: []acp.PermissionOption{
		{OptionID: "a1", Name: "Allow once", Kind: "allow_once"}, {OptionID: "a2", Name: "Always allow edits", Kind: "allow_always"}, {OptionID: "r1", Name: "No", Kind: "reject_once"}}}
	_, _ = m.Update(permMsg{req: req, reply: reply, ctx: context.Background()})
	s := screen(m)
	if !strings.Contains(s, "The agent asks: Write notes.md") || !strings.Contains(s, "2. Always allow edits") || !strings.Contains(s, "allow always") {
		t.Fatal(s)
	}
	press(m, "down")
	press(m, "enter")
	if a := <-reply; !a.ok || a.id != "a2" {
		t.Fatalf("%+v", a)
	}
	if m.picker != nil {
		t.Fatal("overlay left open")
	}
	// A withdrawn question closes its overlay.
	ctx, cancel := context.WithCancel(context.Background())
	_, _ = m.Update(permMsg{req: req, reply: make(chan permAnswer, 1), ctx: ctx})
	cancel()
	_, _ = m.Update(dismissMsg{ctx})
	if m.picker != nil {
		t.Fatal("withdrawn permission still shown")
	}
}

func TestElicitationForm(t *testing.T) {
	m, _ := newModel(t)
	reply := make(chan acp.ElicitationAnswer, 1)
	schema := `{"type":"object","properties":{"name":{"type":"string","title":"Name"},"plan":{"type":"string","enum":["free","pro"]},
		"agree":{"type":"boolean","title":"Agree"},"seats":{"type":"integer"}},"required":["name"]}`
	_, _ = m.Update(elicitMsg{e: acp.Elicitation{Mode: "form", Message: "Who are you?", RequestedSchema: json.RawMessage(schema)}, reply: reply, ctx: context.Background()})
	press(m, "enter")
	if !strings.Contains(screen(m), "Name is required") {
		t.Fatal(screen(m))
	}
	typeText(m, "Ada")
	press(m, "tab")
	press(m, "space") // plan → pro
	press(m, "tab")
	press(m, "space") // agree
	press(m, "tab")
	typeText(m, "3")
	press(m, "enter")
	a := <-reply
	b, _ := json.Marshal(a.Content)
	if a.Action != "accept" || string(b) != `{"agree":true,"name":"Ada","plan":"pro","seats":3}` {
		t.Fatalf("%s %s", a.Action, b)
	}
	urlReply := make(chan acp.ElicitationAnswer, 1)
	_, _ = m.Update(elicitMsg{e: acp.Elicitation{Mode: "url", ElicitationID: "e1", Message: "Sign in", URL: "https://example.com/x"}, reply: urlReply, ctx: context.Background()})
	if !strings.Contains(screen(m), "https://example.com/x") {
		t.Fatal(screen(m))
	}
	_, _ = m.Update(elicitDoneMsg{"e1"})
	if a := <-urlReply; a.Action != "accept" {
		t.Fatal(a.Action)
	}
}

func TestSlashPickerAndPickers(t *testing.T) {
	m, f := newModel(t)
	upd(t, m, `{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"review","description":"Review changes"},{"name":"init","description":"Init"}]}`)
	typeText(m, "/re")
	if !strings.Contains(screen(m), "/review") || strings.Contains(screen(m), "/init") {
		t.Fatal(screen(m))
	}
	press(m, "tab")
	if m.in.Value() != "/review " {
		t.Fatalf("%q", m.in.Value())
	}
	m.in.Reset()
	press(m, "ctrl+t")
	press(m, "down")
	run(m, press(m, "enter"))
	if f.mode != "code" || !strings.Contains(screen(m), "mode Code") {
		t.Fatalf("mode %q", f.mode)
	}
	press(m, "ctrl+p")
	press(m, "down")
	run(m, press(m, "enter"))
	if f.config["model"] != "m2" {
		t.Fatalf("%v", f.config)
	}
	run(m, press(m, "ctrl+l"))
	if !strings.Contains(screen(m), "Earlier work") {
		t.Fatal(screen(m))
	}
	run(m, press(m, "enter"))
	if f.info.SessionID != "old-1" {
		t.Fatal(f.info.SessionID)
	}
}

func TestCancelDisconnectAndImages(t *testing.T) {
	m, f := newModel(t)
	m.state = stateRunning
	press(m, "esc")
	if f.cancelled != 1 || m.state != stateCancelling {
		t.Fatal("esc cancels a running turn")
	}
	m.state = stateIdle
	_, _ = m.Update(disconnectedMsg{})
	if !strings.Contains(screen(m), "boom") || !strings.Contains(screen(m), "disconnected (ctrl+r to restart)") {
		t.Fatal(screen(m))
	}
	run(m, press(m, "ctrl+r"))
	if m.state != stateIdle {
		t.Fatal("restart")
	}
	if _, err := acp.ImageBlock("/no/such.txt"); err == nil {
		t.Fatal("non-image accepted")
	}
	f.info.Caps.PromptCapabilities.Image = false
	press(m, "ctrl+o")
	if m.line != nil || !strings.Contains(screen(m), "does not accept images") {
		t.Fatal("attach is offered only when advertised")
	}
}
