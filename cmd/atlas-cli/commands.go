package main

import (
	"strings"

	"github.com/spf13/cobra"

	"github.com/tryatlas/cli/internal/tui"
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
		Use:           "atlas-cli",
		Short:         "Shared memory and session capture for coding agents",
		Long:          "atlas-cli gives every coding agent run in a folder Atlas shared memory and session capture,\nusing the same on-disk formats as the Atlas desktop app.",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetVersionTemplate("atlas-cli {{.Version}}\n")
	root.SetHelpFunc(styledHelp(root.HelpFunc()))
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		if !interactive() {
			return cmd.Help()
		}
		splash(cmd)
		return nil
	}

	root.AddCommand(
		newLoginCmd(),
		withRun(leaf("logout", "Sign out and delete the local session file", cobra.NoArgs), runLogout),
		newWhoamiCmd(),
		newOrgCmd(),
		newInitCmd(),
		newMemoryCmd(),
		newCaptureCmd(),
		newServeCmd(),
		newSyncCmd(),
		newSettingsCmd(),
		newUpdateCmd(),
		newUninstallCmd(),
		withRun(leaf("status", "Show login, organisation, Project, gates, capture health and agent integrations", cobra.NoArgs), runStatus),
		newAgentsCmd(),
		newHooksCmd(),
		newACPCmd(),
		newMCPBridgeCmd(),
	)
	return root
}

// splashGroups orders the splash's command list.
var splashGroups = []struct {
	title string
	cmds  []string
}{
	{"Start", []string{"login", "init", "status", "settings"}},
	{"Agents", []string{"agents", "sync"}},
	{"Memory and capture", []string{"memory", "capture"}},
	{"Account", []string{"whoami", "org", "logout", "update", "uninstall"}},
}

// splash is `atlas-cli` alone on a terminal.
func splash(cmd *cobra.Command) {
	groups := map[string][]tui.Command{}
	var order []string
	for _, g := range splashGroups {
		order = append(order, g.title)
		for _, name := range g.cmds {
			if c, _, err := cmd.Find([]string{name}); err == nil && c != cmd {
				groups[g.title] = append(groups[g.title], tui.Command{Name: name, Short: c.Short})
			}
		}
	}
	tui.Splash(cmd.OutOrStdout(), version, groups, order)
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
	c.Flags().StringSlice("agents", nil, "hook these agents (comma-separated ids, atlas-cli agents list); skips the agent step")
	c.Flags().Bool("all-agents", false, "hook every agent detected on this machine; skips the agent step")
	c.Flags().String("sync", "", "background sync: on or off (atlas-cli sync); skips the sync step")
	c.MarkFlagsMutuallyExclusive("agents", "all-agents")
	c.MarkFlagsMutuallyExclusive("project", "create")
	c.MarkFlagsMutuallyExclusive("local", "project")
	c.MarkFlagsMutuallyExclusive("local", "create")
	return c
}

func newMemoryCmd() *cobra.Command {
	return group("memory", "Shared memory for agents in this repository",
		withRun(leaf("enable", "Turn shared memory on for this repository", cobra.NoArgs), runMemoryGate(true)),
		withRun(leaf("disable", "Turn shared memory off for this repository", cobra.NoArgs), runMemoryGate(false)),
		withRun(leaf("verify", "Verify the memory revision hash chain", cobra.NoArgs), runMemoryVerify),
		withRun(leaf("browse", "Browse shared memory: entries by kind, search, history", cobra.NoArgs), runMemoryBrowse),
		newMemoryIndexCmd(),
		newMemoryListCmd(),
		withRun(leaf("export <dir>", "Export durable memories as an Agent Memory Repo (MEMORY.md and one file per kind)", cobra.ExactArgs(1)), runMemoryExport),
		withRun(leaf("import <file-or-dir>", "Import Agent Memory Repo files as candidate memories", cobra.ExactArgs(1)), runMemoryImport),
	)
}

func newCaptureCmd() *cobra.Command {
	return group("capture", "Session capture for agents run in this folder",
		withRun(leaf("enable", "Turn session capture on for this Project", cobra.NoArgs), runCaptureGate(true)),
		withRun(leaf("disable", "Pause session capture for this Project (what was recorded still syncs)", cobra.NoArgs), runCaptureGate(false)),
		withRun(leaf("sync", "Upload captured sessions to the cloud Project now", cobra.NoArgs), runCaptureSync(false)),
		withRun(leaf("retry", "Re-queue sessions that failed to sync, then sync", cobra.NoArgs), runCaptureSync(true)),
		withRun(leaf("link", "Link captured sessions to the commits that carried their work", cobra.NoArgs), runCaptureLink),
	)
}

func newServeCmd() *cobra.Command {
	c := withRun(leaf("serve", "Run the Project's listener: memory server, sync drain, and the receiver of agent hooks", cobra.NoArgs), runServe)
	c.Flags().Bool("daemon", false, "start the listener detached")
	c.Flags().Bool("stop", false, "stop a detached listener (atlas-cli sync off also keeps hooks from restarting it)")
	c.Flags().Duration("import-every", 0, "ACP import of native sessions this often (parked; 0: never)")
	_ = c.Flags().MarkHidden("import-every")
	c.MarkFlagsMutuallyExclusive("daemon", "stop")
	return c
}

func newMCPBridgeCmd() *cobra.Command {
	c := withRun(leaf("mcp-bridge [url]", "Carry MCP over stdio to the Project's memory server (used by agents' MCP configs)", cobra.MaximumNArgs(1)), runMCPBridge)
	c.Flags().String("project", "", "the Project root (an agent's MCP config passes it; default: the cwd)")
	c.Flags().String("agent", "", "the agent id the bridge serves")
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

func newMemoryListCmd() *cobra.Command {
	c := withRun(leaf("list", "Print shared memory entries (for scripts; atlas-cli memory browse on a terminal)", cobra.NoArgs), runMemoryList)
	c.Flags().String("kind", "", "only this kind: plan, file_changed, decision, fact, failure, architecture, preference")
	c.Flags().String("search", "", "search instead of listing")
	c.Flags().Bool("all", false, "include candidates (unconfirmed memories), as the app shows them")
	return c
}

func newCaptureImportCmd() *cobra.Command {
	c := withRun(leaf("import", "Replay sessions over ACP (session/list and session/load) into capture (parked)", cobra.NoArgs), runCaptureImport)
	c.Flags().StringArray("agent", nil, "only this installed agent (repeatable; default: every installed agent)")
	c.Flags().Bool("dry-run", false, "list what would be imported")
	return c
}
