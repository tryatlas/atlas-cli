package tui

import (
	"fmt"
	"io"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/term"
)

// Out wraps a terminal writer so colours are cut down to what the terminal
// shows (256 or 16 colours, or none).
func Out(w io.Writer) io.Writer { return colorprofile.NewWriter(w, os.Environ()) }

// Header is a command's first line: the mark, the name, and where we are.
func Header(w io.Writer, crumb ...string) {
	line := Wordmark()
	if len(crumb) > 0 {
		line += "  " + Crumb(crumb...)
	}
	fmt.Fprintln(w, line)
}

// Section starts a group of fields: an upper-case label, as the app's
// eyebrows.
func Section(w io.Writer, title string) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, Upper(title))
}

// fieldWidth is the label column.
const fieldWidth = 14

// Field is one "label   value" line of a section.
func Field(w io.Writer, label, value string) {
	fmt.Fprintln(w, "  "+Default.Faint.Render(fmt.Sprintf("%-*s", fieldWidth, label))+" "+value)
}

// State is how a value reads at a glance.
type State int

// States.
const (
	Neutral State = iota
	Good
	Warn
	Bad
	Idle
)

// Dot is a status dot and the value: ● in the status colour.
func Dot(s State, value string) string {
	dot := map[State]lipgloss.Style{Good: Default.Success, Warn: Default.Warning, Bad: Default.Error, Idle: Default.Off}[s]
	if s == Neutral {
		return "  " + Default.Option.Render(value)
	}
	return dot.Render("●") + " " + value
}

// Done prints "✓ msg" on a terminal, msg elsewhere.
func Done(w io.Writer, msg string) {
	if Interactive() {
		fmt.Fprintln(w, Default.Success.Render("✓")+" "+Default.Bold.Render(msg))
		return
	}
	fmt.Fprintln(w, msg)
}

// Note prints a secondary line: indented and faint on a terminal.
func Note(w io.Writer, msg string) {
	if Interactive() {
		fmt.Fprintln(w, "  "+Default.Faint.Render(msg))
		return
	}
	fmt.Fprintln(w, msg)
}

// Table renders rows under upper-case headers with a rule between; the
// first column is bold.
func Table(headers []string, rows [][]string) string {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = lipgloss.Width(h)
	}
	for _, r := range rows {
		for i := range min(len(r), len(widths)) {
			widths[i] = max(widths[i], lipgloss.Width(r[i]))
		}
	}
	var b strings.Builder
	cells := make([]string, len(headers))
	total := 0
	for i, h := range headers {
		cells[i] = Upper(Fit(h, widths[i]))
		total += widths[i] + 3
	}
	b.WriteString("  " + strings.Join(cells, "   ") + "\n")
	b.WriteString("  " + Rule(total-3) + "\n")
	for _, r := range rows {
		for i := range cells {
			v := ""
			if i < len(r) {
				v = r[i]
			}
			if i == 0 {
				v = Default.Bold.Render(v)
			}
			cells[i] = Fit(v, widths[i])
		}
		b.WriteString("  " + strings.TrimRight(strings.Join(cells, "   "), " ") + "\n")
	}
	return b.String()
}

// Width is the terminal's width, 100 when it cannot be read.
func Width() int {
	if w, _, err := term.GetSize(os.Stdout.Fd()); err == nil && w > 0 {
		return w
	}
	return 100
}

// lineWriter restyles each complete line written through it.
type lineWriter struct {
	w    io.Writer
	fn   func(string) string
	part string
}

// Lines returns a writer that passes each line through fn before writing
// it to w; plain output from shared code gets a screen's look this way.
func Lines(w io.Writer, fn func(line string) string) io.Writer { return &lineWriter{w: w, fn: fn} }

func (lw *lineWriter) Write(p []byte) (int, error) {
	lw.part += string(p)
	for {
		i := strings.IndexByte(lw.part, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := lw.part[:i]
		lw.part = lw.part[i+1:]
		if _, err := io.WriteString(lw.w, lw.fn(line)+"\n"); err != nil {
			return 0, err
		}
	}
}
