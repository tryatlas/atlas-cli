package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/tryatlas/cli/internal/agents/acp"
	atlasrun "github.com/tryatlas/cli/internal/run"
)

// newACPCmd is the parked ACP path (ADR-0013 §8): the registry, installs,
// atlas-cli acp run with the chat screen, and the session/load importer.
func newACPCmd() *cobra.Command {
	list := withRun(leaf("list", "List the ACP agent registry: installed and available agents", cobra.NoArgs), runACPList)
	list.Flags().Bool("refresh", false, "fetch the registry now instead of using the hourly cache")
	install := withRun(leaf("install <agent>", "Install a registry agent (it downloads on first run)", cobra.ExactArgs(1)), runAgentsInstall)
	install.Flags().Bool("detected", false, "use the copy found on PATH instead of downloading (see atlas-cli acp detect)")
	uninstall := withRun(leaf("uninstall <agent>", "Remove an installed agent", cobra.ExactArgs(1)), runAgentsUninstall)
	uninstall.Flags().Bool("purge", false, "also delete its downloaded files")
	c := group("acp", "ACP agents run by Atlas (parked; see atlas-cli agents for the hook path)",
		list, install, uninstall,
		withRun(leaf("detect", "Find registry agents already on PATH", cobra.NoArgs), runAgentsDetect),
		newAgentsCheckCmd(),
		newAgentsSessionsCmd(),
		withRun(leaf("logout <agent>", "Sign the agent out, when it supports it", cobra.ExactArgs(1)), runAgentsLogout),
		newRunCmd(),
		newCaptureImportCmd(),
	)
	c.Hidden = true
	return c
}

func agentsStore() atlasrun.Agents {
	return atlasrun.Agents{Version: version, Progress: func(s string) { fmt.Fprintln(os.Stderr, s) }}
}

func runACPList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	a := agentsStore()
	force, _ := cmd.Flags().GetBool("refresh")
	reg, rerr := a.Catalog(ctx, force)
	if reg == nil {
		return rerr
	}
	if rerr != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "atlas-cli:", rerr)
	}
	inst, _, err := a.Installed()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "AGENT\tVERSION\tSTATE\tNAME")
	plat := acp.PlatformKey()
	seen := map[string]bool{}
	for _, e := range reg.Agents {
		seen[e.ID] = true
		state := "available"
		switch {
		case inst[e.ID] != nil && inst[e.ID].Type == acp.TypeCustom:
			state = "installed (custom)"
		case inst[e.ID] != nil:
			state = "installed"
		case e.KindOn(plat) == acp.KindNone:
			state = "not for " + plat
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", e.ID, e.Version, state, e.Name)
	}
	var extra []string
	for id := range inst {
		if !seen[id] {
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		fmt.Fprintf(w, "%s\t-\tinstalled (%s)\t\n", id, inst[id].Type)
	}
	return w.Flush()
}

// detected maps registry ids to a binary found on PATH: agents with a
// binary target for this platform whose cmd is not node; npx-only agents
// are never detected.
func detected(reg *acp.Registry) map[string]string {
	out := map[string]string{}
	plat := acp.PlatformKey()
	for _, e := range reg.Agents {
		t, ok := e.Distribution.Binary[plat]
		if !ok || t.Cmd == "node" {
			continue
		}
		base := filepath.Base(strings.ReplaceAll(t.Cmd, `\`, "/"))
		if p, err := exec.LookPath(base); err == nil {
			if abs, err := filepath.Abs(p); err == nil {
				out[e.ID] = abs
			}
		}
	}
	return out
}

func runAgentsDetect(cmd *cobra.Command, _ []string) error {
	reg, err := agentsStore().Catalog(cmd.Context(), false)
	if reg == nil {
		return err
	}
	found := detected(reg)
	if len(found) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No registry agent found on PATH.")
		return nil
	}
	ids := make([]string, 0, len(found))
	for id := range found {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", id, found[id])
	}
	fmt.Fprintln(cmd.OutOrStdout(), "Use one with: atlas-cli acp install <agent> --detected")
	return nil
}

func runAgentsInstall(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	a := agentsStore()
	id := args[0]
	reg, rerr := a.Catalog(ctx, false)
	if reg == nil {
		return rerr
	}
	e, ok := reg.Find(id)
	if !ok {
		if reg, rerr = a.Catalog(ctx, true); reg == nil {
			return rerr
		}
		if e, ok = reg.Find(id); !ok {
			return fmt.Errorf("%s: %w (atlas-cli acp list)", id, acp.ErrNotInRegistry)
		}
	}
	if !e.Supported() {
		return fmt.Errorf("%s does not run on %s", id, acp.PlatformKey())
	}
	m, path, err := a.Installed()
	if err != nil {
		return err
	}
	entry := &acp.Installed{Type: acp.TypeRegistry}
	if d, _ := cmd.Flags().GetBool("detected"); d {
		p, ok := detected(reg)[id]
		if !ok {
			return fmt.Errorf("%s was not found on PATH", id)
		}
		entry = &acp.Installed{Type: acp.TypeCustom, Command: p, Args: e.Distribution.Binary[acp.PlatformKey()].Args}
	}
	if prev := m[id]; prev != nil {
		entry.Env, entry.DefaultMode = prev.Env, prev.DefaultMode
	}
	m[id] = entry
	if err := m.Save(path); err != nil {
		return err
	}
	if entry.Type == acp.TypeCustom {
		fmt.Fprintf(cmd.OutOrStdout(), "Installed %s from %s: atlas-cli acp run %s\n", e.Name, entry.Command, id)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Installed %s %s. It downloads on first run: atlas-cli acp run %s\n", e.Name, e.Version, id)
	return nil
}

func runAgentsUninstall(cmd *cobra.Command, args []string) error {
	a := agentsStore()
	id := args[0]
	m, path, err := a.Installed()
	if err != nil {
		return err
	}
	if m[id] == nil {
		return fmt.Errorf("%s: %w", id, acp.ErrNotInstalled)
	}
	delete(m, id)
	if err := m.Save(path); err != nil {
		return err
	}
	if p, _ := cmd.Flags().GetBool("purge"); p {
		data, err := acp.DataDir()
		if err == nil {
			_ = os.RemoveAll(filepath.Join(data, "external-agents", "registry", acp.Sanitize(id)))
			_ = os.RemoveAll(filepath.Join(data, "external-agents", "registry", "npx", acp.Sanitize(id)))
		}
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Uninstalled %s.\n", id)
	return nil
}

func runAgentsLogout(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	id := args[0]
	m, _, err := agentsStore().Installed()
	if err != nil {
		return err
	}
	if m[id] == nil {
		return fmt.Errorf("%s: %w", id, acp.ErrNotInstalled)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	ag, _, err := agentsStore().Launch(ctx, id, cwd, nopHost{})
	if err != nil {
		return err
	}
	defer ag.Close()
	if err := ag.Logout(ctx); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Signed %s out.\n", ag.Name())
	return nil
}

// nopHost serves a connection that opens no session.
type nopHost struct{}

func (nopHost) Update(string, acp.Update) {}
func (nopHost) Permission(context.Context, acp.PermissionRequest) (string, bool) {
	return "", false
}
func (nopHost) Elicit(context.Context, acp.Elicitation) acp.ElicitationAnswer {
	return acp.ElicitationAnswer{Action: "cancel"}
}
func (nopHost) ElicitationComplete(string) {}

var errNoTUI = errors.New("the chat screen needs a terminal; pass --prompt \"…\" --no-tui to run one prompt")

func newAgentsCheckCmd() *cobra.Command {
	c := withRun(leaf("check [agent...]", "Install, launch and handshake agents; report capabilities and sign-in", cobra.ArbitraryArgs), runAgentsCheck)
	c.Flags().Bool("all", false, "check every registry agent that runs on this machine")
	c.Flags().Duration("timeout", 5*time.Minute, "per-agent limit, download included")
	return c
}

func runAgentsCheck(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	a := agentsStore()
	a.Progress = nil
	reg, rerr := a.Catalog(ctx, false)
	if reg == nil {
		return rerr
	}
	all, _ := cmd.Flags().GetBool("all")
	limit, _ := cmd.Flags().GetDuration("timeout")
	var entries []acp.Entry
	switch {
	case all:
		for _, e := range reg.Agents {
			if e.Supported() {
				entries = append(entries, e)
			}
		}
	case len(args) == 0:
		return errors.New("name agents to check, or pass --all")
	default:
		for _, id := range args {
			e, ok := reg.Find(id)
			if !ok {
				return fmt.Errorf("%s: %w", id, acp.ErrNotInRegistry)
			}
			entries = append(entries, e)
		}
	}
	w := cmd.OutOrStdout()
	failed := 0
	for _, e := range entries {
		fmt.Fprintf(cmd.ErrOrStderr(), "checking %s…\n", e.ID)
		r := a.Check(ctx, e, limit)
		if r.Err != nil {
			failed++
			fmt.Fprintf(w, "%s %s (%s): FAILED %v [%s]\n", r.ID, r.Version, r.Kind, firstErrLine(r.Err), r.Took)
			continue
		}
		fmt.Fprintf(w, "%s %s (%s): %s · protocol %d · session: %s · caps: %s · sign-in: %s [%s]\n", r.ID, r.Version, r.Kind,
			r.Agent, r.Protocol, r.Session, strings.Join(r.Caps, ","), strings.Join(r.Auth, ", "), r.Took)
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d agent(s) failed", failed, len(entries))
	}
	return nil
}

func firstErrLine(err error) string {
	s, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func newAgentsSessionsCmd() *cobra.Command {
	c := withRun(leaf("sessions <agent>", "List the agent's own sessions for this folder (session/list)", cobra.ExactArgs(1)), runACPSessions)
	c.Flags().Bool("all-folders", false, "every session the agent lists, not only this folder's")
	return c
}

// runACPSessions asks an installed agent for its sessions in this folder.
func runACPSessions(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if c, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = c
	}
	ag, _, err := agentsStore().Launch(ctx, args[0], cwd, nopHost{})
	if err != nil {
		return err
	}
	defer ag.Close()
	if !ag.Init.AgentCapabilities.CanList() {
		return fmt.Errorf("%s does not list its sessions (no sessionCapabilities.list)", ag.Name())
	}
	cursor := ""
	n := 0
	for range 20 {
		filter := cwd
		if all, _ := cmd.Flags().GetBool("all-folders"); all {
			filter = ""
		}
		list, next, err := ag.ListSessions(ctx, filter, cursor)
		if err != nil {
			return err
		}
		for _, s := range list {
			n++
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", s.SessionID, s.UpdatedAt, s.Cwd, firstLineOf(s.Title))
		}
		if next == "" {
			break
		}
		cursor = next
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "%d session(s) in %s\n", n, cwd)
	return nil
}

func firstLineOf(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 80 {
		s = s[:80] + "…"
	}
	return s
}
