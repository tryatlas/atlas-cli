// Command atlas-cli is the headless Atlas CLI: shared memory and session capture
// for coding agents on a server, byte-compatible with the Atlas desktop app.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/tryatlas/atlas-cli/internal/tui"
	"github.com/tryatlas/atlas-cli/internal/update"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

// stdin is replaced in tests.
var stdin io.Reader = os.Stdin

func main() {
	if exe, err := update.Executable(); err == nil {
		renameLegacy(exe, os.Args[1:], os.Stderr)
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// renameLegacy moves a binary still called atlas (put there by the updater
// of v0.4.0 and earlier) to atlas-cli in the same folder. Agent hooks, the
// memory bridge and the listener run quietly under the old name; the first
// command a person types does the move and says so.
func renameLegacy(exe string, args []string, stderr io.Writer) {
	if filepath.Base(exe) != "atlas" {
		return
	}
	if len(args) > 0 && (args[0] == "hooks" || args[0] == "mcp-bridge" || args[0] == "serve" || args[0] == "uninstall") {
		return
	}
	target := filepath.Join(filepath.Dir(exe), "atlas-cli")
	if _, err := os.Lstat(target); err == nil {
		fmt.Fprintf(stderr, "atlas-cli: the command is now atlas-cli (%s); delete the old %s.\n", target, exe)
		return
	}
	if err := os.Rename(exe, target); err != nil {
		fmt.Fprintf(stderr, "atlas-cli: the command is now atlas-cli, but %s could not be renamed: %v\n", exe, err)
		return
	}
	fmt.Fprintf(stderr, "atlas-cli: the command is now atlas-cli; moved %s to %s.\n", exe, target)
	fmt.Fprintln(stderr, "atlas-cli: run atlas-cli agents add --all in each Project to point the agents' hooks at it.")
}

// run executes the command tree and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	if interactive() {
		tui.Detect() // light or dark palette, before anything draws
		stdout = tui.Out(stdout)
	}
	root := newRootCmd()
	root.SetArgs(args)
	root.SetIn(stdin)
	root.SetOut(stdout)
	root.SetErr(stderr)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root.ExecuteContext(ctx); err != nil {
		var ni notImplementedError
		if errors.As(err, &ni) {
			fmt.Fprintln(stderr, err)
			return 2
		}
		if !errors.Is(err, tui.ErrCancelled) && !tui.Shown(err) { // the screen already said so
			fmt.Fprintln(stderr, "atlas-cli:", err)
		}
		return 1
	}
	return 0
}
