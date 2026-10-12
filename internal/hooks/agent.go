package hooks

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

// InstallState is how an agent's Atlas hook config compares to what Install
// would write now.
type InstallState int

// Install states.
const (
	Absent InstallState = iota
	Current
	Outdated
)

func (s InstallState) String() string {
	switch s {
	case Current:
		return "installed"
	case Outdated:
		return "outdated"
	}
	return "absent"
}

// Agent is one agent integration. Everything agent-specific lives behind it:
// the hook config file and its shape, the payload fields, and the transcript
// format. The dispatcher owns everything else.
type Agent interface {
	// ID is the registry key and the hooks subcommand: claude-code.
	ID() string
	// Name is the display name: Claude Code.
	Name() string
	// Binary is the agent's own command, for detection and resume hints.
	Binary() string
	// Verbs are the hook verbs the agent's config invokes.
	Verbs() []string
	// Parse translates one hook payload. A nil event means nothing to do.
	Parse(verb string, payload []byte) (*Event, error)
	// Install writes the Atlas hook into the agent's config under root,
	// pruning stale Atlas entries; force rewrites every Atlas entry.
	Install(root string, force bool) (changed bool, err error)
	// Uninstall removes the Atlas hook; other content is kept.
	Uninstall(root string) error
	// State reports the hook config's state under root.
	State(root string) InstallState
	// Detect reports that the agent is configured in root (its dot-dir
	// exists) or installed on this machine.
	Detect(root string) bool
	// Reader reads the agent's transcript, or nil when the agent gives
	// everything in its payloads.
	Reader() Reader
	// Response renders what the hook prints on stdout for a user-facing
	// message at the given event; nil prints nothing.
	Response(k Kind, message string) []byte
	// ResumeCommand is how a user continues the session in the agent.
	ResumeCommand(sessionID string) string
}

// Reader reads an agent's transcript between two positions.
type Reader interface {
	// Position is the current end of the transcript in the agent's unit
	// (lines for JSONL, messages for a JSON document).
	Position(path string) (int, error)
	// Settle waits, bounded by ctx, until the agent has finished writing
	// the turn that just ended.
	Settle(ctx context.Context, path string)
	// Read returns what the transcript holds from a position, and the new
	// position.
	Read(path string, from int) (*Turn, int, error)
}

// Turn is one turn's content as a transcript yields it.
type Turn struct {
	Prompts []string
	Items   []Item
	Model   string
	Usage   store.TokenTotals
	// HasUsage says Usage was read, so a zero total is a real zero.
	HasUsage bool
}

// Item is one assistant run or one tool call, in transcript order (the
// recorder's captured item).
type Item = record.CapturedItem

// ── Registry ─────────────────────────────────────────────────────────────────

var (
	regMu    sync.RWMutex
	registry = map[string]Agent{}
)

// Register adds an agent; integrations call it from init.
func Register(a Agent) {
	regMu.Lock()
	defer regMu.Unlock()
	registry[a.ID()] = a
}

// Get finds an agent by id.
func Get(id string) (Agent, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	a, ok := registry[id]
	return a, ok
}

// All lists the agents sorted by id.
func All() []Agent {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Agent, 0, len(registry))
	for _, a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID() < out[j].ID() })
	return out
}

// Deadlines per event: an agent waits on its hook, so none may hang.
const (
	TurnEndDeadline = 30 * time.Second
	OtherDeadline   = 8 * time.Second
)

// Noter is an agent with something the user must do once after atlas-cli agents
// add (approve the hooks in the agent's own UI, trust the project, …).
type Noter interface {
	AddNote() string
}
