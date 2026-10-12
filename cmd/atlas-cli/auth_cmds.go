package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tryatlas/cli/internal/auth"
	"github.com/tryatlas/cli/internal/tui"
)

// newAuth is replaced in tests.
var newAuth = auth.New

func runLogin(cmd *cobra.Command, _ []string) error {
	a, err := newAuth()
	if err != nil {
		return err
	}
	noBrowser, _ := cmd.Flags().GetBool("no-browser")
	out := cmd.OutOrStdout()
	if interactive() {
		var s *auth.Session
		err := tui.DeviceLogin(func(prompt func(tui.DevicePrompt)) (string, error) {
			var err error
			s, err = a.Login(cmd.Context(), func(p auth.Prompt) {
				opened := !noBrowser && canOpenBrowser() && openBrowser(p.VerificationURI) == nil
				prompt(tui.DevicePrompt{URL: p.VerificationURI, Code: p.UserCode, Expires: p.ExpiresAt, Opened: opened})
			})
			if err != nil {
				return "", err
			}
			if s.Identity != nil {
				return fmt.Sprintf("%s <%s>", s.Identity.Name, s.Identity.Email), nil
			}
			return "you", nil
		})
		if err != nil {
			return err
		}
		fmt.Fprintln(out)
		printIdentity(out, s, a.Store.Path())
		return nil
	}
	s, err := a.Login(cmd.Context(), func(p auth.Prompt) {
		fmt.Fprintf(out, "To sign in, open %s and enter the code:\n\n    %s\n\n", p.VerificationURI, p.UserCode)
		if !noBrowser && canOpenBrowser() {
			if openBrowser(p.VerificationURI) == nil {
				fmt.Fprintln(out, "(opened your browser)")
			}
		}
		fmt.Fprintf(out, "Waiting for approval (the code expires at %s)…\n", p.ExpiresAt.Local().Format("15:04"))
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(out)
	printIdentity(out, s, a.Store.Path())
	return nil
}

func runLogout(cmd *cobra.Command, _ []string) error {
	a, err := newAuth()
	if err != nil {
		return err
	}
	return tui.Work(cmd.OutOrStdout(), "Signing out", func(tui.StepFunc) (string, error) {
		confirmed, err := a.Logout(cmd.Context())
		switch {
		case errors.Is(err, auth.ErrSignedOut):
			return "Not signed in.", nil
		case err != nil:
			return "", err
		case confirmed:
			return "Signed out.", nil
		}
		return "Signed out on this machine. Atlas did not confirm the server session ended; it expires on its own.", nil
	})
}

func runWhoami(cmd *cobra.Command, _ []string) error {
	a, err := newAuth()
	if err != nil {
		return err
	}
	refresh, _ := cmd.Flags().GetBool("refresh")
	s, err := a.Session()
	if err != nil {
		return err
	}
	if refresh {
		var r *auth.Session
		var rerr error
		_ = tui.Work(io.Discard, "Refreshing your profile", func(tui.StepFunc) (string, error) {
			r, rerr = a.Refresh(cmd.Context())
			return "", nil
		})
		if errors.Is(rerr, auth.ErrSignedOut) {
			return rerr
		}
		if r != nil {
			s = r
		}
		if rerr != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "atlas-cli: could not refresh:", rerr)
		}
	}
	printIdentity(cmd.OutOrStdout(), s, a.Store.Path())
	return nil
}

func runOrgList(cmd *cobra.Command, _ []string) error {
	a, err := newAuth()
	if err != nil {
		return err
	}
	var s *auth.Session
	_ = tui.Work(io.Discard, "Fetching your organisations", func(tui.StepFunc) (string, error) {
		s, err = a.Refresh(cmd.Context())
		return "", nil
	})
	if errors.Is(err, auth.ErrSignedOut) {
		return err
	}
	if s == nil {
		return err
	}
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "atlas-cli: showing the stored list; could not refresh:", err)
	}
	printOrgs(cmd.OutOrStdout(), s)
	return nil
}

func runOrgUse(cmd *cobra.Command, args []string) error {
	a, err := newAuth()
	if err != nil {
		return err
	}
	id := args[0]
	if id == "none" {
		id = ""
	}
	s, err := a.SetActiveOrg(id)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	if id == "" {
		tui.Done(out, "No organisation pinned.")
		return nil
	}
	if s.ActiveOrgID() != id {
		fmt.Fprintf(cmd.ErrOrStderr(), "atlas-cli: %s is not in your organisation list; pinned anyway. Run atlas-cli org list to refresh it.\n", id)
	}
	tui.Done(out, "Active organisation: "+orgLabel(s, id))
	return nil
}

func runOrgCreate(cmd *cobra.Command, args []string) error {
	a, err := newAuth()
	if err != nil {
		return err
	}
	s, err := a.Session()
	if err != nil {
		return err
	}
	name := strings.TrimSpace(args[0])
	slug, _ := cmd.Flags().GetString("slug")
	if slug == "" {
		slug = slugify(name)
	}
	if slug == "" {
		return fmt.Errorf("cannot make a handle from %q; pass --slug", name)
	}
	ctx := cmd.Context()
	return tui.Work(cmd.OutOrStdout(), "Creating "+name, func(step tui.StepFunc) (string, error) {
		step("checking the handle " + slug)
		free, err := a.Client.CheckSlug(ctx, s.SessionToken, slug)
		if err != nil {
			return "", err
		}
		if !free {
			return "", fmt.Errorf("the handle %q is taken; pass another with --slug", slug)
		}
		step("creating the organisation")
		created, err := a.Client.CreateOrg(ctx, s.SessionToken, name, slug)
		if err != nil {
			var f *auth.Failure
			if errors.As(err, &f) && f.Kind == auth.Denied {
				return "", errors.New("couldn't create it: that organisation name or handle may already be taken")
			}
			return "", err
		}
		step("refreshing your organisations")
		if _, err := a.Refresh(ctx); err != nil && !errors.Is(err, auth.ErrSignedOut) {
			fmt.Fprintln(cmd.ErrOrStderr(), "atlas-cli: created, but could not refresh the list:", err)
		}
		return fmt.Sprintf("Created %s (%s). Run atlas-cli org use %s to make it active.", created.Name, created.ID, created.ID), nil
	})
}

var slugJunk = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	return strings.Trim(slugJunk.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

func printIdentity(w io.Writer, s *auth.Session, path string) {
	if s == nil {
		return
	}
	if interactive() {
		tui.Header(w, "account")
		tui.Section(w, "Signed in")
		if id := s.Identity; id != nil {
			tui.Field(w, "Name", tui.Default.Bold.Render(id.Name))
			tui.Field(w, "Email", id.Email)
			tui.Field(w, "User id", tui.Default.Faint.Render(id.ID))
		} else {
			tui.Field(w, "Profile", "not fetched yet (atlas-cli whoami --refresh)")
		}
		printOrgs(w, s)
		tui.Section(w, "On this machine")
		tui.Field(w, "Session file", tui.Default.Faint.Render(path))
		return
	}
	if id := s.Identity; id != nil {
		fmt.Fprintf(w, "Signed in as %s <%s> (%s)\n", id.Name, id.Email, id.ID)
	} else {
		fmt.Fprintln(w, "Signed in (profile not fetched yet; run atlas-cli whoami --refresh)")
	}
	printOrgs(w, s)
	fmt.Fprintf(w, "Session file: %s\n", path)
}

func printOrgs(w io.Writer, s *auth.Session) {
	id := s.Identity
	if interactive() && id != nil && id.Orgs != nil && len(*id.Orgs) > 0 {
		tui.Section(w, "Organisations")
		var rows [][]string
		for _, o := range *id.Orgs {
			role := ""
			if o.Role != nil {
				role = string(*o.Role)
			}
			state := tui.Default.Off.Render("○")
			if o.ID == s.ActiveOrgID() {
				state = tui.Default.Success.Render("●") + " active"
			}
			rows = append(rows, []string{o.Name, tui.Default.Faint.Render(o.ID), role, state})
		}
		fmt.Fprint(w, tui.Table([]string{"name", "id", "role", ""}, rows))
		return
	}
	switch {
	case id == nil || id.Orgs == nil:
		fmt.Fprintln(w, "Organisations: not known yet")
		return
	case len(*id.Orgs) == 0:
		fmt.Fprintln(w, "Organisations: none")
		return
	}
	active := s.ActiveOrgID()
	fmt.Fprintln(w, "Organisations:")
	for _, o := range *id.Orgs {
		mark := " "
		if o.ID == active {
			mark = "*"
		}
		role := ""
		if o.Role != nil {
			role = "  " + string(*o.Role)
		}
		fmt.Fprintf(w, "  %s %s (%s)%s\n", mark, o.Name, o.ID, role)
	}
}

func orgLabel(s *auth.Session, id string) string {
	if s.Identity != nil && s.Identity.Orgs != nil {
		for _, o := range *s.Identity.Orgs {
			if o.ID == id {
				return fmt.Sprintf("%s (%s)", o.Name, o.ID)
			}
		}
	}
	return id
}

// canOpenBrowser is false on a server: over SSH or without a display.
func canOpenBrowser() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	switch runtime.GOOS {
	case "darwin":
		return true
	case "linux":
		return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
	}
	return false
}

// openBrowser opens the plain verification URL, never the one carrying the code.
func openBrowser(url string) error {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	c := exec.CommandContext(context.Background(), name, url) //nolint:gosec // G204: the URL is the auth server's verification_uri
	c.Stdout, c.Stderr = io.Discard, io.Discard
	return c.Start()
}
