package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	gitlink "github.com/tryatlas/atlas-cli/internal/capture/git"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/project"
)

// runCaptureLink walks new commits and links them to captured sessions.
func runCaptureLink(cmd *cobra.Command, _ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Discover(cmd.Context(), cwd)
	if err != nil {
		return err
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
	res, err := gitlink.Link(cmd.Context(), st, p.Root, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Checked %d commit(s); %d checkpoint(s) written.\n", res.Commits, res.Checkpoints)
	return nil
}
