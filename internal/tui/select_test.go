package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func press(m tea.Model, keys ...tea.KeyPressMsg) tea.Model {
	for _, k := range keys {
		m, _ = m.Update(k)
	}
	return m
}

var (
	down  = tea.KeyPressMsg{Code: tea.KeyDown}
	up    = tea.KeyPressMsg{Code: tea.KeyUp}
	enter = tea.KeyPressMsg{Code: tea.KeyEnter}
	esc   = tea.KeyPressMsg{Code: tea.KeyEscape}
)

func TestSelectMovesAndChooses(t *testing.T) {
	m := press(newSelect("Pick", []string{"a", "b", "c"}, 0), down, down, down, up, enter).(selectModel)
	if m.chosen != 1 || !m.done {
		t.Fatalf("%+v", m)
	}
}

func TestSelectCancel(t *testing.T) {
	m := press(newSelect("Pick", []string{"a"}, 0), esc).(selectModel)
	if m.chosen != -1 || !m.done {
		t.Fatalf("%+v", m)
	}
}

func TestSelectViewMarksTheCursor(t *testing.T) {
	v := newSelect("Pick an organisation", []string{"Atlas", "Delta"}, 1).View()
	s := v.Content
	if !strings.Contains(s, "Pick an organisation") || !strings.Contains(s, "› Delta") || strings.Contains(s, "› Atlas") {
		t.Fatalf("%q", s)
	}
}
