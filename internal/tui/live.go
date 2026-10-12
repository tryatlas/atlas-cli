package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

// LiveEvent is one line of a live screen's feed.
type LiveEvent struct {
	At        time.Time
	Who, What string
	Bad       bool
}

type liveStop struct{}

type liveModel struct {
	title  []string
	fields [][2]string
	events []LiveEvent
	count  int
	frame  int
	start  time.Time
	height int
}

func (m liveModel) Init() tea.Cmd { return workTicker() }

func (m liveModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
	case workTick:
		m.frame++
		return m, workTicker()
	case LiveEvent:
		m.count++
		m.events = append(m.events, msg)
		if len(m.events) > 200 {
			m.events = m.events[len(m.events)-200:]
		}
	case liveStop:
		return m, tea.Quit
	case tea.KeyPressMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m liveModel) View() tea.View {
	var b strings.Builder
	b.WriteString(Header2(m.title...) + "\n")
	for _, f := range m.fields {
		b.WriteString("  " + Default.Faint.Render(fmt.Sprintf("%-*s", fieldWidth, f[0])) + " " + f[1] + "\n")
	}
	up := time.Since(m.start).Round(time.Second)
	b.WriteString("\n" + pulse(m.frame) + " " + Default.Bold.Render("Listening") + "  " +
		Default.Faint.Render(fmt.Sprintf("%d event(s) · up %s", m.count, up)) + "\n\n")
	b.WriteString(Upper("activity") + "\n")
	room := 8
	if m.height > 0 {
		room = max(m.height-len(m.fields)-10, 3)
	}
	ev := m.events
	if len(ev) > room {
		ev = ev[len(ev)-room:]
	}
	if len(ev) == 0 {
		b.WriteString("  " + Default.Faint.Render("waiting for your agents' hooks…") + "\n")
	}
	w := Width()
	for _, e := range ev {
		dot := Default.Success.Render("●")
		if e.Bad {
			dot = Default.Error.Render("●")
		}
		line := fmt.Sprintf("  %s %s %s  %s", Default.Faint.Render(e.At.Format("15:04:05")), dot,
			Default.Bold.Render(fmt.Sprintf("%-12s", e.Who)), e.What)
		b.WriteString(Fit(line, w-1) + "\n")
	}
	b.WriteString("\n" + Keys("Q", "stop") + "\n")
	return tea.NewView(b.String())
}

// Live is a running live screen.
type Live struct{ p *tea.Program }

// StartLive shows a live screen: a header, fields, a pulsing "Listening"
// line and a feed of events. Wait returns when the user quits (q, esc,
// ctrl+c) or Stop is called.
func StartLive(title []string, fields [][2]string) *Live {
	p := tea.NewProgram(liveModel{title: title, fields: fields, start: time.Now()}, tea.WithOutput(os.Stdout), tea.WithInput(os.Stdin))
	return &Live{p: p}
}

// Run blocks until the screen ends.
func (l *Live) Run() error { _, err := l.p.Run(); return err }

// Event adds a line to the feed.
func (l *Live) Event(e LiveEvent) { l.p.Send(e) }

// Stop ends the screen.
func (l *Live) Stop() { l.p.Send(liveStop{}) }
