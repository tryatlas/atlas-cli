package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/hooks"
	"github.com/tryatlas/atlas-cli/internal/listener"
	"github.com/tryatlas/atlas-cli/internal/memory/record"
	"github.com/tryatlas/atlas-cli/internal/memory/server"
	"github.com/tryatlas/atlas-cli/internal/project"
	"github.com/tryatlas/atlas-cli/internal/tui/memview"
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
	rep, err := st.VerifyChain()
	if err != nil {
		return err
	}
	if !rep.OK() {
		return fmt.Errorf("the revision chain is broken at revision %d (%d revisions checked): history was edited outside Atlas", rep.FirstBad, rep.Revisions)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Revision chain intact: %d revisions in %s\n", rep.Revisions, record.Path(st.ScopeRoot))
	return nil
}

func runMemoryExport(cmd *cobra.Command, args []string) error {
	st, err := openRecord(cmd, false)
	if err != nil {
		return err
	}
	defer st.Close()
	n, err := st.Export(cmd.Context(), args[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Exported %d memories to %s (MEMORY.md, decisions.md, architecture.md, facts.md, failures.md).\n", n, args[0])
	return nil
}

func runMemoryImport(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if !project.MemorySharing(gateRoot(cmd, cwd)) {
		return errors.New("shared memory is switched off for this project (atlas memory enable)")
	}
	st, err := openRecord(cmd, true)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := st.Import(cmd.Context(), args[0], now().UnixMilli())
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Imported %d memories as candidates (%d already held). Agents see them as unconfirmed until one restates them.\n", res.Imported, res.Skipped)
	return nil
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
// atlas serve.
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
		return errors.New("the browser needs a terminal; use atlas memory list")
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
	} else if es, err = st.Active(ctx, kinds...); err != nil {
		return err
	}
	for _, e := range es {
		fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\t%s\t%s\n", e.ID, e.Kind, e.State, strings.Join(strings.Fields(e.Content), " "))
	}
	return nil
}
