// Package settings is the settings screen (atlas settings): account,
// Project gates, agent integrations and background sync, in one place. The
// screen only draws and routes keys; a Backend reads and changes the state.
package settings

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/tryatlas/atlas-cli/internal/tui"
)

var th = tui.Default

// Kind is how a row behaves.
type Kind int

// Row kinds.
const (
	Info   Kind = iota // a value to read
	Toggle             // on or off; space flips it
	Action             // enter runs it
)

// Item is one row of a section.
type Item struct {
	Key    string // what Apply receives
	Label  string
	Kind   Kind
	On     bool   // Toggle: the current state
	Value  string // Info: the value; Toggle and Action: a short note after the state
	Detail string // shown under the list when the row is selected
	// Disabled says why the row cannot change now; empty when it can.
	Disabled string
	// Confirm, on an action, is asked first: the action runs on a second
	// enter.
	Confirm string
}

// Section is one entry of the sidebar.
type Section struct {
	Title string
	Icon  string // one cell, before the title
	Items []Item
}

// Backend reads and changes the settings.
type Backend interface {
	Load(ctx context.Context) ([]Section, error)
	// Apply flips a toggle to on, or runs an action (on is then true). The
	// message is shown in the status line.
	Apply(ctx context.Context, key string, on bool) (string, error)
}

type loadedMsg struct {
	sections []Section
	err      error
}

type appliedMsg struct {
	msg string
	err error
}

// Progresser is a Backend whose long changes report how far they are (a
// model download); the row shows a bar while one runs.
type Progresser interface {
	ApplyProgress(ctx context.Context, key string, on bool, report func(frac float64, note string)) (string, error)
}

type progressMsg struct {
	frac float64
	note string
	ch   chan tea.Msg
}

// listen waits for the next message of a running change.
func listen(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

type tickMsg struct{}

var spinner = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Model is the screen.
type Model struct {
	ctx      context.Context
	be       Backend
	title    string
	sections []Section
	sec      int
	row      int
	focus    int // 0: sidebar, 1: rows
	busy     string
	confirm  string // the action key waiting for its second enter
	frame    int
	prog     float64 // the running change's progress, -1 when unknown
	progNote string
	status   string
	err      error
	width    int
	height   int
}

// New opens the screen; title names the Project (or the folder).
func New(ctx context.Context, be Backend, title string) *Model {
	m := &Model{ctx: ctx, be: be, title: title, focus: 1}
	m.sections, m.err = be.Load(ctx)
	return m
}

// Init does nothing.
func (m *Model) Init() tea.Cmd { return nil }

func (m *Model) items() []Item {
	if m.sec < len(m.sections) {
		return m.sections[m.sec].Items
	}
	return nil
}

func (m *Model) current() *Item {
	if it := m.items(); m.row < len(it) {
		return &it[m.row]
	}
	return nil
}

func (m *Model) reload() tea.Cmd {
	return func() tea.Msg {
		s, err := m.be.Load(m.ctx)
		return loadedMsg{s, err}
	}
}

func tick() tea.Cmd {
	return tea.Tick(90*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}

// apply runs the change off the UI loop: starting a listener takes seconds.
func (m *Model) apply(it *Item, on bool) tea.Cmd {
	if it.Disabled != "" {
		m.status, m.err = it.Disabled, nil
		return nil
	}
	key := it.Key
	m.busy, m.status, m.err = it.Label, "", nil
	m.prog, m.progNote = -1, ""
	if pb, ok := m.be.(Progresser); ok {
		ch := make(chan tea.Msg, 64)
		go func() {
			msg, err := pb.ApplyProgress(m.ctx, key, on, func(f float64, note string) {
				select {
				case ch <- progressMsg{frac: f, note: note, ch: ch}:
				default: // the screen is behind: drop a frame, not the download
				}
			})
			ch <- appliedMsg{msg, err}
		}()
		return tea.Batch(tick(), listen(ch))
	}
	return tea.Batch(tick(), func() tea.Msg {
		msg, err := m.be.Apply(m.ctx, key, on)
		return appliedMsg{msg, err}
	})
}

// Update handles keys and results.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(msg.Width, 64), max(msg.Height, 16)
	case tickMsg:
		if m.busy != "" {
			m.frame++
			return m, tick()
		}
	case progressMsg:
		if msg.frac >= 0 {
			m.prog = msg.frac
		}
		if msg.note != "" || msg.frac >= 0 {
			m.progNote = msg.note
		}
		return m, listen(msg.ch)
	case appliedMsg:
		m.busy, m.status, m.err = "", msg.msg, msg.err
		return m, m.reload()
	case loadedMsg:
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.sections = msg.sections
		}
		m.sec = min(m.sec, max(len(m.sections)-1, 0))
		m.row = min(m.row, max(len(m.items())-1, 0))
	case tea.KeyPressMsg:
		return m.key(msg.String())
	}
	return m, nil
}

func (m *Model) key(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q", "esc", "ctrl+c":
		return m, tea.Quit
	}
	if m.busy != "" {
		return m, nil // one change at a time
	}
	switch k {
	case "r":
		m.status, m.err = "Refreshed.", nil
		return m, m.reload()
	case "tab":
		m.focus = 1 - m.focus
	case "left", "h":
		m.focus = 0
	case "right", "l":
		if len(m.items()) > 0 {
			m.focus = 1
		}
	case "up", "k":
		m.move(-1)
		m.confirm = ""
	case "down", "j":
		m.move(1)
		m.confirm = ""
	case "space", "enter", "x":
		if m.focus == 0 {
			m.focus = 1
			return m, nil
		}
		if it := m.current(); it != nil {
			switch {
			case it.Kind == Toggle:
				return m, m.apply(it, !it.On)
			case it.Kind == Action && k != "space":
				if it.Confirm != "" && m.confirm != it.Key && it.Disabled == "" {
					m.confirm, m.status, m.err = it.Key, it.Confirm+" Press enter again to confirm.", nil
					return m, nil
				}
				m.confirm = ""
				return m, m.apply(it, true)
			}
		}
	default:
		// A digit jumps to that section.
		if len(k) == 1 && k[0] >= '1' && k[0] <= '9' && int(k[0]-'1') < len(m.sections) {
			m.sec, m.row, m.focus = int(k[0]-'1'), 0, 1
		}
	}
	return m, nil
}

func (m *Model) move(d int) {
	if m.focus == 0 {
		if n := len(m.sections); n > 0 {
			m.sec = (m.sec + d + n) % n
			m.row = 0
		}
		return
	}
	if n := len(m.items()); n > 0 {
		m.row = (m.row + d + n) % n
	}
}

// ── View ─────────────────────────────────────────────────────────────────────

// state is a row's value: on in green, off dim, run in gold, information
// in green, as the tree's answers.
func (m *Model) state(it Item) string {
	if m.busy != "" && m.busy == it.Label && it.Kind != Info {
		if m.prog >= 0 {
			s := tui.Bar(int(m.prog*1000), 1000, 16) + " " + th.Bold.Render(fmt.Sprintf("%3.0f%%", m.prog*100))
			if m.progNote != "" {
				s += "  " + th.Faint.Render(m.progNote)
			}
			return s
		}
		return th.Badge.Render(spinner[m.frame%len(spinner)] + " applying…")
	}
	note := ""
	if it.Value != "" && it.Kind != Info {
		note = "  " + th.Faint.Render(it.Value)
	}
	switch it.Kind {
	case Toggle:
		if it.On {
			return th.Answer.Render("on") + note
		}
		return th.Off.Render("off") + note
	case Action:
		return th.Tree.Render("run ↵") + note
	}
	return th.Answer.Render(it.Value)
}

// glyph is a tree line's stem: ├, or └ for the last line.
func glyph(last bool) string {
	if last {
		return th.Tree.Render("└")
	}
	return th.Tree.Render("├")
}

func (m *Model) navBody(w int) string {
	var b strings.Builder
	b.WriteString("\n")
	for i, s := range m.sections {
		title := fmt.Sprintf("%-*s", max(w-4, 1), s.Title)
		last := i == len(m.sections)-1
		switch {
		case i == m.sec && m.focus == 0:
			b.WriteString(th.Tree.Render("▸") + " " + th.Highlight.Render(" "+title))
		case i == m.sec:
			b.WriteString(glyph(last) + "  " + th.Bold.Render(title))
		default:
			b.WriteString(glyph(last) + "  " + th.Faint.Render(title))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func (m *Model) rowsBody(w int) string {
	items := m.items()
	lw := 0
	for _, it := range items {
		lw = max(lw, lipgloss.Width(it.Label))
	}
	lw = min(lw, w/2)
	var b strings.Builder
	b.WriteString("\n")
	for i, it := range items {
		label := tui.Fit(it.Label, lw)
		st := m.state(it)
		if it.Disabled != "" && it.Kind != Info {
			st = th.Faint.Render(ansi.Strip(st))
		}
		if i == m.row && m.focus == 1 {
			b.WriteString(th.Tree.Render("▸") + " " + th.Highlight.Render(" "+label+" ") + " " + st + "\n")
		} else {
			b.WriteString(glyph(i == len(items)-1) + "  " + th.Option.Render(label) + "  " + st + "\n")
		}
	}
	if len(items) == 0 {
		b.WriteString(th.Faint.Render("nothing here"))
	}
	return b.String()
}

func (m *Model) detailBody(w int) string {
	it := m.current()
	if it == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n" + th.Bold.Render(it.Label) + "\n\n")
	switch it.Kind {
	case Info:
		b.WriteString(tui.Upper("value") + "\n")
		for _, l := range tui.Wrap(it.Value, w) {
			b.WriteString(th.Answer.Render(l) + "\n")
		}
	case Toggle:
		b.WriteString(tui.Upper("state") + "\n" + m.state(Item{Kind: Toggle, On: it.On}) + "\n")
		if it.Value != "" {
			b.WriteString("\n" + tui.Upper("note") + "\n")
			for _, l := range tui.Wrap(it.Value, w) {
				b.WriteString(l + "\n")
			}
		}
	case Action:
		b.WriteString(tui.Keys("ENTER", "run this") + "\n")
	}
	if it.Detail != "" {
		b.WriteString("\n")
		for _, l := range tui.Wrap(it.Detail, w) {
			b.WriteString(th.Faint.Render(l) + "\n")
		}
	}
	if it.Disabled != "" {
		b.WriteString("\n" + tui.Tag("!", th.TagWarn) + " ")
		for i, l := range tui.Wrap(it.Disabled, w-4) {
			if i > 0 {
				b.WriteString("    ")
			}
			b.WriteString(th.Badge.Render(l) + "\n")
		}
	}
	return b.String()
}

// View draws a breadcrumb over a rule, the sections, rows and details
// panels, and the keys under a rule with the last result.
func (m *Model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("")
	}
	W, H := m.width, m.height
	here := "settings"
	if m.sec < len(m.sections) {
		here = strings.ToLower(m.sections[m.sec].Title)
	}
	left := tui.Header2("settings", here)
	right := th.Faint.Render(ansi.Truncate(m.title, W/2, "…"))
	head := left + strings.Repeat(" ", max(W-lipgloss.Width(left)-lipgloss.Width(right), 1)) + right

	bodyH := max(H-4, 6)
	navW := 12
	for _, s := range m.sections {
		navW = max(navW, lipgloss.Width(s.Title)+10)
	}
	rest := W - navW - 1
	var panels string
	nav := tui.Panel("Sections", m.navBody(navW-4), navW, bodyH, m.focus == 0)
	title := ""
	if m.sec < len(m.sections) {
		title = m.sections[m.sec].Title
	}
	if W >= 110 {
		rowsW := rest * 11 / 20
		detW := rest - rowsW - 1
		rows := tui.Panel(title, m.rowsBody(rowsW-4), rowsW, bodyH, m.focus == 1)
		det := tui.Panel("Details", m.detailBody(detW-4), detW, bodyH, false)
		panels = lipgloss.JoinHorizontal(lipgloss.Top, nav, " ", rows, " ", det)
	} else {
		body := m.rowsBody(rest - 4)
		if m.focus == 1 {
			body += "\n" + th.Rule.Render(strings.Repeat("─", rest-4)) + "\n" + m.detailBody(rest-4)
		}
		panels = lipgloss.JoinHorizontal(lipgloss.Top, nav, " ", tui.Panel(title, body, rest, bodyH, m.focus == 1))
	}

	keys := tui.Keys("↑↓", "navigate", "→", "open", "1-9", "section", "Q", "quit")
	if m.focus == 1 {
		keys = tui.Keys("↑↓", "navigate", "SPACE", "toggle", "ENTER", "run", "←", "sections", "R", "refresh", "Q", "quit")
	}
	status := ""
	switch {
	case m.err != nil:
		status = th.Error.Render("✗ " + m.err.Error())
	case m.status != "":
		status = th.Hex.Render("⬢ ") + m.status
	}
	if status != "" {
		room := W - lipgloss.Width(keys) - 3
		if room < 12 {
			keys, room = "", W
		}
		status = tui.Fit(status, room)
		keys += "   " + status
	}
	v := tea.NewView(head + "\n" + tui.Rule(W) + "\n" + panels + "\n" + tui.Rule(W) + "\n" + keys)
	v.AltScreen = true
	return v
}
