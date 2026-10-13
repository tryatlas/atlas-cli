package tui

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"
)

func TestLoginCopiesTheCode(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	var got []string
	writeClipboard = func(s string) error { got = append(got, s); return nil }
	t.Cleanup(func() { writeClipboard = clipboard.WriteAll })

	var m tea.Model = loginModel{}
	m, cmd := m.Update(loginPrompt{URL: "https://atlas.test/device", Code: "WDJB-MJHT"})
	m, _ = m.Update(cmd())
	view := ansiRE.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(view, "copied to your clipboard") {
		t.Errorf("no copied note:\n%s", view)
	}

	_, cmd = m.Update(tea.KeyPressMsg{Code: 'c', Text: "c"})
	cmd()
	if len(got) != 2 || got[0] != "WDJB-MJHT" || got[1] != "WDJB-MJHT" {
		t.Errorf("clipboard writes = %q, want the code on show and on c", got)
	}
}

func TestLoginFallsBackToTheTerminalClipboard(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	writeClipboard = func(string) error { return errors.New("no clipboard") }
	t.Cleanup(func() { writeClipboard = clipboard.WriteAll })

	var m tea.Model = loginModel{}
	m, cmd := m.Update(loginPrompt{Code: "WDJB-MJHT"})
	msg := cmd()
	if c, ok := msg.(codeCopied); !ok || !c.osc52 {
		t.Fatalf("msg = %#v, want an OSC 52 fallback", msg)
	}
	m, cmd = m.Update(msg)
	if cmd == nil {
		t.Error("no SetClipboard command for the fallback")
	}
	view := ansiRE.ReplaceAllString(m.View().Content, "")
	if strings.Contains(view, "copied to your clipboard") || !strings.Contains(view, "No clipboard here") {
		t.Errorf("fallback should hedge, not claim a copy:\n%s", view)
	}
}

func TestLoginOverSSHSaysSo(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "10.0.0.2 52000 10.0.0.9 22")
	writeClipboard = func(string) error { t.Error("native clipboard used over SSH"); return nil }
	t.Cleanup(func() { writeClipboard = clipboard.WriteAll })

	var m tea.Model = loginModel{}
	m, cmd := m.Update(loginPrompt{Code: "WDJB-MJHT"})
	m, _ = m.Update(cmd())
	view := ansiRE.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(view, "You're on SSH") || !strings.Contains(view, "select the code above") {
		t.Errorf("no SSH note:\n%s", view)
	}
}
