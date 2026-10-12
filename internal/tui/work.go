package tui

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"
)

// ShownError is an error a screen already showed the user; main does not
// print it again.
type ShownError struct{ Err error }

func (e ShownError) Error() string { return e.Err.Error() }
func (e ShownError) Unwrap() error { return e.Err }

// Shown reports whether err was already shown.
func Shown(err error) bool {
	var s ShownError
	return errors.As(err, &s)
}

// StepFunc reports progress from inside a Work function: the line under the
// title changes to msg.
type StepFunc func(msg string)

// FracFunc sets a Work bar to a known fraction, 0..1 (a download).
type FracFunc func(f float64)

type (
	workTick struct{}
	workFrac float64
	workStep string
	workDone struct {
		msg string
		err error
	}
)

type workModel struct {
	title  string
	step   string
	frame  int
	start  time.Time
	done   bool
	msg    string
	err    error
	output []string // finished steps, kept on screen
	prog   float64  // the bar, 0..1
	full   bool     // finished: the bar shows full a moment before the result
	known  bool     // the bar follows reported fractions, not time
}

type workShown struct{}

// advance moves the bar: it eases toward 90% with time and jumps forward
// with each step, never back.
func (m *workModel) advance(step bool) {
	if m.known {
		return
	}
	el := time.Since(m.start).Seconds()
	m.prog = max(m.prog, 0.9*(1-math.Exp(-el/2.5)))
	if step {
		m.prog += (0.95 - m.prog) * 0.3
	}
}

func (m workModel) Init() tea.Cmd { return workTicker() }

func workTicker() tea.Cmd {
	return tea.Tick(110*time.Millisecond, func(time.Time) tea.Msg { return workTick{} })
}

func (m workModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case workTick:
		if m.done || m.full {
			return m, nil
		}
		m.frame++
		m.advance(false)
		return m, workTicker()
	case workFrac:
		m.known = true
		m.prog = max(m.prog, min(float64(msg), 1))
	case workStep:
		if m.step != "" {
			m.output = append(m.output, m.step)
		}
		m.step = string(msg)
		m.advance(true)
	case workDone:
		m.msg, m.err = msg.msg, msg.err
		if m.step != "" {
			m.output = append(m.output, m.step)
			m.step = ""
		}
		if msg.err != nil {
			m.done = true
			return m, tea.Quit
		}
		m.full, m.prog = true, 1
		return m, tea.Tick(220*time.Millisecond, func(time.Time) tea.Msg { return workShown{} })
	case workShown:
		m.done = true
		return m, tea.Quit
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			m.done, m.err = true, errors.New("interrupted")
			return m, tea.Quit
		}
	}
	return m, nil
}

// barWidth is the progress bar's width on this terminal.
func barWidth() int { return max(min(Width()-4, 56), 16) }

func (m workModel) View() tea.View {
	var b []byte
	if m.done {
		for _, o := range m.output {
			b = fmt.Appendf(b, "%s %s\n", Default.Tree.Render("├"), Default.Faint.Render(o))
		}
		switch {
		case m.err != nil:
			b = fmt.Appendf(b, "%s %s %s\n", Default.Error.Render("✗"), Default.Bold.Render(m.title), Default.Error.Render(m.err.Error()))
		case m.msg != "":
			b = fmt.Appendf(b, "%s %s\n", Default.Success.Render("✓"), Default.Bold.Render(m.msg))
		}
		return tea.NewView(string(b))
	}
	el := time.Since(m.start).Round(100 * time.Millisecond)
	b = fmt.Appendf(b, "%s  %s\n", Default.Bold.Render(m.title), Default.Faint.Render(el.String()))
	b = fmt.Appendf(b, "%s\n", Bar(int(m.prog*1000), 1000, barWidth()))
	for _, o := range m.output {
		b = fmt.Appendf(b, "%s %s\n", Default.Tree.Render("├"), Default.Faint.Render(o))
	}
	if m.step != "" {
		b = fmt.Appendf(b, "%s %s\n", Default.Tree.Render("└"), Default.Faint.Render(m.step))
	}
	return tea.NewView(string(b))
}

// Work runs fn while a pulsing mark and the title show that something is
// happening; steps fn reports appear under it. On a terminal it ends with
// "✓ <msg>" or "✗ <title> <error>" (the error comes back as a ShownError);
// elsewhere fn runs silently and msg is printed plain to w.
func Work(w io.Writer, title string, fn func(step StepFunc) (string, error)) error {
	return WorkProgress(w, title, func(step StepFunc, _ FracFunc) (string, error) { return fn(step) })
}

// WorkProgress is Work for a job that knows how far it is: frac drives the
// bar (a download), steps still show under it.
func WorkProgress(w io.Writer, title string, fn func(step StepFunc, frac FracFunc) (string, error)) error {
	if !Interactive() {
		msg, err := fn(func(string) {}, func(float64) {})
		if err == nil && msg != "" {
			fmt.Fprintln(w, msg)
		}
		return err
	}
	p := tea.NewProgram(workModel{title: title, start: time.Now()}, tea.WithOutput(os.Stdout), tea.WithInput(os.Stdin))
	go func() {
		msg, err := fn(func(s string) { p.Send(workStep(s)) }, func(f float64) { p.Send(workFrac(f)) })
		p.Send(workDone{msg, err})
	}()
	final, err := p.Run()
	if err != nil {
		return err
	}
	m := final.(workModel)
	if m.err != nil {
		return ShownError{m.err}
	}
	return nil
}
