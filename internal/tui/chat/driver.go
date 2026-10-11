// Package chat is the atlas run chat screen (ADR-0009 §1): a Bubble Tea v2
// view over one ACP session. It decides everything from what the agent
// advertises and sends; nothing here looks at an agent id.
package chat

import (
	"context"
	"sync"

	tea "charm.land/bubbletea/v2"

	"github.com/tryatlas/atlas-cli/internal/agents/acp"
)

// Info is what the screen shows about the agent and its session.
type Info struct {
	AgentName string
	SessionID string
	Cwd       string
	Caps      acp.Capabilities
	Modes     *acp.Modes
	Config    []acp.ConfigOption
	Capturing bool
}

// Driver runs the session behind the screen.
type Driver interface {
	Info() Info
	Prompt(ctx context.Context, text string, images []acp.ContentBlock) (*acp.PromptResult, error)
	Cancel()
	SetMode(ctx context.Context, modeID string) error
	SetConfig(ctx context.Context, configID string, value any) error
	ListSessions(ctx context.Context) ([]acp.ListedSession, error)
	Resume(ctx context.Context, sessionID string) error
	NewSession(ctx context.Context) error
	// Restart relaunches the agent and reopens the current session.
	Restart(ctx context.Context) error
	// Done closes when the agent's connection ends; Exit explains why.
	Done() <-chan struct{}
	Exit() string
	// TerminalOutput is a client-side terminal's buffered output.
	TerminalOutput(id string) string
}

// Messages from the session into the program.
type (
	updateMsg struct{ u acp.Update }
	permMsg   struct {
		req   acp.PermissionRequest
		reply chan permAnswer
		ctx   context.Context
	}
	permAnswer struct {
		id string
		ok bool
	}
	elicitMsg struct {
		e     acp.Elicitation
		reply chan acp.ElicitationAnswer
		ctx   context.Context
	}
	elicitDoneMsg struct{ id string }
	dismissMsg    struct{ ctx context.Context }
)

// Bridge is the session's view: it turns updates and questions into
// program messages, and waits for the user's answers.
type Bridge struct {
	program *tea.Program
	ready   chan struct{}

	mu    sync.Mutex
	cond  *sync.Cond
	queue []tea.Msg
}

// NewBridge returns a bridge; attach the program before the agent runs.
func NewBridge() *Bridge {
	b := &Bridge{ready: make(chan struct{})}
	b.cond = sync.NewCond(&b.mu)
	go b.forward()
	return b
}

// Attach connects the program.
func (b *Bridge) Attach(p *tea.Program) {
	b.program = p
	close(b.ready)
}

// forward delivers queued messages in order; the agent's serial queue
// never waits on the screen.
func (b *Bridge) forward() {
	<-b.ready
	for {
		b.mu.Lock()
		for len(b.queue) == 0 {
			b.cond.Wait()
		}
		m := b.queue[0]
		b.queue[0] = nil
		b.queue = b.queue[1:]
		b.mu.Unlock()
		b.program.Send(m)
	}
}

func (b *Bridge) send(m tea.Msg) {
	b.mu.Lock()
	b.queue = append(b.queue, m)
	b.cond.Signal()
	b.mu.Unlock()
}

// Update forwards a session/update.
func (b *Bridge) Update(_ string, u acp.Update) { b.send(updateMsg{u}) }

// Permission shows the agent's own options and waits.
func (b *Bridge) Permission(ctx context.Context, req acp.PermissionRequest) (string, bool) {
	reply := make(chan permAnswer, 1)
	b.send(permMsg{req: req, reply: reply, ctx: ctx})
	select {
	case a := <-reply:
		return a.id, a.ok
	case <-ctx.Done():
		b.send(dismissMsg{ctx})
		return "", false
	}
}

// Elicit shows a form or a URL and waits.
func (b *Bridge) Elicit(ctx context.Context, e acp.Elicitation) acp.ElicitationAnswer {
	reply := make(chan acp.ElicitationAnswer, 1)
	b.send(elicitMsg{e: e, reply: reply, ctx: ctx})
	select {
	case a := <-reply:
		return a
	case <-ctx.Done():
		b.send(dismissMsg{ctx})
		return acp.ElicitationAnswer{Action: "cancel"}
	}
}

// ElicitationComplete closes a URL elicitation finished elsewhere.
func (b *Bridge) ElicitationComplete(id string) { b.send(elicitDoneMsg{id}) }
