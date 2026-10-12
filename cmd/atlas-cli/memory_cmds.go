package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/tryatlas/cli/internal/hooks"
	"github.com/tryatlas/cli/internal/listener"
	"github.com/tryatlas/cli/internal/memory/record"
	"github.com/tryatlas/cli/internal/memory/server"
	"github.com/tryatlas/cli/internal/project"
	"github.com/tryatlas/cli/internal/tui"
	"github.com/tryatlas/cli/internal/tui/memview"
)

// openRecord opens the shared-memory record of the repository around the cwd.
func openRecord(cmd *cobra.Command, create bool) (*record.Store, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	scope := project.ScopeRoot(cmd.Context(), cwd)
	st, err := record.Open(scope, create)
	if errors.Is(err, record.ErrNoRecord) {
		return nil, fmt.Errorf("no shared memory yet for %s", scope)
	}
	return st, err
}

func runMemoryVerify(cmd *cobra.Command, _ []string) error {
	st, err := openRecord(cmd, false)
	if err != nil {
		return err
	}
	defer st.Close()
	return tui.Work(cmd.OutOrStdout(), "Verifying the memory revision chain", func(tui.StepFunc) (string, error) {
		rep, err := st.VerifyChain()
		if err != nil {
			return "", err
		}
		if !rep.OK() {
			return "", fmt.Errorf("the revision chain is broken at revision %d (%d revisions checked): history was edited outside Atlas", rep.FirstBad, rep.Revisions)
		}
		return fmt.Sprintf("Revision chain intact: %d revisions in %s", rep.Revisions, record.Path(st.ScopeRoot)), nil
	})
}

func runMemoryExport(cmd *cobra.Command, args []string) error {
	st, err := openRecord(cmd, false)
	if err != nil {
		return err
	}
	defer st.Close()
	return tui.Work(cmd.OutOrStdout(), "Exporting shared memory", func(tui.StepFunc) (string, error) {
		n, err := st.Export(cmd.Context(), args[0])
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Exported %d memories to %s (MEMORY.md, decisions.md, architecture.md, facts.md, failures.md).", n, args[0]), nil
	})
}

func runMemoryImport(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if !project.MemorySharing(gateRoot(cmd, cwd)) {
		return errors.New("shared memory is switched off for this project (atlas-cli memory enable)")
	}
	st, err := openRecord(cmd, true)
	if err != nil {
		return err
	}
	defer st.Close()
	return tui.Work(cmd.OutOrStdout(), "Importing memories", func(tui.StepFunc) (string, error) {
		res, err := st.Import(cmd.Context(), args[0], now().UnixMilli())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Imported %d memories as candidates (%d already held). Agents see them as unconfirmed until one restates them.", res.Imported, res.Skipped), nil
	})
}

func gateRoot(cmd *cobra.Command, cwd string) string {
	if p, err := project.Discover(cmd.Context(), cwd); err == nil {
		return p.Root
	}
	return cwd
}

// runMCPBridge carries MCP over stdio to the memory server. With a URL it is
// the desktop's bridge (ADR-0019): the session's bearer in ATLAS_MCP_TOKEN,
// never in argv. Without one it is the entry an agent's MCP config holds
// (ADR-0013 §6): it reaches a running listener over the owner-only socket,
// else serves memory in this process, so memory works with or without
// atlas-cli serve.
func runMCPBridge(cmd *cobra.Command, args []string) error {
	if len(args) == 1 {
		f, err := server.NewTCPForwarder(args[0], os.Getenv("ATLAS_MCP_TOKEN"))
		if err != nil {
			return err
		}
		return server.RunBridge(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), f)
	}
	root, _ := cmd.Flags().GetString("project")
	agentID, _ := cmd.Flags().GetString("agent")
	if root == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		root = cwd
	}
	if p, err := project.Discover(cmd.Context(), root); err == nil {
		root = p.Root
	} else if c, err := project.Canonical(root); err == nil {
		root = c
	}
	if agentID == "" {
		agentID = "unknown"
	}
	g := server.Grant{Agent: agentID, Cwd: root, SessionID: hooks.CurrentSession(root, agentID)}
	if rf := listener.ReadRunFile(root); rf != nil {
		return server.RunBridge(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), server.NewUnixForwarder(rf.Socket, g))
	}
	srv := server.New(version)
	defer srv.Close()
	return server.RunBridge(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), server.NewLocalForwarder(srv, g))
}

// runMemoryBrowse opens the memory browser.
func runMemoryBrowse(cmd *cobra.Command, _ []string) error {
	if !interactive() {
		return errors.New("the browser needs a terminal; use atlas-cli memory list")
	}
	st, err := openRecord(cmd, false)
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = tea.NewProgram(memview.New(cmd.Context(), st, st.ScopeRoot), tea.WithContext(cmd.Context())).Run()
	return err
}

// runMemoryList prints entries, one per line: id, kind, status, content.
func runMemoryList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	kind, _ := cmd.Flags().GetString("kind")
	q, _ := cmd.Flags().GetString("search")
	st, err := openRecord(cmd, false)
	if err != nil {
		return err
	}
	defer st.Close()
	var kinds []string
	if kind != "" {
		kinds = []string{kind}
	}
	var es []*record.Entry
	if q != "" {
		hits, err := st.Search(ctx, q, kinds, 100)
		if err != nil {
			return err
		}
		for _, h := range hits {
			es = append(es, h.Entry)
		}
	} else if all, _ := cmd.Flags().GetBool("all"); all {
		// Candidates too, as the app's memory panel shows them.
		if es, err = st.List(ctx, kind, 10000); err != nil {
			return err
		}
	} else if es, err = st.Active(ctx, kinds...); err != nil {
		return err
	}
	if interactive() {
		out := cmd.OutOrStdout()
		tui.Header(out, "memory", "list")
		fmt.Fprintln(out)
		if len(es) == 0 {
			tui.Note(out, "No memories here yet.")
			return nil
		}
		var rows [][]string
		for _, e := range es {
			state := tui.Dot(tui.Good, e.State)
			if e.State == record.StateCandidate {
				state = tui.Dot(tui.Idle, e.State)
			}
			rows = append(rows, []string{fmt.Sprint(e.ID), strings.ReplaceAll(e.Kind, "_", " "), state,
				tui.Fit(strings.Join(strings.Fields(e.Content), " "), max(tui.Width()-50, 20))})
		}
		fmt.Fprint(out, tui.Table([]string{"id", "kind", "state", "memory"}, rows))
		tui.Note(out, fmt.Sprintf("%d memories · atlas-cli memory browse to read them", len(es)))
		return nil
	}
	for _, e := range es {
		fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\t%s\n", e.ID, e.Kind, e.State, strings.Join(strings.Fields(e.Content), " "))
	}
	return nil
}
