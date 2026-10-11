package tui

import "charm.land/lipgloss/v2"

// Theme is every colour and style the CLI draws with. No other file names a
// colour: screens take their styles from here.
type Theme struct {
	Title, Cursor, Hint, Faint, Bold lipgloss.Style

	// Chat transcript.
	User, Assistant, Thinking, Notice, Error lipgloss.Style
	ToolTitle, ToolMeta, Location            lipgloss.Style
	StatusPending, StatusRunning             lipgloss.Style
	StatusDone, StatusFailed                 lipgloss.Style
	DiffAdd, DiffDel, DiffHunk, Terminal     lipgloss.Style
	PlanDone, PlanActive, PlanPending        lipgloss.Style

	// Frame.
	Bar, BarKey, BarValue, Usage lipgloss.Style
	Overlay, OverlayTitle        lipgloss.Style
	Selected, Option, Input      lipgloss.Style
	Border                       lipgloss.Style
}

// ANSI palette indices, so the user's terminal theme decides the shades.
var (
	cBlue    = lipgloss.Color("12")
	cCyan    = lipgloss.Color("6")
	cGreen   = lipgloss.Color("2")
	cRed     = lipgloss.Color("1")
	cYellow  = lipgloss.Color("3")
	cMagenta = lipgloss.Color("5")
	cGrey    = lipgloss.Color("8")
)

// Default is the theme.
var Default = Theme{
	Title:  lipgloss.NewStyle().Bold(true),
	Cursor: lipgloss.NewStyle().Foreground(cBlue).Bold(true),
	Hint:   lipgloss.NewStyle().Faint(true),
	Faint:  lipgloss.NewStyle().Faint(true),
	Bold:   lipgloss.NewStyle().Bold(true),

	User:      lipgloss.NewStyle().Foreground(cBlue).Bold(true),
	Assistant: lipgloss.NewStyle(),
	Thinking:  lipgloss.NewStyle().Faint(true).Italic(true),
	Notice:    lipgloss.NewStyle().Foreground(cGrey),
	Error:     lipgloss.NewStyle().Foreground(cRed).Bold(true),

	ToolTitle: lipgloss.NewStyle().Bold(true),
	ToolMeta:  lipgloss.NewStyle().Foreground(cGrey),
	Location:  lipgloss.NewStyle().Foreground(cCyan),

	StatusPending: lipgloss.NewStyle().Foreground(cGrey),
	StatusRunning: lipgloss.NewStyle().Foreground(cYellow),
	StatusDone:    lipgloss.NewStyle().Foreground(cGreen),
	StatusFailed:  lipgloss.NewStyle().Foreground(cRed),

	DiffAdd:  lipgloss.NewStyle().Foreground(cGreen),
	DiffDel:  lipgloss.NewStyle().Foreground(cRed),
	DiffHunk: lipgloss.NewStyle().Foreground(cCyan),
	Terminal: lipgloss.NewStyle().Foreground(cGrey),

	PlanDone:    lipgloss.NewStyle().Foreground(cGreen),
	PlanActive:  lipgloss.NewStyle().Foreground(cYellow),
	PlanPending: lipgloss.NewStyle().Foreground(cGrey),

	Bar:      lipgloss.NewStyle().Foreground(cGrey),
	BarKey:   lipgloss.NewStyle().Foreground(cMagenta),
	BarValue: lipgloss.NewStyle().Bold(true),
	Usage:    lipgloss.NewStyle().Foreground(cGrey),

	Overlay:      lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(cBlue).Padding(0, 1),
	OverlayTitle: lipgloss.NewStyle().Bold(true).Foreground(cBlue),
	Selected:     lipgloss.NewStyle().Foreground(cBlue).Bold(true),
	Option:       lipgloss.NewStyle(),
	Input:        lipgloss.NewStyle().Border(lipgloss.NormalBorder(), true, false, false, false).BorderForeground(cGrey),
	Border:       lipgloss.NewStyle().Foreground(cGrey),
}
