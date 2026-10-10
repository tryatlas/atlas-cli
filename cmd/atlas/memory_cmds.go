package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/listener"
	"github.com/tryatlas/atlas-cli/internal/memory/record"
	"github.com/tryatlas/atlas-cli/internal/memory/server"
	"github.com/tryatlas/atlas-cli/internal/project"
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

// runMCPBridge carries MCP over stdio. With a URL (the desktop's form) it
// posts there with ATLAS_MCP_TOKEN; otherwise it reaches the listener of
// --project over its Unix socket, or serves memory in this process when no
// listener runs.
func runMCPBridge(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	in, out := cmd.InOrStdin(), cmd.OutOrStdout()
	if len(args) == 1 {
		f, err := server.NewTCPForwarder(args[0], os.Getenv("ATLAS_MCP_TOKEN"))
		if err != nil {
			return err
		}
		return server.RunBridge(ctx, in, out, f)
	}
	agent, _ := cmd.Flags().GetString("agent")
	if agent == "" {
		agent = "unknown"
	}
	dir, _ := cmd.Flags().GetString("project")
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if dir == "" {
		dir = cwd
	}
	root, err := project.Canonical(dir)
	if err != nil {
		return err
	}
	if c, err := project.Canonical(cwd); err == nil {
		if rel, err := filepath.Rel(root, c); err == nil && !filepath.IsAbs(rel) && rel != ".." && (len(rel) < 3 || rel[:3] != "../") {
			cwd = c
		} else {
			cwd = root
		}
	}
	g := server.Grant{SessionID: server.UUID(), Agent: agent, Cwd: cwd}
	if p, err := project.Discover(ctx, root); err == nil {
		if rf := listener.ReadRunFile(p.Root); rf != nil && dialable(rf.Socket) {
			return server.RunBridge(ctx, in, out, server.NewSocketForwarder(rf.Socket, g))
		}
	}
	srv := server.New(version)
	defer srv.Close()
	return server.RunBridge(ctx, in, out, server.NewLocalForwarder(srv, g))
}

func dialable(sock string) bool {
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// runServe runs the listener in the foreground until interrupted.
func runServe(cmd *cobra.Command, _ []string) error {
	if d, _ := cmd.Flags().GetBool("daemon"); d {
		return notImplementedError{path: "atlas serve --daemon"}
	}
	if s, _ := cmd.Flags().GetBool("stop"); s {
		return notImplementedError{path: "atlas serve --stop"}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Discover(cmd.Context(), cwd)
	if err != nil {
		return err
	}
	l, err := listener.Start(p.Root, version, listener.Options{})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Serving shared memory for %s on %s. Ctrl-C to stop.\n", p.Root, l.Socket)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sig:
	case <-cmd.Context().Done():
	}
	return l.Stop()
}
