package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tryatlas/cli/internal/tui"
)

// styledHelp is --help on a terminal: the wordmark and path, what the
// command does, then usage, subcommands and flags under upper-case labels.
// Pipes keep cobra's plain help.
func styledHelp(plain func(*cobra.Command, []string)) func(*cobra.Command, []string) {
	return func(cmd *cobra.Command, args []string) {
		if !interactive() {
			plain(cmd, args)
			return
		}
		w := cmd.OutOrStdout()
		path := strings.Fields(cmd.CommandPath())
		tui.Header(w, path[1:]...)
		fmt.Fprintln(w)
		desc := cmd.Long
		if desc == "" {
			desc = cmd.Short
		}
		for _, l := range tui.Wrap(desc, min(tui.Width()-4, 96)) {
			fmt.Fprintln(w, "  "+l)
		}
		tui.Section(w, "Usage")
		fmt.Fprintln(w, "  "+tui.Default.Bold.Render(cmd.UseLine()))
		var rows [][]string
		for _, c := range cmd.Commands() {
			if c.IsAvailableCommand() {
				rows = append(rows, []string{c.Name(), tui.Default.Faint.Render(tui.Fit(c.Short, max(tui.Width()-30, 20)))})
			}
		}
		if len(rows) > 0 {
			tui.Section(w, "Commands")
			for _, r := range rows {
				fmt.Fprintln(w, "  "+tui.Default.Bold.Render(fmt.Sprintf("%-12s", r[0]))+"  "+r[1])
			}
		}
		if f := cmd.LocalFlags().FlagUsages(); f != "" {
			tui.Section(w, "Flags")
			for _, l := range strings.Split(strings.TrimRight(f, "\n"), "\n") {
				name, desc := splitFlag(l)
				fmt.Fprintln(w, "  "+tui.Default.Bold.Render(fmt.Sprintf("%-28s", name))+" "+tui.Default.Faint.Render(desc))
			}
		}
		fmt.Fprintln(w)
		if len(rows) > 0 {
			tui.Note(w, cmd.CommandPath()+" <command> --help for more")
		}
	}
}

// splitFlag splits a pflag usage line into the flag and its description.
func splitFlag(l string) (string, string) {
	l = strings.TrimSpace(l)
	if i := strings.Index(l, "   "); i > 0 {
		return strings.TrimSpace(l[:i]), strings.TrimSpace(l[i:])
	}
	return l, ""
}
