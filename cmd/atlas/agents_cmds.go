package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/hooks"
	"github.com/tryatlas/atlas-cli/internal/hooks/mcpconfig"
	"github.com/tryatlas/atlas-cli/internal/project"
	"github.com/tryatlas/atlas-cli/internal/tui"
)

// atlas agents: the agents that run in their own UIs and report to Atlas
// through their hooks (ADR-0013). add writes the hook into the agent's
// config in this Project and the atlas_memory entry into its MCP config;
// remove takes both out; list shows every supported agent's state.
func newAgentsCmd() *cobra.Command {
	add := withRun(leaf("add <agent>...", "Install the Atlas hook and memory server into the agent's config in this Project", cobra.MinimumNArgs(1)), runAgentsAdd)
	add.Flags().Bool("force", false, "rewrite every Atlas entry even when it looks current")
	add.Flags().Bool("no-memory", false, "install the capture hook only, not the memory server entry")
	add.Flags().Bool("all", false, "every agent detected on this machine")
	remove := withRun(leaf("remove <agent>...", "Remove the Atlas hook and memory server entry from the agent's config", cobra.MinimumNArgs(1)), runAgentsRemove)
	remove.Flags().Bool("all", false, "every agent with an Atlas entry here")
	list := withRun(leaf("list", "List the supported agents and their state in this Project", cobra.NoArgs), runAgentsList)
	return group("agents", "Agents run in their own UIs, captured through their hooks", add, remove, list,
		withRun(leaf("sessions", "List the sessions the hooks have seen in this Project", cobra.NoArgs), runAgentsSessions))
}

// agentRoot is the Project the agent commands act on.
func agentRoot(ctx context.Context) (*project.Project, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	p, err := project.Discover(ctx, cwd)
	if errors.Is(err, project.ErrNotInitialised) {
		return nil, errNoProject
	}
	return p, err
}

func resolveAgents(args []string, all bool, pick func(hooks.Agent) bool) ([]hooks.Agent, error) {
	if all {
		var out []hooks.Agent
		for _, a := range hooks.All() {
			if pick(a) {
				out = append(out, a)
			}
		}
		if len(out) == 0 {
			return nil, errors.New("no agent matched; atlas agents list shows them")
		}
		return out, nil
	}
	var out []hooks.Agent
	for _, id := range args {
		a, ok := hooks.Get(id)
		if !ok {
			return nil, fmt.Errorf("%s is not a supported agent (atlas agents list)", id)
		}
		out = append(out, a)
	}
	return out, nil
}

func runAgentsAdd(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	p, err := agentRoot(ctx)
	if err != nil {
		return err
	}
	force, _ := cmd.Flags().GetBool("force")
	noMem, _ := cmd.Flags().GetBool("no-memory")
	all, _ := cmd.Flags().GetBool("all")
	agents, err := resolveAgents(args, all, func(a hooks.Agent) bool { return a.Detect(p.Root) })
	if err != nil {
		return err
	}
	w := out
	if interactive() {
		tui.Header(out, "agents", "add")
		fmt.Fprintln(out)
		w = agentLines(out)
	}
	if err := installAgents(ctx, w, p.Root, agents, force, noMem); err != nil {
		return err
	}
	if interactive() {
		fmt.Fprintln(out)
		tui.Note(out, "Use the agents as usual in "+p.Root+"; sessions are captured as they run. atlas status shows them.")
	} else {
		fmt.Fprintf(out, "Use the agents as usual in %s; sessions are captured as they run. atlas status shows them.\n", p.Root)
	}
	return nil
}

// installAgents writes the git hook, then each agent's hook and memory entry,
// one line per agent.
func installAgents(ctx context.Context, out io.Writer, root string, agents []hooks.Agent, force, noMem bool) error {
	if changed, err := hooks.InstallGitHooks(ctx, root); err != nil {
		return fmt.Errorf("git hooks: %w", err)
	} else if changed {
		fmt.Fprintln(out, "Installed the git post-commit hook (links sessions to commits).")
	}
	for _, a := range agents {
		changed, err := a.Install(root, force)
		if err != nil {
			return fmt.Errorf("%s: %w", a.ID(), err)
		}
		what := "already installed"
		if changed {
			what = "installed"
		}
		line := fmt.Sprintf("%s: hook %s", a.Name(), what)
		if w, ok := mcpconfig.For(a.ID()); ok && !noMem {
			mchanged, err := w.Write(root)
			if err != nil {
				return fmt.Errorf("%s memory config: %w", a.ID(), err)
			}
			if mchanged {
				line += ", memory server added to " + strings.TrimPrefix(w.Path(root), root+"/")
			} else {
				line += ", memory server present"
			}
		} else if !ok && !noMem {
			line += " (no memory config writer for this agent yet)"
		}
		fmt.Fprintln(out, line)
		if _, err := exec.LookPath(a.Binary()); err != nil {
			fmt.Fprintf(out, "  note: %s is not on PATH here\n", a.Binary())
		}
		if n, ok := a.(hooks.Noter); ok {
			fmt.Fprintf(out, "  note: %s\n", n.AddNote())
		}
	}
	return nil
}

func runAgentsRemove(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	p, err := agentRoot(ctx)
	if err != nil {
		return err
	}
	all, _ := cmd.Flags().GetBool("all")
	agents, err := resolveAgents(args, all, func(a hooks.Agent) bool { return a.State(p.Root) != hooks.Absent })
	if err != nil {
		return err
	}
	if interactive() {
		tui.Header(out, "agents", "remove")
		fmt.Fprintln(out)
		out = agentLines(out)
	}
	return removeAgents(ctx, out, p.Root, agents)
}

// agentLines styles installAgents' and removeAgents' lines on a terminal:
// "Claude Code: hook installed, …" becomes "✓ Claude Code  hook installed, …",
// notes go faint under it.
func agentLines(w io.Writer) io.Writer {
	return tui.Lines(w, func(l string) string {
		if n, ok := strings.CutPrefix(l, "  note: "); ok {
			return "    " + tui.Default.Faint.Render("↳ "+n)
		}
		if name, rest, ok := strings.Cut(l, ": "); ok && !strings.Contains(name, " the ") {
			return tui.Default.Success.Render("✓") + " " + tui.Default.Bold.Render(fmt.Sprintf("%-20s", name)) + " " + tui.Default.Faint.Render(rest)
		}
		return tui.Default.Success.Render("✓") + " " + l
	})
}

// removeAgents takes the Atlas entries out of each agent's config, and the
// git hook once no agent is left.
func removeAgents(ctx context.Context, out io.Writer, root string, agents []hooks.Agent) error {
	for _, a := range agents {
		if err := a.Uninstall(root); err != nil {
			return fmt.Errorf("%s: %w", a.ID(), err)
		}
		if w, ok := mcpconfig.For(a.ID()); ok {
			if err := w.Remove(root); err != nil {
				return fmt.Errorf("%s memory config: %w", a.ID(), err)
			}
		}
		fmt.Fprintf(out, "%s: Atlas hook and memory entry removed.\n", a.Name())
	}
	left := false
	for _, a := range hooks.All() {
		if a.State(root) != hooks.Absent {
			left = true
		}
	}
	if !left {
		if err := hooks.UninstallGitHooks(ctx, root); err != nil {
			return err
		}
		fmt.Fprintln(out, "No agent left: the git post-commit hook was removed too.")
	}
	return nil
}

func runAgentsList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	root := ""
	if p, err := agentRoot(ctx); err == nil {
		root = p.Root
	}
	if interactive() {
		return agentsTable(cmd.OutOrStdout(), root)
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tNAME\tBINARY\tHOOK\tMEMORY")
	for _, a := range hooks.All() {
		bin := "not on PATH"
		if _, err := exec.LookPath(a.Binary()); err == nil {
			bin = a.Binary()
		}
		hook, mem := "-", "-"
		if root != "" {
			hook = a.State(root).String()
			if mw, ok := mcpconfig.For(a.ID()); ok {
				mem = mw.State(root).String()
			} else {
				mem = "no writer"
			}
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", a.ID(), a.Name(), bin, hook, mem)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if root == "" {
		fmt.Fprintln(cmd.OutOrStdout(), "Not in an Atlas Project: run atlas init, then atlas agents add <agent>.")
	}
	return nil
}

// agentsTable is atlas agents list on a terminal.
func agentsTable(out io.Writer, root string) error {
	tui.Header(out, "agents")
	fmt.Fprintln(out)
	state := func(st hooks.InstallState) string {
		switch st {
		case hooks.Current:
			return tui.Dot(tui.Good, "installed")
		case hooks.Outdated:
			return tui.Dot(tui.Warn, "outdated")
		}
		return tui.Dot(tui.Idle, "off")
	}
	var rows [][]string
	for _, a := range hooks.All() {
		bin := tui.Default.Faint.Render("not on PATH")
		if _, err := exec.LookPath(a.Binary()); err == nil {
			bin = a.Binary()
		}
		hook, mem := tui.Default.Faint.Render("-"), tui.Default.Faint.Render("-")
		if root != "" {
			hook = state(a.State(root))
			if mw, ok := mcpconfig.For(a.ID()); ok {
				mem = state(mw.State(root))
			}
		}
		rows = append(rows, []string{a.Name(), tui.Default.Faint.Render(a.ID()), bin, hook, mem})
	}
	fmt.Fprint(out, tui.Table([]string{"agent", "id", "binary", "hook", "memory"}, rows))
	fmt.Fprintln(out)
	if root == "" {
		tui.Note(out, "Not in an Atlas Project: run atlas init, then atlas agents add <agent>.")
	} else {
		tui.Note(out, "atlas agents add <id> · atlas agents add --all · atlas settings")
	}
	return nil
}

func runAgentsSessions(cmd *cobra.Command, _ []string) error {
	p, err := agentRoot(cmd.Context())
	if err != nil {
		return err
	}
	states, err := hooks.ListStates(p.Root, "")
	if err != nil {
		return err
	}
	if len(states) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No session seen by the hooks yet.")
		return nil
	}
	if interactive() {
		out := cmd.OutOrStdout()
		tui.Header(out, "agents", "sessions")
		fmt.Fprintln(out)
		var rows [][]string
		for _, s := range states {
			st := tui.Dot(tui.Good, "idle")
			switch {
			case s.Ended:
				st = tui.Dot(tui.Idle, "ended")
			case s.InTurn:
				st = tui.Dot(tui.Warn, "in a turn")
			}
			resume := ""
			if a, ok := hooks.Get(s.Agent); ok {
				resume = a.ResumeCommand(s.SessionID)
			}
			rows = append(rows, []string{s.Agent, tui.Fit(s.SessionID, 28), fmt.Sprint(s.Turn), st, ago(s.UpdatedAt), tui.Default.Faint.Render(resume)})
		}
		fmt.Fprint(out, tui.Table([]string{"agent", "session", "turns", "state", "last", "resume"}, rows))
		return nil
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tSESSION\tTURNS\tSTATE\tLAST\tRESUME")
	for _, s := range states {
		state := "idle"
		switch {
		case s.Ended:
			state = "ended"
		case s.InTurn:
			state = "in a turn"
		}
		if s.Pending > 0 {
			state += fmt.Sprintf(", %d spooled", s.Pending)
		}
		resume := ""
		if a, ok := hooks.Get(s.Agent); ok {
			resume = a.ResumeCommand(s.SessionID)
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\t%s\n", s.Agent, s.SessionID, s.Turn, state, ago(s.UpdatedAt), resume)
	}
	return w.Flush()
}

func ago(t time.Time) string {
	d := time.Since(t).Round(time.Minute)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return t.Format("2006-01-02")
}

// printAgents is the Agents line of atlas status: per agent, hook and
// memory state in this Project, and the last hook error if any.
func printAgents(ctx context.Context, w io.Writer, root string) {
	var on []string
	for _, a := range hooks.All() {
		st := a.State(root)
		if st == hooks.Absent {
			continue
		}
		s := a.ID()
		if st == hooks.Outdated {
			s += " (hook outdated: atlas agents add " + a.ID() + ")"
		}
		if mw, ok := mcpconfig.For(a.ID()); ok && mw.State(root) != hooks.Current {
			s += " (no memory entry)"
		}
		on = append(on, s)
	}
	if len(on) == 0 {
		fmt.Fprintln(w, "Agents:    none hooked here (atlas agents add <agent>)")
		return
	}
	fmt.Fprintf(w, "Agents:    %s\n", strings.Join(on, ", "))
	if hooks.GitHooksState(ctx, root) != hooks.Current {
		fmt.Fprintln(w, "Git hook:  post-commit not installed (atlas agents add <agent> installs it)")
	}
	if states, _ := hooks.ListStates(root, ""); len(states) > 0 {
		live, spooled := 0, 0
		for _, s := range states {
			if !s.Ended {
				live++
			}
			spooled += s.Pending
		}
		line := fmt.Sprintf("%d session(s) seen by hooks, %d open", len(states), live)
		if spooled > 0 {
			line += fmt.Sprintf(", %d turn(s) spooled for the next writer", spooled)
		}
		fmt.Fprintf(w, "Hooks:     %s\n", line)
	}
	if e := hooks.LastError(root); e != "" {
		fmt.Fprintf(w, "Hook err:  %s", e)
	}
}
