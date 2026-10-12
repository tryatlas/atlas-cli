package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type introTick struct{}

// intro plays a render function from t=0 to t=1, then leaves the last frame.
type intro struct {
	render func(t float64) string
	frame  int
	frames int
}

func (m intro) Init() tea.Cmd { return introNext() }

func introNext() tea.Cmd {
	return tea.Tick(28*time.Millisecond, func(time.Time) tea.Msg { return introTick{} })
}

func (m intro) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case introTick:
		m.frame++
		if m.frame >= m.frames {
			return m, tea.Quit
		}
		return m, introNext()
	case tea.KeyPressMsg:
		m.frame = m.frames // any key skips
		return m, tea.Quit
	}
	return m, nil
}

func (m intro) View() tea.View {
	t := min(float64(m.frame)/float64(max(m.frames-1, 1)), 1)
	return tea.NewView(m.render(t) + "\n")
}

// Animate plays render from 0 to 1 (about half a second) on a terminal and
// leaves the final frame; elsewhere it prints only the final frame.
func Animate(w io.Writer, render func(t float64) string) {
	if !Interactive() || os.Getenv("ATLAS_NO_ANIMATION") != "" {
		fmt.Fprintln(w, render(1))
		return
	}
	_, _ = tea.NewProgram(intro{render: render, frames: 20}, tea.WithOutput(os.Stdout), tea.WithInput(os.Stdin)).Run()
}

// Command is one line of the splash's command list.
type Command struct{ Name, Short string }

// Splash is `atlas-cli` with no command: the mark lighting up stripe by stripe,
// the name and version beside it, then the commands by group.
func Splash(w io.Writer, version string, groups map[string][]Command, order []string) {
	Animate(w, func(t float64) string {
		logo := Logo(t)
		side := Default.Bold.Render("Atlas") + " " + Default.Faint.Render(version) + "\n" +
			Default.Faint.Render("Shared memory and session capture") + "\n" + Default.Faint.Render("for coding agents.")
		return "\n" + lipgloss.JoinHorizontal(lipgloss.Center, "  "+strings.ReplaceAll(logo, "\n", "\n  "), "    ", side) + "\n"
	})
	nameW := 0
	for _, g := range order {
		for _, c := range groups[g] {
			nameW = max(nameW, len(c.Name))
		}
	}
	for _, g := range order {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "  "+Upper(g))
		for _, c := range groups[g] {
			fmt.Fprintln(w, "  "+Default.Bold.Render(fmt.Sprintf("atlas-cli %-*s", nameW, c.Name))+"  "+Default.Faint.Render(Fit(c.Short, Width()-nameW-16)))
		}
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "  "+Keys("atlas <command> --help", "details", "atlas-cli init", "start here"))
	fmt.Fprintln(w)
}
