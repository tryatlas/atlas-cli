package run

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/agents/acp"
	"github.com/tryatlas/atlas-cli/internal/agents/acp/acptest"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/project"
)

func TestMain(m *testing.M) {
	acptest.Main()
	os.Exit(m.Run())
}

type noTokens struct{}

func (noTokens) Token(context.Context) (string, error) { return "", errors.New("signed out") }
func (noTokens) Invalidate()                           {}

// testView records what reached the view and allows every permission.
type testView struct {
	mu      sync.Mutex
	kinds   []string
	allow   bool
	answers int
}

func (v *testView) Update(_ string, u acp.Update) {
	v.mu.Lock()
	v.kinds = append(v.kinds, u.Kind)
	v.mu.Unlock()
}

func (v *testView) Permission(_ context.Context, req acp.PermissionRequest) (string, bool) {
	v.mu.Lock()
	v.answers++
	v.mu.Unlock()
	if v.allow {
		return req.Options[0].OptionID, true
	}
	return req.Options[1].OptionID, true
}

func (v *testView) Elicit(context.Context, acp.Elicitation) acp.ElicitationAnswer {
	return acp.ElicitationAnswer{Action: "decline"}
}
func (v *testView) ElicitationComplete(string) {}

// project makes a local Project with capture and memory on.
func testProject(t *testing.T) string {
	t.Helper()
	tmp, _ := os.MkdirTemp("/tmp", "atlrun") // short: the socket path must fit
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("XDG_RUNTIME_DIR", tmp)
	root := filepath.Join(tmp, "w")
	_ = os.MkdirAll(root, 0o755)
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"commit", "-q", "--allow-empty", "-m", "first"}} {
		c := exec.Command("git", append([]string{"-C", root, "-c", "user.name=T", "-c", "user.email=t@e.test", "-c", "commit.gpgsign=false"}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	root, _ = project.Canonical(root)
	st, err := store.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.PutBinding(context.Background(), store.Binding{WorkspaceID: root, Root: root, Mode: store.ModeLocal, Enabled: true}, time.Now())
	_ = st.Close()
	if err := project.SetMemorySharing(root, true); err != nil {
		t.Fatal(err)
	}
	return root
}

func fakeCommand(scenario string, caps string) acp.Command {
	env := map[string]string{acptest.EnvScenario: scenario}
	if caps != "" {
		env[acptest.EnvCaps] = caps
	}
	return acp.Command{Path: os.Args[0], Args: []string{"-test.run=^$"}, Env: env}
}

func start(t *testing.T, scenario, caps string, v View) (*Session, *Host, string) {
	t.Helper()
	root := testProject(t)
	h, err := StartHost(root, HostOptions{Version: "test", Ingest: "http://127.0.0.1:1", Tokens: noTokens{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.Close() })
	s := NewSession("fake-agent", root, h, v)
	ag, err := acp.Connect(context.Background(), "fake-agent", fakeCommand(scenario, caps), root, "test", s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ag.Close)
	s.Agent = ag
	return s, h, root
}

func one(t *testing.T, db *sql.DB, q string, args ...any) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(q, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return s.String
}

func TestPromptIsCapturedLikeTheDesktop(t *testing.T) {
	v := &testView{allow: true}
	s, h, root := start(t, "edit", "", v)
	ctx := context.Background()
	if err := s.Open(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if s.ID != "sess-1" || s.Modes == nil || s.Modes.CurrentModeID != "ask" {
		t.Fatalf("session %s modes %+v (synthesised from the mode select)", s.ID, s.Modes)
	}
	r, err := s.Prompt(ctx, "Write the notes please", nil, nil)
	if err != nil || r.StopReason != acp.StopEndTurn {
		t.Fatalf("%+v %v", r, err)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "notes.md")); string(b) != "# Notes\n" {
		t.Fatalf("fs/write_text_file: %q", b)
	}
	s.Close(ctx)
	db := h.Store.DB()
	sid := one(t, db, `SELECT id FROM agent_session WHERE native_session_id = 'sess-1'`)
	if got := one(t, db, `SELECT source || '|' || agent || '|' || model || '|' || title FROM agent_session WHERE id = ?`, sid); got != "acp|fake-agent|fake-1|Write the notes please" {
		t.Fatal(got)
	}
	msgs := one(t, db, `SELECT group_concat(role || ':' || mode || ':' || native_message_id || ':' || body, ' / ') FROM
	    (SELECT * FROM agent_message WHERE session_id = ? ORDER BY seq)`, sid)
	if !strings.Contains(msgs, "assistant:thinking:m0:Thinking about it.") || !strings.Contains(msgs, "assistant:text:m1:I'll write the notes.") ||
		!strings.Contains(msgs, "assistant:text:m2:Done.") || !strings.HasPrefix(msgs, "user:text:prompt-1-") {
		t.Fatal(msgs)
	}
	if got := one(t, db, `SELECT tool_name || '|' || kind || '|' || status || '|' || title FROM tool_call WHERE session_id = ?`, sid); got != "Write|edit|completed|Write notes.md" {
		t.Fatal(got)
	}
	if got := one(t, db, `SELECT path || '|' || existed_before || '|' || (sha256_after IS NOT NULL) FROM file_touch WHERE session_id = ?`, sid); got != "notes.md|0|1" {
		t.Fatal(got)
	}
	if got := one(t, db, `SELECT patch FROM agent_edit WHERE session_id = ?`, sid); got != "--- before\n+++ after\n+# Notes\n+" {
		t.Fatalf("%q", got)
	}
	if got := one(t, db, `SELECT token_totals FROM agent_session WHERE id = ?`, sid); !strings.Contains(got, `"input_tokens":10,"output_tokens":5`) ||
		!strings.Contains(got, `"context_used":100,"context_size":1000`) {
		t.Fatal(got)
	}
	if got := one(t, db, `SELECT state FROM turn WHERE session_id = ?`, sid); got != "completed" {
		t.Fatal(got)
	}
	mem, err := sql.Open("sqlite", filepath.Join(root, ".atlas", "memory", "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	if got := one(t, mem, `SELECT group_concat(kind || ':' || key, ' ') FROM (SELECT * FROM events WHERE session = 'sess-1' ORDER BY seq)`); got !=
		"session_start: file_changed:notes.md plan_set:plan session_end:" {
		t.Fatal(got)
	}
	if !strings.Contains(strings.Join(v.kinds, " "), "plan") || strings.Contains(strings.Join(v.kinds, " "), "some_future_update") {
		t.Fatalf("view saw %v (unknown variants are dropped)", v.kinds)
	}
}

func TestRejectedPermissionRecordsAFailedCallAndNoWrites(t *testing.T) {
	s, h, root := start(t, "edit", "", &testView{allow: false})
	ctx := context.Background()
	if err := s.Open(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prompt(ctx, "Write", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "notes.md")); err == nil {
		t.Fatal("written after a rejection")
	}
	db := h.Store.DB()
	if got := one(t, db, `SELECT status FROM tool_call`); got != "failed" {
		t.Fatal(got)
	}
	if got := one(t, db, `SELECT count(*) FROM file_touch`); got != "0" {
		t.Fatal(got)
	}
}

func TestMCPEntryFollowsCapabilities(t *testing.T) {
	s, _, _ := start(t, "edit", "", &testView{})
	got := s.mcpServers()
	if len(got) != 1 || got[0].Type != "http" || got[0].Name != MemoryServerName || len(got[0].Headers) != 1 ||
		!strings.HasPrefix(got[0].Headers[0].Value, "Bearer ") || len(got[0].Headers[0].Value) != len("Bearer ")+64 {
		t.Fatalf("%+v", got)
	}
	s2, _, _ := start(t, "edit", `{"mcpCapabilities":{"http":false}}`, &testView{})
	got = s2.mcpServers()
	if len(got) != 1 || got[0].Type != "" || got[0].Args[0] != "mcp-bridge" || !strings.HasSuffix(got[0].Args[1], "/mcp") ||
		got[0].Env[0].Name != "ATLAS_MCP_TOKEN" || strings.Contains(strings.Join(got[0].Args, " "), got[0].Env[0].Value) {
		t.Fatalf("the stdio bridge entry, token only in env: %+v", got)
	}
}

func TestAuthRequiredRunsTheTerminalMethodThenRetries(t *testing.T) {
	s, _, _ := start(t, "auth", "", &testView{})
	ran := 0
	err := s.Open(context.Background(), func(_ context.Context, m acp.AuthMethod) error {
		ran++
		c, ok := LoginCommand(s.Agent.Command, m)
		if !ok || c.Path != s.Agent.Command.Path || c.Args[len(c.Args)-1] != "--login" {
			t.Fatalf("the agent's own binary with launch args ++ method args: %+v", c)
		}
		return nil
	})
	if err != nil || ran != 1 || s.ID != "sess-1" {
		t.Fatalf("%v ran=%d", err, ran)
	}
	s2, _, _ := start(t, "auth", "", &testView{})
	if err := s2.Open(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "never guesses") {
		t.Fatalf("no login runner and no agent method: %v", err)
	}
	// An agent-kind method is authenticated directly; the agent runs its own flow.
	s3, _, _ := start(t, "auth", `{}`, &testView{})
	s3.Agent.Init.AuthMethods = []acp.AuthMethod{{ID: "oauth", Name: "Browser", Type: ""}}
	if err := s3.Open(context.Background(), nil); err == nil || !strings.Contains(err.Error(), "does not support signing in this way") {
		t.Fatalf("the fake agent answers not implemented, which is not success for a non-terminal method: %v", err)
	}
}

func TestLoadReplaysWithoutRecording(t *testing.T) {
	s, h, _ := start(t, "edit", "", &testView{})
	if err := s.Reopen(context.Background(), "sess-9"); err != nil {
		t.Fatal(err)
	}
	if s.ID != "sess-9" {
		t.Fatal(s.ID)
	}
	if got := one(t, h.Store.DB(), `SELECT count(*) FROM agent_message`); got != "0" {
		t.Fatalf("a replay is not a new turn: %s rows", got)
	}
}

func TestTerminalAuthMetaSpecRunsVerbatim(t *testing.T) {
	m := acp.AuthMethod{ID: "x"}
	m.Meta.TerminalAuth = &acp.TerminalAuthSpec{Label: "Log in", Command: "cursor-agent", Args: []string{"login"}, Env: map[string]string{"A": "1"}}
	c, ok := LoginCommand(acp.Command{Path: "/agent", Args: []string{"--acp"}}, m)
	if !ok || c.Path != "cursor-agent" || strings.Join(c.Args, " ") != "login" || c.Env["A"] != "1" {
		t.Fatalf("%+v", c)
	}
	if _, ok := LoginCommand(acp.Command{}, acp.AuthMethod{ID: "y", Type: "env_var"}); ok {
		t.Fatal("never guess a login command")
	}
	if FirstURL("visit https://example.com/device?code=1 now") != "https://example.com/device?code=1" {
		t.Fatal("url")
	}
}
