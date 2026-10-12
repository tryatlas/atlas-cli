package settings

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type fake struct {
	on      map[string]bool
	applied []string
}

func (f *fake) Load(context.Context) ([]Section, error) {
	return []Section{
		{Title: "Account", Items: []Item{{Label: "Signed in as", Value: "Ada <ada@example.com>"}}},
		{Title: "Agents", Items: []Item{
			{Key: "agent:codex", Label: "Codex", Kind: Toggle, On: f.on["agent:codex"], Detail: "Capture Codex sessions"},
			{Key: "agent:pi", Label: "Pi", Kind: Toggle, Disabled: "Run atlas init in this folder first."},
			{Key: "agents:all", Label: "Add all detected agents", Kind: Action},
		}},
	}, nil
}

func (f *fake) Apply(_ context.Context, key string, on bool) (string, error) {
	f.applied = append(f.applied, key)
	f.on[key] = on
	return "done " + key, nil
}

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;:]*[A-Za-z]`)

// run feeds a command's message back, as the program would.
func run(m *Model, cmd tea.Cmd) {
	for cmd != nil {
		msg := cmd()
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				if r := c(); r != nil {
					if _, ok := r.(tickMsg); !ok {
						_, cmd = m.Update(r)
						run(m, cmd)
					}
				}
			}
			return
		}
		_, cmd = m.Update(msg)
	}
}

func key(m *Model, code rune, text string) {
	_, cmd := m.Update(tea.KeyPressMsg{Code: code, Text: text})
	run(m, cmd)
}

func TestSettingsScreen(t *testing.T) {
	f := &fake{on: map[string]bool{}}
	m := New(context.Background(), f, "/repo")
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	s := ansiRE.ReplaceAllString(m.View().Content, "")
	for _, want := range []string{"Atlas  settings / account", "/repo", "├  Account", "└  Agents", "╭─ Sections", "╭─ Details", "Ada <ada@example.com>"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q\n%s", want, s)
		}
	}
	key(m, '2', "2")
	s = ansiRE.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(s, "▸  Codex ") || !strings.Contains(s, " off") || !strings.Contains(s, "Capture Codex sessions") || !strings.Contains(s, "Atlas  settings / agents") {
		t.Fatal(s)
	}
	key(m, tea.KeySpace, " ") // toggle Codex on
	if len(f.applied) != 1 || f.applied[0] != "agent:codex" || !f.on["agent:codex"] {
		t.Fatalf("%v", f.applied)
	}
	s = ansiRE.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(s, "  on") || !strings.Contains(s, "⬢ done agent:codex") {
		t.Fatal(s)
	}
	key(m, tea.KeyDown, "")
	key(m, tea.KeySpace, " ") // disabled: explains, applies nothing
	if len(f.applied) != 1 || !strings.Contains(ansiRE.ReplaceAllString(m.View().Content, ""), "atlas init") {
		t.Fatalf("%v", f.applied)
	}
	key(m, tea.KeyDown, "")
	key(m, tea.KeySpace, " ") // space does not run an action
	key(m, tea.KeyEnter, "")
	if len(f.applied) != 2 || f.applied[1] != "agents:all" {
		t.Fatalf("%v", f.applied)
	}
	key(m, tea.KeyLeft, "")
	key(m, tea.KeyUp, "")
	if m.sec != 0 || m.focus != 0 {
		t.Fatalf("sec %d focus %d", m.sec, m.focus)
	}
}

type progressFake struct{ fake }

func (p *progressFake) ApplyProgress(_ context.Context, key string, on bool, report func(float64, string)) (string, error) {
	report(0.5, "40 of 87 MB")
	return p.Apply(context.Background(), key, on)
}

func TestProgressAndConfirm(t *testing.T) {
	f := &progressFake{fake{on: map[string]bool{}}}
	m := New(context.Background(), f, "/repo")
	_, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	m.sections = []Section{{Title: "Model", Items: []Item{
		{Key: "model:download", Label: "Download model", Kind: Action},
		{Key: "model:remove", Label: "Remove model", Kind: Action, Confirm: "This deletes the files."},
	}}}
	m.sec, m.row, m.focus = 0, 0, 1
	// The running change shows its progress in the row.
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.busy != "Download model" {
		t.Fatal("download did not start")
	}
	_, _ = m.Update(progressMsg{frac: 0.5, note: "40 of 87 MB", ch: make(chan tea.Msg, 1)})
	if s := ansiRE.ReplaceAllString(m.View().Content, ""); !strings.Contains(s, "50%") || !strings.Contains(s, "40 of 87 MB") {
		t.Fatal(s)
	}
	_, _ = m.Update(appliedMsg{msg: "Model downloaded."})
	m.sections = []Section{{Title: "Model", Items: []Item{
		{Key: "model:download", Label: "Download model", Kind: Action},
		{Key: "model:remove", Label: "Remove model", Kind: Action, Confirm: "This deletes the files."},
	}}}
	m.row = 1
	// A confirmed action asks first.
	key(m, tea.KeyEnter, "")
	if slices.Contains(f.applied, "model:remove") || !strings.Contains(m.status, "Press enter again") {
		t.Fatalf("%v %q", f.applied, m.status)
	}
	_, cmd = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.busy != "Remove model" {
		t.Fatal("second enter must run it")
	}
}
