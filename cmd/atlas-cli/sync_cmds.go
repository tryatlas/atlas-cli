package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/clistate"
	"github.com/tryatlas/atlas-cli/internal/listener"
	"github.com/tryatlas/atlas-cli/internal/project"
	"github.com/tryatlas/atlas-cli/internal/tui"
)

// atlas-cli sync [on|off]: background sync is a running atlas-cli serve for the
// Project. on starts it and keeps it wanted, so an agent's next hook
// restarts it after a reboot; off stops it and forgets it.
func newSyncCmd() *cobra.Command {
	return withRun(&cobra.Command{
		Use:       "sync [on|off]",
		Short:     "Turn background sync on or off for this Project (a detached atlas-cli serve), or show it",
		Args:      cobra.MatchAll(cobra.MaximumNArgs(1), cobra.OnlyValidArgs),
		ValidArgs: []string{"on", "off", "status"},
	}, runSync)
}

func runSync(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Discover(cmd.Context(), cwd)
	if errors.Is(err, project.ErrNotInitialised) {
		return errNoProject
	}
	if err != nil {
		return err
	}
	switch {
	case len(args) == 0 || args[0] == "status":
		if interactive() {
			on, note := syncState(p.Root)
			tui.Header(out, "sync")
			tui.Section(out, "Background sync")
			if on {
				tui.Field(out, "State", tui.Dot(tui.Good, "on"))
			} else {
				tui.Field(out, "State", tui.Dot(tui.Idle, "off"))
			}
			tui.Field(out, "Listener", note)
			fmt.Fprintln(out)
			tui.Note(out, "atlas-cli sync on · atlas-cli sync off")
			return nil
		}
		printSync(out, p.Root)
		return nil
	case args[0] == "on":
		return tui.Work(out, "Starting background sync", func(tui.StepFunc) (string, error) {
			return setBackgroundSync(p.Root, true)
		})
	default:
		return tui.Work(out, "Stopping background sync", func(tui.StepFunc) (string, error) {
			return setBackgroundSync(p.Root, false)
		})
	}
}

// setBackgroundSync records the choice and starts or stops the listener.
func setBackgroundSync(root string, on bool) (string, error) {
	prefs := clistate.LoadPrefs(root)
	prefs.BackgroundSync = on
	if err := clistate.SavePrefs(root, prefs); err != nil {
		return "", err
	}
	if !on {
		pid, err := haltListener(root)
		if err != nil {
			return "", err
		}
		if pid == 0 {
			return "Background sync is off.", nil
		}
		return fmt.Sprintf("Background sync is off (stopped pid %d).", pid), nil
	}
	if rf := listener.ReadRunFile(root); rf != nil {
		return fmt.Sprintf("Background sync is on (already running, pid %d).", rf.PID), nil
	}
	pid, port, err := launchListener(root, 0)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Background sync is on: listener pid %d on 127.0.0.1:%d.", pid, port), nil
}

// syncState says whether background sync is on, and a note on the listener.
func syncState(root string) (on bool, note string) {
	want := clistate.LoadPrefs(root).BackgroundSync
	rf := listener.ReadRunFile(root)
	switch {
	case rf != nil && rf.Daemon:
		return true, fmt.Sprintf("listener pid %d on 127.0.0.1:%d since %s", rf.PID, rf.Port, rf.StartedAt)
	case rf != nil:
		return want, fmt.Sprintf("a foreground listener runs (pid %d)", rf.PID)
	case want:
		return true, "not running now; the next agent hook starts it"
	}
	return false, "sessions sync as each hook fires"
}

func printSync(w io.Writer, root string) {
	on, note := syncState(root)
	state := "off"
	if on {
		state = "on"
	}
	fmt.Fprintf(w, "Listener:  background sync %s (%s)\n", state, note)
}

// listenerRetry spaces the hooks' attempts to restart a wanted listener
// (the desktop may hold the store, and then serve cannot start).
const listenerRetry = time.Minute

// ensureListener restarts the background listener from an agent hook when
// the user turned background sync on and none runs. It never waits.
func ensureListener(root string) {
	if !clistate.LoadPrefs(root).BackgroundSync || listener.ReadRunFile(root) != nil {
		return
	}
	mark := clistate.Path(root, "serve.spawn")
	if fi, err := os.Stat(mark); err == nil && time.Since(fi.ModTime()) < listenerRetry {
		return
	}
	_ = os.WriteFile(mark, nil, 0o600)
	now := time.Now()
	_ = os.Chtimes(mark, now, now)
	if c, err := spawnListener(root, 0); err == nil {
		_ = c.Process.Release()
	}
}
