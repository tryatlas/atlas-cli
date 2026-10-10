// Package ambient holds the per-agent adapters for agents the user runs as
// normal commands (D3). All agent-specific knowledge on the ambient path lives
// here, behind one interface (ADR-0000); nothing else branches on an agent id.
package ambient

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Dialect is the provider wire format the proxy parses for an agent.
type Dialect int

// Dialects (SPEC §7.2).
const (
	Anthropic Dialect = iota
	OpenAIResponses
	Gemini
)

// Listener is what an adapter needs to route its agent through the proxy.
type Listener struct {
	Port int // the loopback proxy port
}

// ShellSetup is what an adapter adds to `atlas shell`: environment variables
// and, where an env var is not enough (ADR-0004), small wrapper scripts placed
// on a PATH entry that exists only inside the shell.
type ShellSetup struct {
	Env   map[string]string
	Shims map[string]string // command name → sh script body
}

// Adapter integrates one agent (SPEC §7.1, amended by ADR-0000 and ADR-0004).
// Every write is idempotent, marks its entries, and is recorded in
// .atlas/cli.json so removal is exact.
type Adapter interface {
	ID() string
	Detect(ctx context.Context) (installed bool, version string)
	// Configured reports that the Project already has this agent's files.
	Configured() bool

	// Shared memory: the MCP entry in the agent's config.
	InstallMCP(st *State) error
	RemoveMCP(st *State) error

	// Capture: how the agent's traffic reaches the proxy, and hooks for identity.
	Shell(l Listener) (ShellSetup, error)
	InstallHooks(st *State) error
	RemoveHooks(st *State) error

	// Visibility.
	InstallStatus(st *State) error
	RemoveStatus(st *State) error

	Dialect() Dialect
}

// For returns the adapters for a Project. atlas is how agents invoke this
// binary (AtlasCommand).
func For(dir, atlas string) []Adapter {
	return []Adapter{Claude{Dir: dir, Atlas: atlas}}
}

// version runs "<bin> --version" with a short timeout.
func version(ctx context.Context, bin string) (bool, string) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return false, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output() //nolint:gosec // G204: a known agent binary
	if err != nil {
		return true, ""
	}
	return true, strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
}
