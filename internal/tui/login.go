package tui

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
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
	// codeCopied reports a copy of the code; osc52 means the native
	// clipboard was out of reach, so the terminal is asked to copy instead.
	codeCopied struct{ osc52 bool }
)

type loginModel struct {
	prompt *DevicePrompt
	frame  int
	copied bool
	done   bool
	who    string
	err    error
}

// writeClipboard is replaced in tests.
var writeClipboard = clipboard.WriteAll

// copyCode puts code on the clipboard: the native one on this machine, or
// over SSH (where that clipboard is the wrong machine's) the terminal's.
func copyCode(code string) tea.Cmd {
	return func() tea.Msg {
		remote := os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != ""
		if !remote && writeClipboard(code) == nil {
			return codeCopied{}
		}
		return codeCopied{osc52: true}
	}
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
		return m, copyCode(p.Code)
	case codeCopied:
		m.copied = true
		if msg.osc52 {
			return m, tea.SetClipboard(m.prompt.Code)
		}
	case loginDone:
		m.done, m.who, m.err = true, msg.who, msg.err
		return m, tea.Quit
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" || msg.String() == "esc" {
			m.done, m.err = true, ErrCancelled
			return m, tea.Quit
		}
		if msg.String() == "c" && m.prompt != nil {
			return m, copyCode(m.prompt.Code)
		}
	}
	return m, nil
}

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
	// The code sits alone at the line start, unboxed, so a triple-click copies it clean.
	b.WriteString(Upper("your code") + "\n" + Default.Logo.Bold(true).Render(m.prompt.Code) + "\n")
	if m.copied {
		b.WriteString(Default.Success.Render("✓") + " " + Default.Faint.Render("copied to your clipboard") + "\n")
	}
	b.WriteString("\n")
	b.WriteString(Upper("open") + "\n" + Default.Info.Render(m.prompt.URL) + "\n")
	if m.prompt.Opened {
		b.WriteString(Default.Faint.Render("opened in your browser") + "\n")
	}
	left := time.Until(m.prompt.Expires).Round(time.Second)
	b.WriteString("\n" + pulse(m.frame) + " " + Default.Bold.Render("Waiting for approval") + "  " +
		Default.Faint.Render(fmt.Sprintf("code expires in %s", max(left, 0))) + "\n\n")
	b.WriteString(Keys("C", "copy code", "ESC", "cancel") + "\n")
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
