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

// View draws the list and the selected entry side by side.
func (m *Model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("")
	}
	var tabs []string
	for i, k := range Kinds {
		if i == m.kind {
			tabs = append(tabs, th.Selected.Render("["+label(k)+"]"))
		} else {
			tabs = append(tabs, th.Faint.Render(label(k)))
		}
	}
	head := th.Title.Render("Shared memory · "+m.title) + "\n" + strings.Join(tabs, " ")
	if m.searching || m.query != "" {
		head += "\n" + th.BarKey.Render("search ") + m.query
		if m.searching {
			head += "▏"
		}
	}
	listW := m.width * 2 / 5
	bodyH := m.height - lipgloss.Height(head) - 2
	if m.cursor < m.top {
		m.top = m.cursor
	}
	if m.cursor >= m.top+bodyH {
		m.top = m.cursor - bodyH + 1
	}
	var list []string
	for i := m.top; i < len(m.entries) && i < m.top+bodyH; i++ {
		e := m.entries[i]
		line := fmt.Sprintf("%-12s %s", label(e.Kind), clip(e.Content, listW-16))
		if e.State == record.StateCandidate {
			line = th.Faint.Render(line)
		}
		if i == m.cursor {
			list = append(list, th.Selected.Render("› "+line))
		} else {
			list = append(list, "  "+line)
		}
	}
	if len(m.entries) == 0 {
		list = append(list, th.Faint.Render("  nothing here"))
	}
	left := lipgloss.NewStyle().Width(listW).Height(bodyH).Render(strings.Join(list, "\n"))
	right := lipgloss.NewStyle().Width(m.width - listW - 2).Height(bodyH).PaddingLeft(2).Render(m.detail(m.width - listW - 4))
	foot := th.Hint.Render("↑/↓ move · tab kind · / search · h history · q quit")
	if m.err != nil {
		foot = th.Error.Render(m.err.Error())
	}
	v := tea.NewView(lipgloss.JoinVertical(lipgloss.Left, head, lipgloss.JoinHorizontal(lipgloss.Top, left, right), foot))
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
	b.WriteString(th.Bold.Render(label(e.Kind)) + "  " + th.Faint.Render(fmt.Sprintf("#%d · rev %d · %s", e.ID, e.Rev, strings.TrimSpace(e.State+" "+e.Status))) + "\n\n")
	b.WriteString(wrap.Render(e.Content) + "\n\n")
	meta := [][2]string{
		{"by", e.By()}, {"session", e.Session}, {"key", e.Key}, {"confidence", fmt.Sprintf("%.2f", e.Confidence)},
		{"uses", fmt.Sprint(e.Uses)}, {"created", when(e.CreatedAt)}, {"updated", when(e.UpdatedAt)},
	}
	for _, kv := range meta {
		if kv[1] != "" {
			b.WriteString(th.BarKey.Render(fmt.Sprintf("%-11s", kv[0])) + kv[1] + "\n")
		}
	}
	for _, c := range e.Evidence {
		ev := c.Path + ":" + c.Lines()
		if c.Symbol != nil {
			ev += " " + *c.Symbol
		}
		b.WriteString(th.Location.Render("evidence   "+ev) + "\n")
	}
	if m.showHist {
		b.WriteString("\n" + th.Bold.Render("History") + "\n")
		for _, r := range m.history {
			b.WriteString(th.Faint.Render(fmt.Sprintf("rev %d · %s · %s · %s", r.Rev, r.Op, r.By(), when(r.At))) + "\n")
			b.WriteString(wrap.Render("  "+clip(r.Content, w*3)) + "\n")
		}
	}
	return b.String()
}
