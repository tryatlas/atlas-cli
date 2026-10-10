package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// notImplementedError is returned by every command that has not been built yet.
type notImplementedError struct{ path string }

func (e notImplementedError) Error() string { return e.path + ": not implemented" }

// stub returns a RunE that reports the command as not implemented.
func stub(cmd *cobra.Command, _ []string) error {
	return notImplementedError{path: cmd.CommandPath()}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "atlas",
		Short:         "Shared memory and session capture for coding agents",
		Long:          "atlas gives every coding agent run in a folder Atlas shared memory and session capture,\nusing the same on-disk formats as the Atlas desktop app.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("atlas {{.Version}}\n")

	root.AddCommand(
		newLoginCmd(),
		withRun(leaf("logout", "Sign out and delete the local session file", cobra.NoArgs), runLogout),
		newWhoamiCmd(),
		newOrgCmd(),
		newInitCmd(),
		newMemoryCmd(),
		newCaptureCmd(),
		newShellCmd(),
		newEnvCmd(),
		newServeCmd(),
		withRun(leaf("status", "Show login, organisation, Project, gates, capture health and agent integrations", cobra.NoArgs), runStatus),
		newRunCmd(),
		newMCPBridgeCmd(),
		newHookCmd(),
		newStatuslineCmd(),
	)
	return root
}

// leaf builds a stub command with the given argument rule.
func leaf(use, short string, args cobra.PositionalArgs) *cobra.Command {
	return &cobra.Command{Use: use, Short: short, Args: args, RunE: stub}
}

// group builds a parent command that only holds subcommands.
func group(use, short string, children ...*cobra.Command) *cobra.Command {
	c := &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs}
	c.AddCommand(children...)
	return c
}

// withRun replaces a stub with a real implementation.
func withRun(c *cobra.Command, run func(*cobra.Command, []string) error) *cobra.Command {
	c.RunE = run
	return c
}

func newWhoamiCmd() *cobra.Command {
	c := withRun(leaf("whoami", "Show the signed-in user, organisations, active organisation and session file path", cobra.NoArgs), runWhoami)
	c.Flags().Bool("refresh", false, "fetch the profile and organisations from Atlas first")
	return c
}

func newLoginCmd() *cobra.Command {
	c := withRun(leaf("login", "Sign in with the Atlas device grant (same server as the desktop app)", cobra.NoArgs), runLogin)
	c.Flags().Bool("no-browser", false, "print the verification URL and code instead of opening a browser")
	return c
}

func newOrgCmd() *cobra.Command {
	create := withRun(leaf("create <name>", "Create an organisation", cobra.ExactArgs(1)), runOrgCreate)
	create.Flags().String("slug", "", "the organisation's handle (default: derived from the name)")
	return group("org", "List, choose or create organisations",
		withRun(leaf("list", "List the organisations you belong to", cobra.NoArgs), runOrgList),
		withRun(leaf("use <org-id|none>", "Set the active organisation locally", cobra.ExactArgs(1)), runOrgUse),
		create,
	)
}

func newInitCmd() *cobra.Command {
	c := withRun(leaf("init", "Bind this folder to an organisation and a cloud Project, and write .atlas/", cobra.NoArgs), runInit)
	c.Flags().String("org", "", "organisation id (skips the picker)")
	c.Flags().String("project", "", "connect to this cloud Project id (skips matching)")
	c.Flags().Bool("create", false, "create a new cloud Project for this folder")
	c.Flags().String("slug", "", "handle for a new Project (default: the folder name)")
	c.Flags().String("name", "", "name for a new Project (default: the folder name)")
	c.Flags().String("visibility", "org", "who sees a new Project: org or restricted")
	c.Flags().Bool("local", false, "bind locally only; nothing is synced")
	c.Flags().Bool("yes", false, "do not prompt for confirmation")
	c.MarkFlagsMutuallyExclusive("project", "create")
	c.MarkFlagsMutuallyExclusive("local", "project")
	c.MarkFlagsMutuallyExclusive("local", "create")
	return c
}

func newMemoryCmd() *cobra.Command {
	return group("memory", "Shared memory for agents in this repository",
		withRun(leaf("enable", "Turn shared memory on and install the agents' MCP entries", cobra.NoArgs), runMemoryGate(true)),
		withRun(leaf("disable", "Turn shared memory off and remove the agents' MCP entries", cobra.NoArgs), runMemoryGate(false)),
		withRun(leaf("verify", "Verify the memory revision hash chain", cobra.NoArgs), runMemoryVerify),
		withRun(leaf("export <dir>", "Export durable memories as an Agent Memory Repo (MEMORY.md and one file per kind)", cobra.ExactArgs(1)), runMemoryExport),
		withRun(leaf("import <file-or-dir>", "Import Agent Memory Repo files as candidate memories", cobra.ExactArgs(1)), runMemoryImport),
	)
}

func newCaptureCmd() *cobra.Command {
	return group("capture", "Session capture for agents run in this folder",
		withRun(leaf("enable", "Turn capture on and install hooks and the status line", cobra.NoArgs), runCaptureGate(true)),
		withRun(leaf("disable", "Turn capture off and remove hooks and the status line", cobra.NoArgs), runCaptureGate(false)),
		leaf("retry", "Re-queue sessions that failed to sync", cobra.NoArgs),
	)
}

func newShellCmd() *cobra.Command {
	c := withRun(leaf("shell", "Start a subshell with the capture proxy and memory server running", cobra.NoArgs), runShell)
	c.Flags().String("agent", "all", "agents to route through the proxy: claude, codex, gemini or all")
	return c
}

func newEnvCmd() *cobra.Command {
	c := withRun(leaf("env", "Print the environment for a running listener, for eval", cobra.NoArgs), runEnv)
	c.Flags().String("shell", "sh", "output syntax: sh or fish")
	return c
}

func newServeCmd() *cobra.Command {
	c := withRun(leaf("serve", "Run the listener (memory server and capture proxy) without a subshell", cobra.NoArgs), runServe)
	c.Flags().Bool("daemon", false, "start the listener detached")
	c.Flags().Bool("stop", false, "stop a detached listener")
	c.MarkFlagsMutuallyExclusive("daemon", "stop")
	return c
}

func newRunCmd() *cobra.Command {
	return leaf("run <agent>", "Drive an ACP agent from the registry headlessly, with full capture", cobra.MaximumNArgs(1))
}

func newMCPBridgeCmd() *cobra.Command {
	c := withRun(leaf("mcp-bridge [url]", "Carry MCP over stdio to the local memory server (used by agents)", cobra.MaximumNArgs(1)), runMCPBridge)
	c.Hidden = true
	c.Flags().String("project", "", "launch dir whose listener to reach")
	c.Flags().String("agent", "", "agent id for the memory grant")
	return c
}

// hookEvents lists the hook entry points each ambient adapter installs (SPEC §7.4).
var hookEvents = map[string][]string{
	"claude": {"session-start", "user-prompt-submit", "stop", "session-end"},
	"gemini": {"session-start"},
	"codex":  {"notify"},
}

func newHookCmd() *cobra.Command {
	c := group("hook", "Hook entry points called by agents")
	c.Hidden = true
	for _, agent := range []string{"claude", "gemini", "codex"} {
		events := make([]*cobra.Command, 0, len(hookEvents[agent]))
		for _, ev := range hookEvents[agent] {
			h := leaf(ev, fmt.Sprintf("%s %s hook", agent, ev), cobra.NoArgs)
			if agent == "claude" {
				h.RunE = runHook(agent, ev)
			}
			events = append(events, h)
		}
		c.AddCommand(group(agent, "Hooks for "+agent, events...))
	}
	return c
}

func newStatuslineCmd() *cobra.Command {
	c := group("statusline", "Status line commands called by agents",
		withRun(leaf("claude", "Print the Claude Code status line", cobra.NoArgs), runStatusline),
	)
	c.Hidden = true
	return c
}

// commandPaths lists every runnable command path below root, for tests and docs.
func commandPaths(root *cobra.Command) []string {
	var out []string
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() && c != root {
			out = append(out, strings.TrimPrefix(c.CommandPath(), root.Name()+" "))
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return out
}
