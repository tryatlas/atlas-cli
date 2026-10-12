// Package memview is the shared-memory browser (atlas memory browse): the
// record's entries by kind, search, and one entry with its evidence and
// revision history. It only reads.
package memview

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/tryatlas/atlas-cli/internal/memory/record"
	"github.com/tryatlas/atlas-cli/internal/tui"
)

var th = tui.Default

// Kinds are the tabs, in the record's order.
var Kinds = []string{"", "plan", "file_changed", "decision", "fact", "failure", "architecture", "preference"}

// Source is what the browser reads (a *record.Store).
type Source interface {
	Active(ctx context.Context, kinds ...string) ([]*record.Entry, error)
	Search(ctx context.Context, query string, kinds []string, limit int) ([]record.Hit, error)
	History(ctx context.Context, id int64) ([]record.Revision, error)
}

// Model is the browser.
type Model struct {
	ctx       context.Context
	src       Source
	title     string
	kind      int
	entries   []*record.Entry
	cursor    int
	top       int
	query     string
	searching bool
	history   []record.Revision
	showHist  bool
	err       error
	width     int
	height    int
}

// New opens the browser on a record.
func New(ctx context.Context, src Source, title string) *Model {
	m := &Model{ctx: ctx, src: src, title: title}
	m.load()
	return m
}

func (m *Model) load() {
	var kinds []string
	if k := Kinds[m.kind]; k != "" {
		kinds = []string{k}
	}
	m.err = nil
	if m.query != "" {
		hits, err := m.src.Search(m.ctx, m.query, kinds, 200)
		m.entries, m.err = nil, err
		for _, h := range hits {
			m.entries = append(m.entries, h.Entry)
		}
	} else {
		m.entries, m.err = m.src.Active(m.ctx, kinds...)
	}
	m.cursor, m.top, m.showHist, m.history = 0, 0, false, nil
}

// Init does nothing.
func (m *Model) Init() tea.Cmd { return nil }

// Update handles keys.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = max(msg.Width, 60), max(msg.Height, 12)
	case tea.KeyPressMsg:
		k := msg.String()
		if m.searching {
			switch k {
			case "enter":
				m.searching = false
				m.load()
			case "esc":
				m.searching, m.query = false, ""
				m.load()
			case "backspace":
				if r := []rune(m.query); len(r) > 0 {
					m.query = string(r[:len(r)-1])
				}
			default:
				m.query += msg.Text
			}
			return m, nil
		}
		switch k {
		case "q", "esc", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
				m.showHist = false
			}
		case "down", "j":
			if m.cursor < len(m.entries)-1 {
				m.cursor++
				m.showHist = false
			}
		case "tab", "right", "l":
			m.kind = (m.kind + 1) % len(Kinds)
			m.load()
		case "shift+tab", "left":
			m.kind = (m.kind - 1 + len(Kinds)) % len(Kinds)
			m.load()
		case "/":
			m.searching, m.query = true, ""
		case "h":
			if e := m.current(); e != nil {
				m.showHist = !m.showHist
				if m.showHist {
					m.history, m.err = m.src.History(m.ctx, e.ID)
				}
			}
		}
	}
	return m, nil
}

func (m *Model) current() *record.Entry {
	if m.cursor < len(m.entries) {
		return m.entries[m.cursor]
	}
	return nil
}

func when(ms int64) string {
	if ms == 0 {
		return "-"
	}
	return time.UnixMilli(ms).Local().Format("2006-01-02 15:04")
}

func label(k string) string {
	if k == "" {
		return "all"
	}
	return strings.ReplaceAll(k, "_", " ")
}

func clip(s string, w int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if w < 2 || len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

// tabs are the kinds, the current one on the highlight bar.
func (m *Model) tabs() string {
	var out []string
	for i, k := range Kinds {
		if i == m.kind {
			out = append(out, th.Highlight.Render(" "+label(k)+" "))
		} else {
			out = append(out, th.Faint.Render(" "+label(k)+" "))
		}
	}
	return strings.Join(out, " ")
}

// View draws a breadcrumb and the kind tabs, the entries and the selected
// entry in two panels, and the keys.
func (m *Model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("")
	}
	W := m.width
	left := tui.Header2("memory", label(Kinds[m.kind]))
	right := th.Faint.Render(clip(m.title, W/2))
	head := left + strings.Repeat(" ", max(W-lipgloss.Width(left)-lipgloss.Width(right), 1)) + right
	head += "\n" + tui.Rule(W) + "\n" + m.tabs()
	if m.searching || m.query != "" {
		q := m.query
		if m.searching {
			q += "▏"
		}
		head += "   " + tui.Upper("search") + " " + q
	}
	bodyH := max(m.height-lipgloss.Height(head)-2, 6)
	listW := W * 2 / 5
	rows := bodyH - 2
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+rows {
		m.top = m.cursor - rows + 1
	}
	var list []string
	for i := m.top; i < len(m.entries) && i < m.top+rows; i++ {
		e := m.entries[i]
		kind := th.Accent.Render(fmt.Sprintf("%-12s", label(e.Kind)))
		text := clip(e.Content, listW-20)
		if e.State == record.StateCandidate {
			text = th.Faint.Render(text)
		}
		if i == m.cursor {
			list = append(list, th.Tree.Render("▸")+kind+th.Highlight.Render(" "+text+" "))
		} else {
			list = append(list, " "+kind+" "+text)
		}
	}
	if len(m.entries) == 0 {
		list = append(list, th.Faint.Render("nothing here"))
	}
	count := fmt.Sprintf("Entries · %d", len(m.entries))
	panels := lipgloss.JoinHorizontal(lipgloss.Top,
		tui.Panel(count, strings.Join(list, "\n"), listW, bodyH, true), " ",
		tui.Panel("Entry", m.detail(W-listW-5), W-listW-1, bodyH, false))
	foot := tui.Keys("↑↓", "navigate", "TAB", "kind", "/", "search", "H", "history", "Q", "quit")
	if m.err != nil {
		foot = th.Error.Render("✗ " + m.err.Error())
	}
	v := tea.NewView(head + "\n" + panels + "\n" + tui.Rule(W) + "\n" + foot)
	v.AltScreen = true
	return v
}

func (m *Model) detail(w int) string {
	e := m.current()
	if e == nil {
		return ""
	}
	wrap := lipgloss.NewStyle().Width(w)
	var b strings.Builder
	b.WriteString(tui.Tag(strings.ToUpper(label(e.Kind)), th.TagRun) + "  " + th.Faint.Render(fmt.Sprintf("#%d · rev %d · %s", e.ID, e.Rev, strings.TrimSpace(e.State+" "+e.Status))) + "\n\n")
	b.WriteString(wrap.Render(e.Content) + "\n\n")
	meta := [][2]string{
		{"by", e.By()}, {"session", e.Session}, {"key", e.Key}, {"confidence", fmt.Sprintf("%.2f", e.Confidence)},
		{"uses", fmt.Sprint(e.Uses)}, {"created", when(e.CreatedAt)}, {"updated", when(e.UpdatedAt)},
	}
	for _, kv := range meta {
		if kv[1] != "" {
			b.WriteString(tui.Upper(fmt.Sprintf("%-11s", kv[0])) + kv[1] + "\n")
		}
	}
	for _, c := range e.Evidence {
		ev := c.Path + ":" + c.Lines()
		if c.Symbol != nil {
			ev += " " + *c.Symbol
		}
		b.WriteString(tui.Upper("evidence   ") + th.Location.Render(ev) + "\n")
	}
	if m.showHist {
		b.WriteString("\n" + tui.Upper("history") + "\n")
		for _, r := range m.history {
			b.WriteString(th.Faint.Render(fmt.Sprintf("rev %d · %s · %s · %s", r.Rev, r.Op, r.By(), when(r.At))) + "\n")
			b.WriteString(wrap.Render("  "+clip(r.Content, w*3)) + "\n")
		}
	}
	return b.String()
}
