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

	"github.com/tryatlas/atlas-cli/internal/auth"
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
	confirmed, err := a.Logout(cmd.Context())
	if errors.Is(err, auth.ErrSignedOut) {
		fmt.Fprintln(cmd.OutOrStdout(), "Not signed in.")
		return nil
	}
	if err != nil {
		return err
	}
	if confirmed {
		fmt.Fprintln(cmd.OutOrStdout(), "Signed out.")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "Signed out on this machine. Atlas did not confirm the server session ended; it expires on its own.")
	}
	return nil
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
		r, rerr := a.Refresh(cmd.Context())
		if errors.Is(rerr, auth.ErrSignedOut) {
			return rerr
		}
		if r != nil {
			s = r
		}
		if rerr != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "atlas: could not refresh:", rerr)
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
	s, err := a.Refresh(cmd.Context())
	if errors.Is(err, auth.ErrSignedOut) {
		return err
	}
	if s == nil {
		return err
	}
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "atlas: showing the stored list; could not refresh:", err)
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
		fmt.Fprintln(out, "No organisation pinned.")
		return nil
	}
	if s.ActiveOrgID() != id {
		fmt.Fprintf(cmd.ErrOrStderr(), "atlas: %s is not in your organisation list; pinned anyway. Run atlas org list to refresh it.\n", id)
	}
	fmt.Fprintf(out, "Active organisation: %s\n", orgLabel(s, id))
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
	free, err := a.Client.CheckSlug(ctx, s.SessionToken, slug)
	if err != nil {
		return err
	}
	if !free {
		return fmt.Errorf("the handle %q is taken; pass another with --slug", slug)
	}
	created, err := a.Client.CreateOrg(ctx, s.SessionToken, name, slug)
	if err != nil {
		var f *auth.Failure
		if errors.As(err, &f) && f.Kind == auth.Denied {
			return errors.New("couldn't create it: that organisation name or handle may already be taken")
		}
		return err
	}
	if _, err := a.Refresh(ctx); err != nil && !errors.Is(err, auth.ErrSignedOut) {
		fmt.Fprintln(cmd.ErrOrStderr(), "atlas: created, but could not refresh the list:", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Created %s (%s). Run atlas org use %s to make it active.\n", created.Name, created.ID, created.ID)
	return nil
}

var slugJunk = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	return strings.Trim(slugJunk.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

func printIdentity(w io.Writer, s *auth.Session, path string) {
	if s == nil {
		return
	}
	if id := s.Identity; id != nil {
		fmt.Fprintf(w, "Signed in as %s <%s> (%s)\n", id.Name, id.Email, id.ID)
	} else {
		fmt.Fprintln(w, "Signed in (profile not fetched yet; run atlas whoami --refresh)")
	}
	printOrgs(w, s)
	fmt.Fprintf(w, "Session file: %s\n", path)
}

func printOrgs(w io.Writer, s *auth.Session) {
	id := s.Identity
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
