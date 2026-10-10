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
)

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run executes the command tree and returns the process exit code.
func run(args []string, stdout, stderr io.Writer) int {
	root := newRootCmd()
	root.SetArgs(args)
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
		fmt.Fprintln(stderr, "atlas:", err)
		return 1
	}
	return 0
}
