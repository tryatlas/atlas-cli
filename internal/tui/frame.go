package tui

import (
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The frame every screen shares: a breadcrumb over a rule, a key line under
// a rule, titled panels, tags, tree lines and a block progress bar.

// Crumb is "atlas / settings / agents": the path faint, the last part bright.
func Crumb(parts ...string) string {
	if len(parts) == 0 {
		return ""
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		if i == len(parts)-1 {
			out[i] = Default.CrumbHere.Render(p)
		} else {
			out[i] = Default.Crumb.Render(p)
		}
	}
	return strings.Join(out, Default.Crumb.Render(" / "))
}

// Rule is a horizontal line w cells wide.
func Rule(w int) string { return Default.Rule.Render(strings.Repeat("─", max(w, 1))) }

// Keys renders key hints, "[ENTER] select · [ESC] back", from key, label pairs.
func Keys(pairs ...string) string {
	var out []string
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Default.Key.Render("["+pairs[i]+"]")+" "+Default.KeyLabel.Render(pairs[i+1]))
	}
	return strings.Join(out, Default.KeyLabel.Render(" · "))
}

// Badge is a key shown inside a list, "<1>".
func Badge(key string) string { return Default.KeyBadge.Render("<" + key + ">") }

// Tag is a short label on a coloured ground, " ON ".
func Tag(text string, style lipgloss.Style) string { return style.Render(" " + text + " ") }

// Bar is a block progress bar of width cells, done of total full.
func Bar(done, total, width int) string {
	if total <= 0 || width <= 0 {
		return ""
	}
	full := min(width*done/total, width)
	return Default.BarFull.Render(strings.Repeat("▮", full)) + Default.BarEmpty.Render(strings.Repeat("▯", width-full))
}

// TreeLine is one answered step: "├ Project        atlas-cli", or └ for
// the last one.
func TreeLine(last bool, label, answer string) string {
	glyph := "├"
	if last {
		glyph = "└"
	}
	return Default.Tree.Render(glyph) + " " + lipgloss.NewStyle().Width(16).Render(label) + " " + answer
}

// Upper is a small heading: "STEP 2 OF 4".
func Upper(s string) string { return Default.Label.Render(strings.ToUpper(s)) }

// Fit cuts a styled line to w cells and pads it to exactly w.
func Fit(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if lipgloss.Width(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", max(w-lipgloss.Width(s), 0))
}

// Panel draws body in a rounded box w×h cells (borders included) with the
// title set into the top edge, "╭─ Agents ──╮". A focused panel is
// coloured.
func Panel(title, body string, w, h int, focused bool) string {
	if w < 4 || h < 2 {
		return ""
	}
	edge, ts := Default.PanelEdge, Default.PanelTitle
	if focused {
		edge, ts = Default.Panel, Default.PanelTitleFocused
	}
	inner := w - 2
	top := edge.Render("╭" + strings.Repeat("─", inner) + "╮")
	if title != "" {
		t := ansi.Truncate(title, max(inner-4, 1), "…")
		top = edge.Render("╭─ ") + ts.Render(t) + edge.Render(" "+strings.Repeat("─", max(inner-3-lipgloss.Width(t), 0))+"╮")
	}
	var b strings.Builder
	b.WriteString(top + "\n")
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	for i := range h - 2 {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		b.WriteString(edge.Render("│") + " " + Fit(line, inner-2) + " " + edge.Render("│") + "\n")
	}
	b.WriteString(edge.Render("╰" + strings.Repeat("─", inner) + "╯"))
	return b.String()
}

// Wrap breaks plain text into lines of at most w cells.
func Wrap(s string, w int) []string {
	if w < 8 {
		return []string{s}
	}
	var out []string
	for _, para := range strings.Split(s, "\n") {
		out = append(out, strings.Split(ansi.Wordwrap(para, w, ""), "\n")...)
	}
	return out
}

// indentWidth is where Indent wraps a line.
const indentWidth = 76

// indentWriter prefixes each line it writes.
type indentWriter struct {
	w      io.Writer
	prefix string
	mid    bool
}

// Indent returns a writer that prefixes every line with the tree's stem,
// "│  ", so output printed between steps stays inside the tree.
func Indent(w io.Writer) io.Writer {
	return &indentWriter{w: w, prefix: Default.Tree.Render("│") + "  "}
}

func (iw *indentWriter) Write(p []byte) (int, error) {
	var b strings.Builder
	parts := strings.SplitAfter(string(p), "\n")
	for _, part := range parts {
		if part == "" {
			continue
		}
		if !iw.mid {
			b.WriteString(iw.prefix)
		}
		text, nl := strings.CutSuffix(part, "\n")
		for i, l := range Wrap(text, indentWidth) {
			if i > 0 {
				b.WriteString("\n" + iw.prefix + "  ") // a wrapped line stays on the stem
			}
			b.WriteString(Default.Faint.Render(l))
		}
		if nl {
			b.WriteString("\n")
		}
		iw.mid = !nl
	}
	if _, err := io.WriteString(iw.w, b.String()); err != nil {
		return 0, err
	}
	return len(p), nil
}
