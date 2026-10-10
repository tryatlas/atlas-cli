package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/agents/ambient"
	"github.com/tryatlas/atlas-cli/internal/auth"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/hosts"
	"github.com/tryatlas/atlas-cli/internal/ingest"
	"github.com/tryatlas/atlas-cli/internal/project"
	"github.com/tryatlas/atlas-cli/internal/tui"
)

// Seams replaced in tests.
var (
	interactive = tui.Interactive
	choose      = tui.Select
	now         = time.Now
)

// lockedHelp follows the desktop's "Another Atlas window is recording this Project".
const lockedHelp = "another Atlas window is recording this Project; close the folder in the Atlas app (or stop the other atlas shell) and try again"

func runInit(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	flags := cmd.Flags()
	local, _ := flags.GetBool("local")
	yes, _ := flags.GetBool("yes")

	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	launch, err := project.Canonical(cwd)
	if err != nil {
		return err
	}
	scope := project.ScopeRoot(ctx, launch)

	if launch != scope {
		fmt.Fprintf(errOut, "Note: %s is inside the repository %s.\n"+
			"Capture is recorded for this folder only; shared memory is shared with the whole repository.\n", launch, scope)
		if err := confirm(cmd, yes, "Bind this subfolder as its own Project?"); err != nil {
			return err
		}
	}

	// The session file is needed for a cloud binding, not for a local one.
	var a *auth.Auth
	var sess *auth.Session
	if !local {
		if a, err = newAuth(); err != nil {
			return err
		}
		if sess, err = a.Refresh(ctx); errors.Is(err, auth.ErrSignedOut) {
			return err
		} else if sess == nil {
			return err
		}
	}

	lock, err := store.Lock(launch)
	if errors.Is(err, store.ErrLocked) {
		return errors.New(lockedHelp)
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	st, err := store.Open(launch, true)
	if err != nil {
		return err
	}
	defer st.Close()

	if prev, err := st.Binding(ctx); err != nil {
		return err
	} else if prev != nil {
		fmt.Fprintf(errOut, "This folder is already bound (%s).\n", describeBinding(prev, sess))
		if err := confirm(cmd, yes, "Bind it again?"); err != nil {
			return err
		}
	}

	fp := project.Fingerprint(ctx, launch)
	b := store.Binding{
		WorkspaceID: launch, Root: launch, Enabled: true, DrainState: store.DrainOK,
		RootCommitSHA: fp.RootCommitSHA, FingerprintIsShallow: fp.Shallow, GitURL: fp.GitURL,
	}

	if local {
		b.Mode, b.ImportApproved = store.ModeLocal, true
		if err := st.PutBinding(ctx, b, now()); err != nil {
			return err
		}
		fmt.Fprintf(out, "Bound %s locally. Nothing is synced.\n", launch)
		return nil
	}

	org, err := pickOrg(cmd, sess)
	if err != nil {
		return err
	}
	ic := ingest.New(hosts.Ingest(), a.Tokens())
	ws, how, err := pickProject(cmd, ic, org, launch, fp)
	if err != nil {
		return err
	}
	b.OrgID, b.RemoteWorkspaceID = &org.ID, &ws.ID
	if ws.Slug != "" {
		b.Slug = &ws.Slug
	}
	// A cloud Project imports on-disk transcripts only after the user confirms
	// the disclosure in the app (evidence-capture.md §7.5), so this stays 0.
	b.ImportApproved = false
	if err := st.BindCloud(ctx, b, now()); err != nil {
		return err
	}
	fmt.Fprintf(out, "Bound %s to %s / %s (%s).\n", launch, org.Name, ws.Label(), how)
	if launch != scope {
		fmt.Fprintf(out, "Shared memory for this folder lives at %s.\n", filepath.Join(scope, ".atlas", "memory"))
	}
	return nil
}

// confirm asks a yes/no question; without a terminal, --yes is required.
func confirm(cmd *cobra.Command, yes bool, question string) error {
	if yes {
		return nil
	}
	if !interactive() {
		return errors.New("pass --yes to continue without a prompt")
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", question)
	line, _ := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		return nil
	}
	return tui.ErrCancelled
}

func pickOrg(cmd *cobra.Command, s *auth.Session) (auth.Org, error) {
	want, _ := cmd.Flags().GetString("org")
	var orgs []auth.Org
	if s.Identity != nil && s.Identity.Orgs != nil {
		orgs = *s.Identity.Orgs
	}
	if len(orgs) == 0 {
		return auth.Org{}, errors.New("you are not in any organisation; create one with atlas org create <name>, or use atlas init --local")
	}
	if want != "" {
		for _, o := range orgs {
			if o.ID == want {
				return o, nil
			}
		}
		return auth.Org{}, fmt.Errorf("%s is not one of your organisations (atlas org list)", want)
	}
	if len(orgs) == 1 {
		return orgs[0], nil
	}
	if !interactive() {
		return auth.Org{}, errors.New("you belong to several organisations; pass --org <id> (atlas org list)")
	}
	labels := make([]string, len(orgs))
	initial := 0
	for i, o := range orgs {
		labels[i] = fmt.Sprintf("%s (%s)", o.Name, o.ID)
		if o.ID == s.ActiveOrgID() {
			initial = i
		}
	}
	i, err := choose("Which organisation?", labels, initial)
	if err != nil {
		return auth.Org{}, err
	}
	return orgs[i], nil
}

// pickProject chooses the cloud Project: an explicit --project, a new one with
// --create, else the server's fingerprint match, else a choice from the list.
func pickProject(cmd *cobra.Command, ic *ingest.Client, org auth.Org, launch string, fp project.GitFingerprint) (ingest.Workspace, string, error) {
	ctx := cmd.Context()
	flags := cmd.Flags()
	explicit, _ := flags.GetString("project")
	create, _ := flags.GetBool("create")

	connect := func(id string, slug *string) (ingest.Workspace, string, error) {
		res, err := ic.Connect(ctx, ingest.ConnectRequest{OrgID: org.ID, WorkspaceID: &id, RootCommitSHA: fp.RootCommitSHA, GitURL: fp.GitURL})
		if err != nil {
			return ingest.Workspace{}, "", err
		}
		if res.Status != "connected" {
			return ingest.Workspace{}, "", fmt.Errorf("the server did not connect Project %s (status %q)", id, res.Status)
		}
		ws := ingest.Workspace{ID: res.WorkspaceID, Slug: deref(res.Slug)}
		if ws.Slug == "" {
			ws.Slug = deref(slug)
		}
		return ws, "connected: " + res.Reason, nil
	}

	switch {
	case explicit != "":
		return connect(explicit, nil)
	case create:
		return createProject(cmd, ic, org, launch, fp)
	}

	if fp.RootCommitSHA != nil || fp.GitURL != nil {
		res, err := ic.Connect(ctx, ingest.ConnectRequest{OrgID: org.ID, RootCommitSHA: fp.RootCommitSHA, GitURL: fp.GitURL})
		if err != nil {
			return ingest.Workspace{}, "", err
		}
		switch res.Status {
		case "connected":
			return ingest.Workspace{ID: res.WorkspaceID, Slug: deref(res.Slug)}, "matched by " + res.Reason, nil
		case "ambiguous":
			if !interactive() {
				return ingest.Workspace{}, "", fmt.Errorf("several Projects match this repository (%s); pass --project <id>", workspaceList(res.Candidates))
			}
			i, err := choose("Several Projects match this repository. Which one?", workspaceLabels(res.Candidates), 0)
			if err != nil {
				return ingest.Workspace{}, "", err
			}
			c := res.Candidates[i]
			return connect(c.ID, &c.Slug)
		}
	}

	list, err := ic.Workspaces(ctx, org.ID)
	if err != nil {
		return ingest.Workspace{}, "", err
	}
	if !interactive() {
		if len(list) == 0 {
			return ingest.Workspace{}, "", errors.New("no Project matches this repository; pass --create to make one")
		}
		return ingest.Workspace{}, "", fmt.Errorf("no Project matches this repository; pass --project <id> (%s) or --create", workspaceList(list))
	}
	newLabel := fmt.Sprintf("Create a new Project “%s”", defaultSlug(cmd, launch))
	i, err := choose("Which Project in "+org.Name+"?", append(workspaceLabels(list), newLabel), len(list))
	if err != nil {
		return ingest.Workspace{}, "", err
	}
	if i == len(list) {
		return createProject(cmd, ic, org, launch, fp)
	}
	return connect(list[i].ID, &list[i].Slug)
}

func createProject(cmd *cobra.Command, ic *ingest.Client, org auth.Org, launch string, fp project.GitFingerprint) (ingest.Workspace, string, error) {
	ctx := cmd.Context()
	slug := defaultSlug(cmd, launch)
	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		name = filepath.Base(launch)
	}
	vis, _ := cmd.Flags().GetString("visibility")
	if vis != "org" && vis != "restricted" {
		return ingest.Workspace{}, "", fmt.Errorf("--visibility must be org or restricted, not %q", vis)
	}
	if free, err := ic.SlugAvailable(ctx, org.ID, slug); err != nil {
		return ingest.Workspace{}, "", err
	} else if !free {
		return ingest.Workspace{}, "", fmt.Errorf("the Project handle %q is taken in %s; pass another with --slug", slug, org.Name)
	}
	id, err := ic.Create(ctx, ingest.CreateRequest{OrgID: org.ID, Slug: slug, Visibility: vis, Name: &name,
		RootCommitSHA: fp.RootCommitSHA, GitURL: fp.GitURL})
	if err != nil {
		return ingest.Workspace{}, "", err
	}
	return ingest.Workspace{ID: id, Slug: slug, Name: &name}, "created", nil
}

func defaultSlug(cmd *cobra.Command, launch string) string {
	if s, _ := cmd.Flags().GetString("slug"); s != "" {
		return s
	}
	return slugify(filepath.Base(launch))
}

func workspaceLabels(ws []ingest.Workspace) []string {
	out := make([]string, len(ws))
	for i, w := range ws {
		out[i] = fmt.Sprintf("%s (%s)", w.Label(), w.ID)
	}
	return out
}

func workspaceList(ws []ingest.Workspace) string { return strings.Join(workspaceLabels(ws), ", ") }

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func describeBinding(b *store.Binding, s *auth.Session) string {
	if b.Mode != store.ModeCloud {
		return "local, not synced"
	}
	org := deref(b.OrgID)
	if s != nil && s.Identity != nil && s.Identity.Orgs != nil {
		for _, o := range *s.Identity.Orgs {
			if o.ID == org {
				org = o.Name
			}
		}
	}
	proj := deref(b.Slug)
	if proj == "" {
		proj = deref(b.RemoteWorkspaceID)
	}
	return fmt.Sprintf("cloud: %s / %s", org, proj)
}

// ── Gates ────────────────────────────────────────────────────────────────────

// gateDir is the folder whose gates a command flips: the discovered Project,
// else the cwd.
func gateDir(ctx context.Context) (string, *project.Project, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", nil, err
	}
	p, err := project.Discover(ctx, cwd)
	if err == nil {
		return p.Root, p, nil
	}
	if !errors.Is(err, project.ErrNotInitialised) {
		return "", nil, err
	}
	c, err := project.Canonical(cwd)
	return c, nil, err
}

func runMemoryGate(enabled bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		dir, _, err := gateDir(cmd.Context())
		if err != nil {
			return err
		}
		if err := project.SetMemorySharing(dir, enabled); err != nil {
			return err
		}
		state := "off"
		if enabled {
			state = "on"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Shared memory is %s for %s.\n", state, dir)
		return applyAdapters(cmd, dir, func(c ambient.Claude, st *ambient.State) (string, error) {
			if enabled {
				return "Claude Code: MCP server \"atlas\" in .mcp.json", c.InstallMCP(st)
			}
			return "Claude Code: MCP server removed", c.RemoveMCP(st)
		}, enabled)
	}
}

func runCaptureGate(enabled bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		_, p, err := gateDir(ctx)
		if err != nil {
			return err
		}
		if p == nil {
			return project.ErrNotInitialised
		}
		lock, err := store.Lock(p.Root)
		if errors.Is(err, store.ErrLocked) {
			return errors.New(lockedHelp)
		}
		if err != nil {
			return err
		}
		defer func() { _ = lock.Release() }()
		st, err := store.Open(p.Root, false)
		if err != nil {
			return err
		}
		defer st.Close()
		if err := st.SetEnabled(ctx, enabled, now()); err != nil {
			return err
		}
		if enabled {
			fmt.Fprintf(cmd.OutOrStdout(), "Capture is on for %s.\n", p.Root)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Capture is paused for %s. What was recorded still syncs.\n", p.Root)
		}
		return applyAdapters(cmd, p.Root, func(c ambient.Claude, st *ambient.State) (string, error) {
			if enabled {
				if err := c.InstallHooks(st); err != nil {
					return "", err
				}
				st.SetCaptureProxy(true)
				return "Claude Code: hooks and status line in .claude/settings.json; run claude inside atlas shell", c.InstallStatus(st)
			}
			if err := c.RemoveHooks(st); err != nil {
				return "", err
			}
			return "Claude Code: hooks and status line removed", c.RemoveStatus(st)
		}, enabled)
	}
}

// ── Status ───────────────────────────────────────────────────────────────────

func runStatus(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()
	var sess *auth.Session
	if a, err := newAuth(); err == nil {
		if s, err := a.Session(); err == nil {
			sess = s
			who := "signed in"
			if s.Identity != nil {
				who = fmt.Sprintf("%s <%s>", s.Identity.Name, s.Identity.Email)
			}
			fmt.Fprintf(w, "Account:   %s\n", who)
		} else {
			fmt.Fprintln(w, "Account:   not signed in (atlas login)")
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Discover(ctx, cwd)
	if errors.Is(err, project.ErrNotInitialised) {
		fmt.Fprintln(w, "Project:   not initialised here (atlas init)")
		dir, _ := project.Canonical(cwd)
		printGates(w, dir, nil)
		return nil
	}
	if err != nil {
		return err
	}
	st, err := store.Open(p.Root, false)
	if err != nil {
		return err
	}
	defer st.Close()
	b, err := st.Binding(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "Project:   %s\n", p.Root)
	fmt.Fprintf(w, "Binding:   %s\n", describeBinding(b, sess))
	if p.ScopeRoot != p.Root {
		fmt.Fprintf(w, "Memory at: %s (shared with the whole repository)\n", p.ScopeRoot)
	}
	printGates(w, p.Root, b)
	fmt.Fprintln(w, "Memory sync: this machine only")
	if l, err := store.Lock(p.Root); errors.Is(err, store.ErrLocked) {
		fmt.Fprintln(w, "Recorder:  another Atlas window (or atlas shell) is recording this Project")
	} else if err == nil {
		_ = l.Release()
		fmt.Fprintln(w, "Recorder:  idle")
	}
	return nil
}

func printGates(w io.Writer, dir string, b *store.Binding) {
	mem := "on"
	if !project.MemorySharing(dir) {
		mem = "off"
	}
	fmt.Fprintf(w, "Memory:    %s\n", mem)
	switch {
	case b == nil:
		fmt.Fprintln(w, "Capture:   off (not initialised)")
	case b.Enabled:
		fmt.Fprintln(w, "Capture:   on")
	default:
		fmt.Fprintln(w, "Capture:   paused")
	}
}

// applyAdapters runs an adapter change for each agent the Project uses and
// records it in .atlas/cli.json. Installing needs the agent to be present
// (on PATH, or already configured in the folder); removing always runs.
func applyAdapters(cmd *cobra.Command, dir string, apply func(ambient.Claude, *ambient.State) (string, error), installing bool) error {
	st, err := ambient.LoadState(dir)
	if err != nil {
		return err
	}
	c := ambient.Claude{Dir: dir, Atlas: ambient.AtlasCommand()}
	if installing && !c.Detect() && !exists(filepath.Join(dir, ".claude")) && !exists(filepath.Join(dir, ".mcp.json")) {
		fmt.Fprintln(cmd.OutOrStdout(), "Claude Code not found; nothing to configure.")
		return st.Save()
	}
	msg, err := apply(c, st)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), msg)
	return st.Save()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
