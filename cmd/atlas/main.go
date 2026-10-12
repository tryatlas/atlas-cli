// Command atlas is the headless Atlas CLI: shared memory and session capture
// for coding agents on a server, byte-compatible with the Atlas desktop app.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/tryatlas/atlas-cli/internal/tui"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

// stdin is replaced in tests.
var stdin io.Reader = os.Stdin

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
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
			fmt.Fprintln(stderr, "atlas:", err)
		}
		return 1
	}
	return 0
}
