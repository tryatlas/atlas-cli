package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/clistate"
	"github.com/tryatlas/atlas-cli/internal/hooks"
	"github.com/tryatlas/atlas-cli/internal/hooks/mcpconfig"
	"github.com/tryatlas/atlas-cli/internal/project"
	"github.com/tryatlas/atlas-cli/internal/tui"
	"github.com/tryatlas/atlas-cli/internal/update"
)

// uninstallTarget is the binary uninstall deletes; replaced in tests.
var uninstallTarget = update.Executable

// atlas-cli uninstall: undo what the CLI set up on this machine. Every Project
// it kept state for (and the one here) loses its listener, its agent hooks and
// memory entries, and its git hook; then the CLI's state directory and the
// binary go. Each Project's .atlas/ and the sign-in are left alone: the Atlas
// desktop app reads both.
func newUninstallCmd() *cobra.Command {
	c := withRun(leaf("uninstall", "Remove Atlas hooks from every Project, stop the listeners and delete atlas-cli", cobra.NoArgs), runUninstall)
	c.Flags().Bool("yes", false, "do not prompt for confirmation")
	c.Flags().Bool("keep-binary", false, "clean up the Projects and the CLI's state, but leave the atlas-cli binary")
	return c
}

func runUninstall(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	yes, _ := cmd.Flags().GetBool("yes")
	keep, _ := cmd.Flags().GetBool("keep-binary")
	roots := uninstallRoots(ctx)
	exe, exeErr := uninstallTarget()

	if interactive() {
		tui.Header(out, "uninstall")
		fmt.Fprintln(out)
	}
	fmt.Fprintln(out, "This removes:")
	for _, r := range roots {
		fmt.Fprintf(out, "  - Atlas's agent hooks, memory entries and git hook in %s\n", r)
	}
	fmt.Fprintf(out, "  - the CLI's state in %s\n", clistate.Base())
	if !keep && exeErr == nil {
		fmt.Fprintf(out, "  - the binary %s\n", exe)
	}
	fmt.Fprintln(out, "Each Project's .atlas/ folder and your sign-in stay (the Atlas desktop app uses them);")
	fmt.Fprintln(out, "run atlas-cli logout first to sign out too.")
	if err := confirm(cmd, yes, "Uninstall atlas-cli?"); err != nil {
		return err
	}
	fmt.Fprintln(out)

	w := out
	if interactive() {
		w = agentLines(out)
	}
	var errs []error
	for _, r := range roots {
		if err := cleanProject(ctx, w, r); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r, err))
		}
	}
	if err := os.RemoveAll(clistate.Base()); err != nil {
		errs = append(errs, err)
	} else {
		fmt.Fprintln(w, "Removed the CLI's state.")
	}
	switch {
	case keep:
	case exeErr != nil:
		errs = append(errs, fmt.Errorf("could not find the atlas-cli binary to delete: %w", exeErr))
	default:
		if err := os.Remove(exe); err != nil {
			errs = append(errs, fmt.Errorf("%w (delete %s by hand, or rerun with write access to it)", err, exe))
		} else {
			fmt.Fprintf(w, "Deleted %s.\n", exe)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if interactive() {
		fmt.Fprintln(out)
		tui.Note(out, "atlas-cli is uninstalled.")
	} else {
		fmt.Fprintln(out, "atlas-cli is uninstalled.")
	}
	return nil
}

// uninstallRoots are the Projects to clean: those with CLI state, and the one
// here, each once and only while the folder still exists.
func uninstallRoots(ctx context.Context) []string {
	roots := clistate.Roots()
	if cwd, err := os.Getwd(); err == nil {
		if p, err := project.Discover(ctx, cwd); err == nil {
			roots = append(roots, p.Root)
		}
	}
	slices.Sort(roots)
	roots = slices.Compact(roots)
	return slices.DeleteFunc(roots, func(r string) bool {
		fi, err := os.Stat(r)
		return err != nil || !fi.IsDir()
	})
}

// cleanProject stops a Project's background listener and takes Atlas's
// entries out of every agent config and the git hooks.
func cleanProject(ctx context.Context, out io.Writer, root string) error {
	var errs []error
	if pid, err := haltListener(root); err != nil {
		errs = append(errs, err)
	} else if pid != 0 {
		fmt.Fprintf(out, "Stopped the listener for %s (pid %d).\n", root, pid)
	}
	for _, a := range hooks.All() {
		mw, hasMem := mcpconfig.For(a.ID())
		hooked := a.State(root) != hooks.Absent
		mem := hasMem && mw.State(root) != hooks.Absent
		if !hooked && !mem {
			continue
		}
		if hooked {
			if err := a.Uninstall(root); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", a.ID(), err))
				continue
			}
		}
		if mem {
			if err := mw.Remove(root); err != nil {
				errs = append(errs, fmt.Errorf("%s memory config: %w", a.ID(), err))
				continue
			}
		}
		fmt.Fprintf(out, "%s: Atlas hook and memory entry removed from %s.\n", a.Name(), root)
	}
	if hooks.GitHooksState(ctx, root) != hooks.Absent {
		if err := hooks.UninstallGitHooks(ctx, root); err != nil {
			errs = append(errs, fmt.Errorf("git hooks: %w", err))
		} else {
			fmt.Fprintf(out, "Removed the git post-commit hook from %s.\n", root)
		}
	}
	return errors.Join(errs...)
}
