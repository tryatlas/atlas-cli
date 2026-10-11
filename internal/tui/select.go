package tui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// ErrCancelled is returned when the user leaves a prompt without choosing.
var ErrCancelled = errors.New("cancelled")

// Interactive reports whether stdin and stdout are terminals, so a prompt can
// be shown. On a pipe or in CI, commands take flags instead.
func Interactive() bool {
	return isTerminal(os.Stdin) && isTerminal(os.Stdout)
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

var (
	titleStyle  = Default.Title
	cursorStyle = Default.Cursor
	hintStyle   = Default.Hint
)

// selectModel is a single-choice list.
type selectModel struct {
	title   string
	options []string
	cursor  int
	chosen  int // -1 until enter
	done    bool
}

func newSelect(title string, options []string, initial int) selectModel {
	if initial < 0 || initial >= len(options) {
		initial = 0
	}
	return selectModel{title: title, options: options, cursor: initial, chosen: -1}
}

func (m selectModel) Init() tea.Cmd { return nil }

func (m selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	key, ok := msg.(tea.KeyPressMsg)
	if !ok {
		return m, nil
	}
	switch key.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.options)-1 {
			m.cursor++
		}
	case "enter":
		m.chosen, m.done = m.cursor, true
		return m, tea.Quit
	case "esc", "q", "ctrl+c":
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m selectModel) View() tea.View {
	var b strings.Builder
	b.WriteString(titleStyle.Render(m.title) + "\n")
	for i, o := range m.options {
		if i == m.cursor {
			b.WriteString(cursorStyle.Render("› "+o) + "\n")
		} else {
			b.WriteString("  " + o + "\n")
		}
	}
	if !m.done {
		b.WriteString(hintStyle.Render("↑/↓ to move · enter to choose · esc to cancel") + "\n")
	}
	return tea.NewView(b.String())
}

// Select shows a list and returns the chosen index, or ErrCancelled.
func Select(title string, options []string, initial int) (int, error) {
	return selectWith(title, options, initial, os.Stdin, os.Stdout)
}

func selectWith(title string, options []string, initial int, in io.Reader, out io.Writer) (int, error) {
	if len(options) == 0 {
		return -1, fmt.Errorf("%s: nothing to choose from", title)
	}
	final, err := tea.NewProgram(newSelect(title, options, initial), tea.WithInput(in), tea.WithOutput(out)).Run()
	if err != nil {
		return -1, err
	}
	m := final.(selectModel)
	if m.chosen < 0 {
		return -1, ErrCancelled
	}
	return m.chosen, nil
}
