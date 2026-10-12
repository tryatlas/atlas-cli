package tui

import (
	"io"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;:]*[A-Za-z]`)

func plain(v tea.View) string { return ansiRE.ReplaceAllString(v.Content, "") }

func hit(m tea.Model, code rune, text string) tea.Model {
	m, _ = m.Update(tea.KeyPressMsg{Code: code, Text: text})
	return m
}

func TestStepSelect(t *testing.T) {
	var m tea.Model = stepSelect{step: Step{N: 2, Total: 4, Title: "Project"}, question: "Which Project?",
		choices: []Choice{{Label: "backend", Detail: "matches"}, {Label: "+ Create"}}, chosen: -1}
	s := plain(m.View())
	for _, want := range []string{"Atlas  setup / project", "STEP 2 OF 4", "▮▮▮▮▮▮▮▮▯▯▯▯▯▯▯▯", "Which Project?", "▸  backend", "matches", "[ENTER] select"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
	m = hit(m, tea.KeyDown, "")
	m = hit(m, tea.KeyEnter, "")
	if fm := m.(stepSelect); fm.chosen != 1 || !fm.done {
		t.Fatalf("%+v", fm)
	}
	if s := plain(m.View()); !strings.Contains(s, "├ Project") || !strings.Contains(s, "+ Create") {
		t.Fatalf("summary: %s", s)
	}
}

func TestStepMultiAllRow(t *testing.T) {
	var m tea.Model = stepMulti{step: Step{N: 3, Total: 4, Title: "Agents"}, allLabel: "All agents",
		choices: []Choice{{Label: "Claude Code"}, {Label: "Codex"}, {Label: "Cursor"}}, checked: []bool{true, false, false}}
	if s := plain(m.View()); !strings.Contains(s, "1 of 3 selected") {
		t.Fatal(s)
	}
	m = hit(m, tea.KeySpace, " ") // on the "all" row: select every agent
	if got := m.(stepMulti).picked(); len(got) != 3 {
		t.Fatalf("all: %v", got)
	}
	m = hit(m, 'a', "a") // a again clears
	if got := m.(stepMulti).picked(); len(got) != 0 {
		t.Fatalf("none: %v", got)
	}
	m = hit(m, tea.KeyDown, "")
	m = hit(m, tea.KeyDown, "")
	m = hit(m, tea.KeySpace, " ") // Codex
	m = hit(m, tea.KeyEnter, "")
	fm := m.(stepMulti)
	if got := fm.picked(); !fm.ok || len(got) != 1 || got[0] != 1 {
		t.Fatalf("%v %v", fm.ok, got)
	}
	if s := plain(m.View()); !strings.Contains(s, "├ Agents") || !strings.Contains(s, "Codex") {
		t.Fatal(s)
	}
	m = hit(stepMulti{choices: []Choice{{Label: "x"}}, checked: []bool{true}}, tea.KeyEscape, "")
	if m.(stepMulti).ok {
		t.Fatal("esc must cancel")
	}
}

func TestStepSelectScrollsALongList(t *testing.T) {
	choices := make([]Choice, 30)
	for i := range choices {
		choices[i] = Choice{Label: string(rune('a' + i%26))}
	}
	var m tea.Model = stepSelect{step: Step{Title: "Project"}, choices: choices, chosen: -1}
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 20})
	s := plain(m.View())
	if !strings.Contains(s, "↓ 24 more") || strings.Count(s, " more") != 1 {
		t.Fatal(s)
	}
	for range 29 {
		m = hit(m, tea.KeyDown, "")
	}
	if s := plain(m.View()); !strings.Contains(s, "↑ 24 more") || strings.Count(s, " more") != 1 {
		t.Fatal(s)
	}
}

func TestIndentKeepsWrappedLinesOnTheStem(t *testing.T) {
	var b strings.Builder
	w := Indent(&b)
	_, _ = io.WriteString(w, "short\n"+strings.Repeat("word ", 30)+"\n")
	lines := strings.Split(strings.TrimRight(ansiRE.ReplaceAllString(b.String(), ""), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatalf("%q", lines)
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "│  ") || len([]rune(l)) > indentWidth+5 {
			t.Fatalf("%q", l)
		}
	}
}
