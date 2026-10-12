package main

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/listener"
	"github.com/tryatlas/atlas-cli/internal/project"
	"github.com/tryatlas/atlas-cli/internal/tui"
	"github.com/tryatlas/atlas-cli/internal/update"
)

// Seams replaced in tests.
var (
	updateSource = update.GitHub
	updateTarget = update.Executable
)

// atlas-cli update: replace this binary with the latest release (or --version),
// verified against the release's checksums.txt like the installer does.
func newUpdateCmd() *cobra.Command {
	c := withRun(leaf("update", "Update atlas-cli to the latest release (verified against the release checksums)", cobra.NoArgs), runUpdate)
	c.Flags().Bool("check", false, "only say whether a newer release exists")
	c.Flags().String("version", "", "install this release tag instead of the latest (e.g. v0.3.4)")
	return c
}

func runUpdate(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	check, _ := cmd.Flags().GetBool("check")
	want, _ := cmd.Flags().GetString("version")
	if want != "" && want[0] != 'v' {
		want = "v" + want
	}
	if !interactive() {
		msg, err := applyUpdate(ctx, func(label, value string) { fmt.Fprintf(out, "%-10s %s\n", label+":", value) }, want, check)
		if err == nil {
			fmt.Fprintln(out, msg)
		}
		return err
	}
	title := "Updating atlas-cli"
	if check {
		title = "Checking for a newer atlas-cli"
	}
	return tui.Work(out, title, func(step tui.StepFunc) (string, error) {
		return applyUpdate(ctx, func(label, value string) { step(label + "  " + value) }, want, check)
	})
}

// applyUpdate does the work and returns the closing line; each step is
// reported to step as a label and a value.
func applyUpdate(ctx context.Context, step func(label, value string), want string, check bool) (string, error) {
	src := updateSource()
	tag := want
	if tag == "" {
		step("Checking", "the latest release on GitHub")
		latest, err := src.Latest(ctx)
		if err != nil {
			return "", fmt.Errorf("could not read the latest release: %w", err)
		}
		tag = latest
	}
	step("Installed", version)
	step("Release", tag)
	if want == "" && !update.Newer(version, tag) {
		return fmt.Sprintf("atlas-cli %s is up to date.", version), nil
	}
	if check {
		return fmt.Sprintf("atlas-cli %s is available; run atlas-cli update.", tag), nil
	}
	exe, err := updateTarget()
	if err != nil {
		return "", err
	}
	step("Downloading", update.Archive(tag, runtime.GOOS, runtime.GOARCH))
	bin, err := src.Download(ctx, tag)
	if err != nil {
		return "", err
	}
	step("Verified", update.Archive(tag, runtime.GOOS, runtime.GOARCH)+" (sha256)")
	if err := update.Replace(exe, bin); err != nil {
		return "", fmt.Errorf("%w (reinstall with the installer, or run atlas-cli update with write access to %s)", err, exe)
	}
	step("Replaced", exe)
	if note := restartListener(ctx); note != "" {
		step("Listener", note)
	}
	return fmt.Sprintf("Updated atlas-cli %s → %s.", version, tag), nil
}

// restartListener puts this Project's background listener on the new
// binary; a foreground one is left to its owner.
func restartListener(ctx context.Context) string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	p, err := project.Discover(ctx, cwd)
	if err != nil {
		return ""
	}
	rf := listener.ReadRunFile(p.Root)
	if rf == nil {
		return ""
	}
	if !rf.Daemon {
		return "a foreground atlas-cli serve still runs the old version; restart it"
	}
	if _, err := haltListener(p.Root); err != nil {
		return "could not stop the old listener: " + err.Error()
	}
	pid, _, err := launchListener(p.Root, 0)
	if err != nil {
		return "restart failed: " + err.Error()
	}
	return fmt.Sprintf("restarted on the new version (pid %d)", pid)
}
