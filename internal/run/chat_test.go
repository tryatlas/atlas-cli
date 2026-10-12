package run

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/tryatlas/cli/internal/agents/acp"
)

// fakeAgents launches the scripted agent instead of a registry agent.
func chatWithFake(t *testing.T) (*Chat, *Host) {
	t.Helper()
	root := testProject(t)
	h, err := StartHost(root, HostOptions{Version: "test", Ingest: "http://127.0.0.1:1", Tokens: noTokens{}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = h.Close() })
	c := &Chat{Host: h, AgentID: "fake-agent", Cwd: root, View: &testView{allow: true}}
	c.launchFn = func(ctx context.Context, s *Session) (*acp.Agent, *acp.Installed, error) {
		ag, err := acp.Connect(ctx, "fake-agent", fakeCommand("edit", ""), root, "test", s)
		return ag, &acp.Installed{Type: acp.TypeRegistry}, err
	}
	return c, h
}

func TestChatRestartAfterACrash(t *testing.T) {
	c, h := chatWithFake(t)
	ctx := context.Background()
	if err := c.Start(ctx, ""); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(context.Background()) })
	if _, err := c.Prompt(ctx, "first", nil); err != nil {
		t.Fatal(err)
	}
	id := c.Info().SessionID
	// Kill the agent's whole process group, as a crash would.
	_ = syscall.Kill(-c.current().Agent.PID(), syscall.SIGKILL)
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the disconnect was not noticed")
	}
	if c.Exit() == "" {
		t.Fatal("no exit description")
	}
	if err := c.Restart(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Info().SessionID != id {
		t.Fatalf("restart reopens the same session: %s vs %s", c.Info().SessionID, id)
	}
	if r, err := c.Prompt(ctx, "second", nil); err != nil || r.StopReason != acp.StopEndTurn {
		t.Fatalf("%+v %v", r, err)
	}
	if got := one(t, h.Store.DB(), `SELECT group_concat(body, '|') FROM (SELECT body FROM agent_message WHERE role = 'user' ORDER BY seq)`); got != "first|second" {
		t.Fatalf("both turns recorded on the one session: %s", got)
	}
}
