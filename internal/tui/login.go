package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// DevicePrompt is what the user needs to approve a device sign-in.
type DevicePrompt struct {
	URL, Code string
	Expires   time.Time
	Opened    bool // the browser was opened
}

type (
	loginPrompt DevicePrompt
	loginDone   struct {
		who string
		err error
	}
)

type loginModel struct {
	prompt *DevicePrompt
	frame  int
	done   bool
	who    string
	err    error
}

func (m loginModel) Init() tea.Cmd { return workTicker() }

func (m loginModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case workTick:
		if m.done {
			return m, nil
		}
		m.frame++
		return m, workTicker()
	case loginPrompt:
		p := DevicePrompt(msg)
		m.prompt = &p
	case loginDone:
		m.done, m.who, m.err = true, msg.who, msg.err
		return m, tea.Quit
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			m.done, m.err = true, ErrCancelled
			return m, tea.Quit
		}
	}
	return m, nil
}

// spaced writes a code with room between its characters: "W D J B - M J H T".
func spaced(code string) string { return strings.Join(strings.Split(code, ""), " ") }

func (m loginModel) View() tea.View {
	var b strings.Builder
	if m.done {
		switch {
		case errors.Is(m.err, ErrCancelled):
			b.WriteString(Default.Error.Render("✗") + " " + Default.Bold.Render("Sign-in cancelled") + "\n")
		case m.err != nil:
			b.WriteString(Default.Error.Render("✗") + " " + Default.Bold.Render("Sign-in failed") + " " + Default.Error.Render(m.err.Error()) + "\n")
		default:
			b.WriteString(Default.Success.Render("✓") + " " + Default.Bold.Render("Signed in as "+m.who) + "\n")
		}
		return tea.NewView(b.String())
	}
	b.WriteString(Header2("sign in") + "\n\n")
	if m.prompt == nil {
		b.WriteString(pulse(m.frame) + " " + Default.Faint.Render("Asking Atlas for a sign-in code…") + "\n")
		return tea.NewView(b.String())
	}
	code := Default.Logo.Render(spaced(m.prompt.Code))
	box := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Default.Rule.GetForeground()).Padding(1, 4).Render(code)
	b.WriteString(Upper("your code") + "\n" + box + "\n\n")
	b.WriteString(Upper("open") + "\n" + Default.Info.Render(m.prompt.URL) + "\n")
	if m.prompt.Opened {
		b.WriteString(Default.Faint.Render("opened in your browser") + "\n")
	}
	left := time.Until(m.prompt.Expires).Round(time.Second)
	b.WriteString("\n" + pulse(m.frame) + " " + Default.Bold.Render("Waiting for approval") + "  " +
		Default.Faint.Render(fmt.Sprintf("code expires in %s", max(left, 0))) + "\n\n")
	b.WriteString(Keys("ESC", "cancel") + "\n")
	return tea.NewView(b.String())
}

// Header2 is a screen's first line as a string.
func Header2(crumb ...string) string { return Wordmark() + "  " + Crumb(crumb...) }

// DeviceLogin runs a device sign-in on screen: run calls prompt once it has
// the code, then blocks until the user approves; it returns who signed in.
func DeviceLogin(run func(prompt func(DevicePrompt)) (string, error)) error {
	p := tea.NewProgram(loginModel{}, tea.WithOutput(os.Stdout), tea.WithInput(os.Stdin))
	go func() {
		who, err := run(func(dp DevicePrompt) { p.Send(loginPrompt(dp)) })
		p.Send(loginDone{who, err})
	}()
	final, err := p.Run()
	if err != nil {
		return err
	}
	if m := final.(loginModel); m.err != nil {
		return ShownError{m.err}
	}
	return nil
}
