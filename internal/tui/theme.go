package tui

import (
	"image/color"
	"os"
	"strings"

	"charm.land/lipgloss/v2"
)

// Theme is every colour and style the CLI draws with. No other file names a
// colour: screens take their styles from here.
//
// The palette is the Atlas desktop app's (crates/atlas-theme/themes/
// atlas.toml): monochrome first, black and white with greys between; the
// action colour is an inversion of the page, not a hue; colour is kept for
// status (success, warning, error, info). No gradients. The logo is cream.
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

	// Wizard and settings.
	Brand, Accent, Check, Off, Badge lipgloss.Style
	Pane, PaneFocused, Sidebar       lipgloss.Style

	// Frame: breadcrumb, rules, key hints, panels, tags (frame.go).
	Crumb, CrumbHere, Rule          lipgloss.Style
	Key, KeyLabel, KeyBadge         lipgloss.Style
	Tree, Answer, Label             lipgloss.Style
	Highlight, PanelEdge, Panel     lipgloss.Style
	PanelTitle, PanelTitleFocused   lipgloss.Style
	TagOn, TagOff, TagRun, TagWarn  lipgloss.Style
	BarFull, BarEmpty, Hex, HexIdle lipgloss.Style

	// Logo and status.
	Logo, LogoDim                     lipgloss.Style
	Success, Warning, Info, Secondary lipgloss.Style
}

// Palette is one theme's colours.
type Palette struct {
	Bg, Panel, Fill, Popover        color.Color
	Border, BorderStrong            color.Color
	Fg, Text, Secondary, Muted, Dim color.Color
	Logo                            color.Color
	Success, Warning, Error, Info   color.Color
	SuccessBg                       color.Color
	Gold, Selection                 color.Color
}

// Dark is atlas.toml [dark].
var Dark = Palette{
	Bg: lipgloss.Color("#000000"), Panel: lipgloss.Color("#0a0a0a"), Fill: lipgloss.Color("#171717"), Popover: lipgloss.Color("#1c1c1c"),
	Border: lipgloss.Color("#1e1e1e"), BorderStrong: lipgloss.Color("#3d3d3d"),
	Fg: lipgloss.Color("#ffffff"), Text: lipgloss.Color("#d4d4d4"), Secondary: lipgloss.Color("#aaaaaa"),
	Muted: lipgloss.Color("#8f8f8f"), Dim: lipgloss.Color("#666666"),
	Logo:    lipgloss.Color("#FFFFE2"),
	Success: lipgloss.Color("#9ecf8a"), Warning: lipgloss.Color("#ffff00"), Error: lipgloss.Color("#f44747"), Info: lipgloss.Color("#6796e6"),
	SuccessBg: lipgloss.Color("#0d2211"),
	Gold:      lipgloss.Color("#e5c07b"), // the app's terminal yellow (keys.toml)
	Selection: lipgloss.Color("#303030"), // editor.selection.background
}

// Light is atlas.toml [light].
var Light = Palette{
	Bg: lipgloss.Color("#ffffff"), Panel: lipgloss.Color("#fafafa"), Fill: lipgloss.Color("#f2f2f2"), Popover: lipgloss.Color("#f0f0f0"),
	Border: lipgloss.Color("#e0e0e0"), BorderStrong: lipgloss.Color("#bdbdbd"),
	Fg: lipgloss.Color("#000000"), Text: lipgloss.Color("#2b2b2b"), Secondary: lipgloss.Color("#4f4f4f"),
	Muted: lipgloss.Color("#737373"), Dim: lipgloss.Color("#8a8a8a"),
	Logo:    lipgloss.Color("#000000"),
	Success: lipgloss.Color("#3f7f2f"), Warning: lipgloss.Color("#8f7a00"), Error: lipgloss.Color("#c62828"), Info: lipgloss.Color("#1f5fb0"),
	SuccessBg: lipgloss.Color("#e3f1dc"),
	Gold:      lipgloss.Color("#8f7a00"),
	Selection: lipgloss.Color("#d0d0d0"),
}

// NewTheme builds the styles from a palette.
func NewTheme(p Palette) Theme {
	s := func() lipgloss.Style { return lipgloss.NewStyle() }
	fg := func(c color.Color) lipgloss.Style { return s().Foreground(c) }
	return Theme{
		Title:  fg(p.Fg).Bold(true),
		Cursor: fg(p.Fg).Bold(true),
		Hint:   fg(p.Dim),
		Faint:  fg(p.Muted),
		Bold:   fg(p.Fg).Bold(true),

		User:      fg(p.Fg).Bold(true),
		Assistant: fg(p.Text),
		Thinking:  fg(p.Muted).Italic(true),
		Notice:    fg(p.Muted),
		Error:     fg(p.Error).Bold(true),

		ToolTitle: fg(p.Fg).Bold(true),
		ToolMeta:  fg(p.Muted),
		Location:  fg(p.Info),

		StatusPending: fg(p.Dim),
		StatusRunning: fg(p.Warning),
		StatusDone:    fg(p.Success),
		StatusFailed:  fg(p.Error),

		DiffAdd:  fg(p.Success),
		DiffDel:  fg(p.Error),
		DiffHunk: fg(p.Info),
		Terminal: fg(p.Muted),

		PlanDone:    fg(p.Success),
		PlanActive:  fg(p.Fg).Bold(true),
		PlanPending: fg(p.Dim),

		Bar:      fg(p.Muted),
		BarKey:   fg(p.Secondary),
		BarValue: fg(p.Fg).Bold(true),
		Usage:    fg(p.Muted),

		Overlay:      s().Border(lipgloss.RoundedBorder()).BorderForeground(p.BorderStrong).Padding(0, 1),
		OverlayTitle: fg(p.Fg).Bold(true),
		Selected:     fg(p.Fg).Bold(true),
		Option:       fg(p.Text),
		Input:        s().Border(lipgloss.NormalBorder(), true, false, false, false).BorderForeground(p.BorderStrong),
		Border:       fg(p.BorderStrong),

		Brand:       fg(p.Logo).Bold(true),
		Accent:      fg(p.Secondary),
		Check:       fg(p.Success).Bold(true),
		Off:         fg(p.Dim),
		Badge:       fg(p.Secondary),
		Pane:        s().Border(lipgloss.RoundedBorder()).BorderForeground(p.BorderStrong).Padding(0, 1),
		PaneFocused: s().Border(lipgloss.RoundedBorder()).BorderForeground(p.Fg).Padding(0, 1),
		Sidebar:     s().Padding(0, 1),

		Crumb:     fg(p.Dim),
		CrumbHere: fg(p.Fg).Bold(true),
		Rule:      fg(p.BorderStrong),
		Key:       fg(p.Fg).Bold(true),
		KeyLabel:  fg(p.Dim),
		KeyBadge:  fg(p.Secondary).Background(p.Fill),
		Tree:      fg(p.Gold),
		Answer:    fg(p.Success),
		Label:     fg(p.Dim).Bold(true),
		Highlight: fg(p.Fg).Background(p.Selection).Bold(true),

		PanelEdge:         fg(p.BorderStrong),
		Panel:             fg(p.Fg),
		PanelTitle:        fg(p.Dim),
		PanelTitleFocused: fg(p.Fg).Bold(true),

		TagOn:   fg(p.Success).Background(p.SuccessBg).Bold(true),
		TagOff:  fg(p.Dim).Background(p.Fill),
		TagRun:  fg(p.Fg).Background(p.Popover).Bold(true),
		TagWarn: fg(p.Warning).Background(p.Fill).Bold(true),

		BarFull:  fg(p.Success),
		BarEmpty: fg(p.BorderStrong),
		Hex:      fg(p.Success),
		HexIdle:  fg(p.Dim),

		Logo:      fg(p.Logo).Bold(true),
		LogoDim:   fg(p.BorderStrong),
		Success:   fg(p.Success),
		Warning:   fg(p.Warning),
		Info:      fg(p.Info),
		Secondary: fg(p.Secondary),
	}
}

// Default is the theme in use; screens hold the pointer, so Detect can
// switch it to the light palette before anything draws.
var Default = func() *Theme { t := NewTheme(Dark); return &t }()

// Detect picks the light or dark palette: ATLAS_THEME=light|dark, else the
// terminal's COLORFGBG ("15;0": light text on black), else dark, the app's
// default. The terminal is not queried: one that does not answer would hang.
func Detect() {
	if light(os.Getenv("ATLAS_THEME"), os.Getenv("COLORFGBG")) {
		*Default = NewTheme(Light)
	}
}

func light(theme, fgbg string) bool {
	switch strings.ToLower(theme) {
	case "light":
		return true
	case "dark":
		return false
	}
	if i := strings.LastIndexByte(fgbg, ';'); i >= 0 {
		bg := fgbg[i+1:]
		return bg == "7" || bg == "15"
	}
	return false
}
