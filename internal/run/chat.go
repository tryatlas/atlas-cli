package run

import (
	"context"
	"errors"
	"sync"

	"github.com/tryatlas/atlas-cli/internal/agents/acp"
	"github.com/tryatlas/atlas-cli/internal/tui/chat"
)

// Chat drives the chat screen: one agent process and its current session,
// relaunched on restart. It implements chat.Driver.
type Chat struct {
	Agents  Agents
	Host    *Host
	AgentID string
	Cwd     string
	View    View
	// Login runs a terminal sign-in; nil once the screen owns the terminal.
	Login func(context.Context, *acp.Agent, acp.AuthMethod) error
	// ChooseAuth picks a sign-in method when several are offered.
	ChooseAuth func([]acp.AuthMethod) (acp.AuthMethod, bool)

	mu   sync.Mutex
	sess *Session
	inst *acp.Installed
}

var _ chat.Driver = (*Chat)(nil)

func (c *Chat) current() *Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sess
}

// Start launches the agent and opens a session: a new one, or resume.
func (c *Chat) Start(ctx context.Context, resume string) error {
	s, err := c.launch(ctx)
	if err != nil {
		return err
	}
	if err := c.open(ctx, s, resume); err != nil {
		s.Agent.Close()
		return err
	}
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
	return nil
}

func (c *Chat) launch(ctx context.Context) (*Session, error) {
	s := NewSession(c.AgentID, c.Cwd, c.Host, c.View)
	s.ChooseAuth = c.ChooseAuth
	ag, inst, err := c.Agents.Launch(ctx, c.AgentID, c.Cwd, s)
	if err != nil {
		return nil, err
	}
	s.Agent = ag
	c.mu.Lock()
	c.inst = inst
	c.mu.Unlock()
	return s, nil
}

func (c *Chat) open(ctx context.Context, s *Session, resume string) error {
	var err error
	if resume != "" {
		err = s.Reopen(ctx, resume)
	} else {
		var login func(context.Context, acp.AuthMethod) error
		if c.Login != nil {
			login = func(ctx context.Context, m acp.AuthMethod) error { return c.Login(ctx, s.Agent, m) }
		}
		err = s.Open(ctx, login)
	}
	if err != nil {
		return err
	}
	c.mu.Lock()
	mode := ""
	if c.inst != nil {
		mode = c.inst.DefaultMode
	}
	c.mu.Unlock()
	s.ApplyDefaultMode(ctx, mode)
	return nil
}

// Info describes the agent and the session.
func (c *Chat) Info() chat.Info {
	s := c.current()
	if s == nil {
		return chat.Info{AgentName: c.AgentID, Cwd: c.Cwd}
	}
	modes, config := s.State()
	return chat.Info{AgentName: s.Agent.Name(), SessionID: s.ID, Cwd: c.Cwd, Caps: s.Agent.Init.AgentCapabilities,
		Modes: modes, Config: config, Capturing: c.Host != nil && c.Host.Capturing()}
}

// Prompt sends a turn.
func (c *Chat) Prompt(ctx context.Context, text string, images []acp.ContentBlock) (*acp.PromptResult, error) {
	s := c.current()
	r, err := s.Prompt(ctx, text, images, nil)
	if errors.Is(err, acp.ErrClosed) {
		err = s.Agent.Exited()
	}
	return r, err
}

// Cancel cancels the running turn.
func (c *Chat) Cancel() {
	if s := c.current(); s != nil {
		s.Agent.Cancel(s.ID)
	}
}

// SetMode switches the mode.
func (c *Chat) SetMode(ctx context.Context, modeID string) error {
	return c.current().SetMode(ctx, modeID)
}

// SetConfig sets a config option.
func (c *Chat) SetConfig(ctx context.Context, configID string, value any) error {
	return c.current().SetConfig(ctx, configID, value)
}

// ListSessions lists the agent's sessions in this folder.
func (c *Chat) ListSessions(ctx context.Context) ([]acp.ListedSession, error) {
	var all []acp.ListedSession
	cursor := ""
	for range 10 {
		l, next, err := c.current().Agent.ListSessions(ctx, c.Cwd, cursor)
		if err != nil {
			return all, err
		}
		all = append(all, l...)
		if next == "" {
			break
		}
		cursor = next
	}
	return all, nil
}

// switchTo replaces the session within the same agent process.
func (c *Chat) switchTo(ctx context.Context, resume string) error {
	old := c.current()
	s := NewSession(c.AgentID, c.Cwd, c.Host, c.View)
	s.Agent = old.Agent
	old.Agent.Rehost(s)
	if err := c.open(ctx, s, resume); err != nil {
		old.Agent.Rehost(old)
		return err
	}
	old.Close(ctx)
	_ = old.Agent.CloseSession(ctx, old.ID)
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
	return nil
}

// Resume opens an earlier session (load or resume, by capability).
func (c *Chat) Resume(ctx context.Context, sessionID string) error { return c.switchTo(ctx, sessionID) }

// NewSession starts a fresh session.
func (c *Chat) NewSession(ctx context.Context) error { return c.switchTo(ctx, "") }

// Restart relaunches the agent and reopens the current session when it
// can (else a new one).
func (c *Chat) Restart(ctx context.Context) error {
	old := c.current()
	old.Close(ctx)
	old.Agent.Close()
	s, err := c.launch(ctx)
	if err != nil {
		return err
	}
	resume := ""
	caps := s.Agent.Init.AgentCapabilities
	if old.ID != "" && (caps.LoadSession || caps.CanResume()) {
		resume = old.ID
	}
	if err := c.open(ctx, s, resume); err != nil {
		s.Agent.Close()
		return err
	}
	c.mu.Lock()
	c.sess = s
	c.mu.Unlock()
	return nil
}

// Done closes when the current agent's connection ends.
func (c *Chat) Done() <-chan struct{} { return c.current().Agent.Done() }

// Exit explains how the agent ended.
func (c *Chat) Exit() string {
	if e := c.current().Agent.Exited(); e != nil {
		return e.Error()
	}
	return ""
}

// TerminalOutput reads a client-side terminal.
func (c *Chat) TerminalOutput(id string) string { return c.current().Agent.TerminalOutput(id) }

// Close ends the session and the agent.
func (c *Chat) Close(ctx context.Context) {
	if s := c.current(); s != nil {
		s.Close(ctx)
		s.Agent.Close()
	}
}
