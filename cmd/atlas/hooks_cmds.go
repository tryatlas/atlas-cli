package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	gitlink "github.com/tryatlas/atlas-cli/internal/capture/git"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	capsync "github.com/tryatlas/atlas-cli/internal/capture/sync"
	"github.com/tryatlas/atlas-cli/internal/hooks"
	"github.com/tryatlas/atlas-cli/internal/listener"
	"github.com/tryatlas/atlas-cli/internal/project"

	// The agent integrations register themselves.
	_ "github.com/tryatlas/atlas-cli/internal/hooks/antigravity"
	_ "github.com/tryatlas/atlas-cli/internal/hooks/claude"
	_ "github.com/tryatlas/atlas-cli/internal/hooks/codex"
	_ "github.com/tryatlas/atlas-cli/internal/hooks/copilot"
	_ "github.com/tryatlas/atlas-cli/internal/hooks/cursor"
	_ "github.com/tryatlas/atlas-cli/internal/hooks/droid"
	_ "github.com/tryatlas/atlas-cli/internal/hooks/opencode"
	_ "github.com/tryatlas/atlas-cli/internal/hooks/pi"
)

// atlas hooks <agent> <verb> is what the installed agent hooks run; atlas
// hooks git <hook> is what the git hooks run. Both are hidden, read their
// payload on stdin, never block the agent, and never exit non-zero: an error
// goes to .atlas/cli-hooks.log and shows in atlas status (ADR-0013 §4).
func newHooksCmd() *cobra.Command {
	root := &cobra.Command{Use: "hooks", Short: "Entry points for agent and git hooks (installed by atlas agents add)", Hidden: true, Args: cobra.NoArgs}
	for _, a := range hooks.All() {
		ag := &cobra.Command{Use: a.ID(), Short: a.Name() + " hooks", Hidden: true, Args: cobra.NoArgs}
		for _, verb := range a.Verbs() {
			ag.AddCommand(&cobra.Command{Use: verb, Hidden: true, Args: cobra.NoArgs, RunE: runAgentHook(a, verb)})
		}
		root.AddCommand(ag)
	}
	git := &cobra.Command{Use: "git", Short: "git hooks", Hidden: true, Args: cobra.NoArgs}
	git.AddCommand(&cobra.Command{Use: "post-commit", Hidden: true, Args: cobra.ArbitraryArgs, RunE: runGitPostCommit})
	root.AddCommand(git)
	return root
}

// runAgentHook handles one agent hook invocation.
func runAgentHook(a hooks.Agent, verb string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		payload, err := hooks.ReadPayload(cmd.InOrStdin())
		if err != nil {
			return nil //nolint:nilerr // an unreadable payload must not fail the agent
		}
		ev, err := a.Parse(verb, payload)
		if err != nil || ev == nil {
			return nil //nolint:nilerr // a payload that is not ours
		}
		limit := hooks.OtherDeadline
		if ev.Kind == hooks.TurnEnd {
			limit = hooks.TurnEndDeadline
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), limit)
		defer cancel()
		cwd := ev.Cwd
		if cwd == "" {
			if cwd, err = os.Getwd(); err != nil {
				return nil //nolint:nilerr // nowhere to record
			}
		}
		p, err := project.Discover(ctx, cwd)
		if err != nil {
			return nil //nolint:nilerr // not an Atlas Project: silent
		}
		if !hooks.Enabled(ctx, p.Root) {
			return nil
		}
		d := &hooks.Dispatcher{Root: p.Root, Version: version, Now: time.Now, Deliver: hooks.Deliver(version, syncAfterHook), Banner: banner(ctx, p.Root)}
		out, err := d.Run(ctx, a, ev)
		if err != nil {
			hooks.LogError(p.Root, a.ID(), verb, err)
		}
		if len(out) > 0 {
			_, _ = cmd.OutOrStdout().Write(out)
		}
		return nil
	}
}

// syncAfterHook sends what a direct hook write just recorded to the cloud,
// within the drain's short budget, so a turn reaches the Project without
// waiting for atlas serve or atlas capture sync. Offline or signed out it
// does nothing; the rows stay pending for the next drain.
func syncAfterHook(ctx context.Context, st *store.Store) {
	b, err := st.Binding(ctx)
	if err != nil || b == nil {
		return
	}
	if _, ok := capsync.TargetOf(b); !ok || b.DrainState == store.DrainNotAuthorized {
		return
	}
	_, _ = capsync.Drain(ctx, syncClient(), st, capsync.Budget, time.Now)
}

// banner is the SessionStart message: what is on, and which Project.
func banner(ctx context.Context, root string) string {
	st, err := store.Open(root, false)
	if err != nil {
		return ""
	}
	defer st.Close()
	b, err := st.Binding(ctx)
	if err != nil || b == nil {
		return ""
	}
	mem := "memory on"
	if !project.MemorySharing(root) {
		mem = "memory off"
	}
	where := "local Project"
	if b.Slug != nil && *b.Slug != "" {
		where = *b.Slug
	}
	return "Atlas: capture on · " + mem + " · " + where
}

// runGitPostCommit links captured sessions to the new commit: through the
// running listener when one holds the store, else directly.
func runGitPostCommit(cmd *cobra.Command, _ []string) error {
	ctx, cancel := context.WithTimeout(cmd.Context(), hooks.TurnEndDeadline)
	defer cancel()
	cwd, err := os.Getwd()
	if err != nil {
		return nil //nolint:nilerr // nowhere to link
	}
	p, err := project.Discover(ctx, cwd)
	if err != nil || !store.Exists(p.Root) {
		return nil //nolint:nilerr // not an Atlas Project: silent
	}
	if rf := listener.ReadRunFile(p.Root); rf != nil {
		body, _ := json.Marshal(listener.SyncRequest{Link: true})
		if err := postControl(ctx, rf.Socket, body); err == nil {
			return nil
		}
	}
	lock, err := store.Lock(p.Root)
	if err != nil {
		if !errors.Is(err, store.ErrLocked) {
			hooks.LogError(p.Root, "git", "post-commit", err)
		}
		return nil // the desktop app links on its own
	}
	defer func() { _ = lock.Release() }()
	st, err := store.Open(p.Root, false)
	if err != nil {
		hooks.LogError(p.Root, "git", "post-commit", err)
		return nil
	}
	defer st.Close()
	if _, err := gitlink.Link(ctx, st, p.Root, time.Now); err != nil {
		hooks.LogError(p.Root, "git", "post-commit", err)
	}
	return nil
}

// drainOutput is a sink for hook commands that must print nothing.
var _ io.Writer = io.Discard

// errNoProject is returned by agent commands run outside a Project.
var errNoProject = fmt.Errorf("this folder is not an Atlas Project; run atlas init first")
