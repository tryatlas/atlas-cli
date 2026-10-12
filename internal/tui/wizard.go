package tui

import (
	"fmt"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Step places one question in a numbered flow (atlas init): the header
// shows where the user is, and the answered step collapses to one line.
type Step struct {
	N, Total int
	Title    string
}

// Choice is one option of a step: a label and a faint detail after it.
type Choice struct {
	Label, Detail string
}

// wizardWidth is the frame's width before the terminal reports its own.
const wizardWidth = 72

func frameWidth(term int) int {
	if term <= 0 {
		return wizardWidth
	}
	return max(min(term-2, 96), 40)
}

// header is the breadcrumb over a rule, then "STEP 2 OF 4  ▮▮▮▮▯▯▯▯".
func (s Step) header(w int) string {
	h := Header2("setup", strings.ToLower(s.Title)) + "\n" + Rule(w) + "\n"
	if s.Total > 0 {
		h += Upper(fmt.Sprintf("step %d of %d", s.N, s.Total)) + "  " + Bar(s.N, s.Total, 4*s.Total) + "\n"
	}
	return h
}

// footer is a rule over the key hints.
func footer(w int, keys ...string) string {
	return Rule(w) + "\n" + Keys(keys...) + "\n"
}

// summary is the tree line an answered step leaves behind; Ready closes
// the tree.
func (s Step) summary(answer string) string {
	return TreeLine(false, s.Title, Default.Answer.Render(answer))
}

// cancelled is the line a step left with esc leaves behind.
func (s Step) cancelled() string {
	return TreeLine(true, s.Title, Default.Error.Render("cancelled"))
}

// StepDone prints the line of a step answered without a question (one
// organisation, a flag on the command line).
func StepDone(w io.Writer, s Step, answer string) {
	fmt.Fprintln(w, s.summary(answer))
}

// Ready closes a flow's tree: a green hexagon and the message.
func Ready(w io.Writer, msg string, notes ...string) {
	fmt.Fprintln(w, Default.Tree.Render("└")+" "+Default.Hex.Render("⬢")+" "+Default.Bold.Render(msg))
	for _, n := range notes {
		fmt.Fprintln(w, "    "+Default.Faint.Render(n))
	}
}

// row renders one option as a tree line: ├ (└ for the last), the label,
// and its detail; the cursor's row is ▸ with the label on the selection
// background and the detail in green.
func row(cursor, last bool, mark, label, detail string) string {
	if mark != "" {
		mark += " "
	}
	if cursor {
		if detail != "" {
			detail = "  " + Default.Answer.Render(detail)
		}
		return Default.Tree.Render("▸") + " " + mark + Default.Highlight.Render(" "+label+" ") + detail + "\n"
	}
	if detail != "" {
		detail = "  " + Default.Faint.Render(detail)
	}
	glyph := "├"
	if last {
		glyph = "└"
	}
	return Default.Tree.Render(glyph) + " " + mark + " " + Default.Option.Render(label) + " " + detail + "\n"
}

func labelWidth(choices []Choice) int {
	w := 0
	for _, c := range choices {
		w = max(w, lipgloss.Width(c.Label))
	}
	return w
}

// ── Single choice ────────────────────────────────────────────────────────────

type stepSelect struct {
	step     Step
	question string
	choices  []Choice
	cursor   int
	chosen   int
	done     bool
	height   int // the terminal's, once known; long lists scroll
	width    int
}

// chrome is the lines a step draws around its list: breadcrumb, rule,
// step line, question, the "more" markers, the footer and the blank lines.
const chrome = 14

// window is the slice of rows [from, to) that fits with the cursor in it.
func window(n, cursor, height int) (from, to int) {
	rows := n
	if height > 0 {
		rows = max(height-chrome, 4)
	}
	if n <= rows {
		return 0, n
	}
	from = min(max(cursor-rows/2, 0), n-rows)
	return from, from + rows
}

func (m stepSelect) Init() tea.Cmd { return nil }

func (m stepSelect) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.height, m.width = ws.Height, ws.Width
		return m, nil
	}
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "up", "k", "shift+tab":
		m.cursor = (m.cursor - 1 + len(m.choices)) % len(m.choices)
	case "down", "j", "tab":
		m.cursor = (m.cursor + 1) % len(m.choices)
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(m.choices) - 1
	case "enter", "space":
		m.chosen, m.done = m.cursor, true
		return m, tea.Quit
	case "esc", "q", "ctrl+c":
		m.done = true
		return m, tea.Quit
	default:
		// A digit picks that option directly.
		if r := key.String(); len(r) == 1 && r[0] >= '1' && r[0] <= '9' && int(r[0]-'1') < len(m.choices) {
			m.cursor = int(r[0] - '1')
		}
	}
	return m, nil
}

func (m stepSelect) View() tea.View {
	if m.done {
		if m.chosen < 0 {
			return tea.NewView(m.step.cancelled() + "\n")
		}
		return tea.NewView(m.step.summary(m.choices[m.chosen].Label) + "\n")
	}
	w := frameWidth(m.width)
	var b strings.Builder
	b.WriteString(m.step.header(w) + "\n")
	b.WriteString(Default.Bold.Render(m.question) + "\n\n")
	lw := labelWidth(m.choices)
	from, to := window(len(m.choices), m.cursor, m.height)
	if from > 0 {
		b.WriteString(Default.Tree.Render("│") + " " + Default.Faint.Render(fmt.Sprintf("↑ %d more", from)) + "\n")
	}
	for i := from; i < to; i++ {
		c := m.choices[i]
		b.WriteString(row(i == m.cursor, i == len(m.choices)-1, "", fmt.Sprintf("%-*s", lw, c.Label), c.Detail))
	}
	if to < len(m.choices) {
		b.WriteString(Default.Tree.Render("│") + " " + Default.Faint.Render(fmt.Sprintf("↓ %d more", len(m.choices)-to)) + "\n")
	}
	b.WriteString("\n" + footer(w, "↑↓", "navigate", "ENTER", "select", "ESC", "cancel"))
	return tea.NewView(b.String())
}

// StepSelect asks one question of a flow and returns the chosen index, or
// ErrCancelled.
func StepSelect(s Step, question string, choices []Choice, initial int) (int, error) {
	return stepSelectWith(s, question, choices, initial, os.Stdin, os.Stdout)
}

func stepSelectWith(s Step, question string, choices []Choice, initial int, in io.Reader, out io.Writer) (int, error) {
	if len(choices) == 0 {
		return -1, fmt.Errorf("%s: nothing to choose from", s.Title)
	}
	if initial < 0 || initial >= len(choices) {
		initial = 0
	}
	m := stepSelect{step: s, question: question, choices: choices, cursor: initial, chosen: -1}
	final, err := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out)).Run()
	if err != nil {
		return -1, err
	}
	if fm := final.(stepSelect); fm.chosen >= 0 {
		return fm.chosen, nil
	}
	return -1, ErrCancelled
}

// ── Several choices ──────────────────────────────────────────────────────────

// stepMulti is a checklist whose first row selects or clears every option.
type stepMulti struct {
	step     Step
	question string
	allLabel string
	choices  []Choice
	checked  []bool
	cursor   int // 0 is the "all" row; i+1 is choices[i]
	done     bool
	ok       bool
	width    int
}

func (m stepMulti) allChecked() bool {
	for _, c := range m.checked {
		if !c {
			return false
		}
	}
	return len(m.checked) > 0
}

func (m stepMulti) setAll(on bool) stepMulti {
	for i := range m.checked {
		m.checked[i] = on
	}
	return m
}

func (m stepMulti) Init() tea.Cmd { return nil }

func (m stepMulti) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
		return m, nil
	}
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	rows := len(m.choices) + 1
	switch key.String() {
	case "up", "k", "shift+tab":
		m.cursor = (m.cursor - 1 + rows) % rows
	case "down", "j", "tab":
		m.cursor = (m.cursor + 1) % rows
	case "space", "x":
		if m.cursor == 0 {
			m = m.setAll(!m.allChecked())
		} else {
			m.checked[m.cursor-1] = !m.checked[m.cursor-1]
		}
	case "a":
		m = m.setAll(!m.allChecked())
	case "n":
		m = m.setAll(false)
	case "enter":
		m.done, m.ok = true, true
		return m, tea.Quit
	case "esc", "q", "ctrl+c":
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m stepMulti) picked() []int {
	var out []int
	for i, c := range m.checked {
		if c {
			out = append(out, i)
		}
	}
	return out
}

// box is a checkbox: ☒ chosen, ☐ not, ▣ for the "all" row when some are.
func box(on bool) string {
	if on {
		return Default.Check.Render("☒")
	}
	return Default.Off.Render("☐")
}

func (m stepMulti) View() tea.View {
	if m.done {
		if !m.ok {
			return tea.NewView(m.step.cancelled() + "\n")
		}
		var names []string
		for _, i := range m.picked() {
			names = append(names, m.choices[i].Label)
		}
		answer := strings.Join(names, ", ")
		if answer == "" {
			answer = "none"
		}
		return tea.NewView(m.step.summary(answer) + "\n")
	}
	w := frameWidth(m.width)
	var b strings.Builder
	b.WriteString(m.step.header(w) + "\n")
	b.WriteString(Default.Bold.Render(m.question) + "\n\n")
	lw := max(labelWidth(m.choices), lipgloss.Width(m.allLabel))
	n := len(m.picked())
	all := box(m.allChecked())
	if n > 0 && !m.allChecked() {
		all = Default.Accent.Render("▣")
	}
	b.WriteString(row(m.cursor == 0, false, all, fmt.Sprintf("%-*s", lw, m.allLabel), fmt.Sprintf("%d of %d selected", n, len(m.choices))))
	for i, c := range m.choices {
		b.WriteString(row(m.cursor == i+1, i == len(m.choices)-1, box(m.checked[i]), fmt.Sprintf("%-*s", lw, c.Label), c.Detail))
	}
	b.WriteString("\n" + footer(w, "↑↓", "navigate", "SPACE", "toggle", "A", "all", "N", "none", "ENTER", "confirm", "ESC", "cancel"))
	return tea.NewView(b.String())
}

// StepMulti asks for any number of options, with a first row that selects
// them all; it returns the chosen indexes (possibly none), or ErrCancelled.
func StepMulti(s Step, question, allLabel string, choices []Choice, checked []bool) ([]int, error) {
	return stepMultiWith(s, question, allLabel, choices, checked, os.Stdin, os.Stdout)
}

func stepMultiWith(s Step, question, allLabel string, choices []Choice, checked []bool, in io.Reader, out io.Writer) ([]int, error) {
	m := stepMulti{step: s, question: question, allLabel: allLabel, choices: choices, checked: make([]bool, len(choices))}
	copy(m.checked, checked)
	final, err := tea.NewProgram(m, tea.WithInput(in), tea.WithOutput(out)).Run()
	if err != nil {
		return nil, err
	}
	fm := final.(stepMulti)
	if !fm.ok {
		return nil, ErrCancelled
	}
	return fm.picked(), nil
}
