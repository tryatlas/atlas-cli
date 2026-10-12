package main

import (
	"testing"

	"github.com/tryatlas/cli/internal/tui"
)

func TestStatusDots(t *testing.T) {
	for _, c := range []struct {
		label, value string
		want         tui.State
	}{
		{"Sync", "ok · 1183 sent, 0 pending, 0 failed", tui.Good},
		{"Sync", "ok · 10 sent, 3 pending, 0 failed", tui.Warn},
		{"Sync", "offline · 2 failed", tui.Bad},
		{"Listener", "background sync off (sessions sync as each hook fires)", tui.Idle},
		{"Listener", "background sync on (listener pid 1)", tui.Good},
		{"Capture", "paused", tui.Idle},
		{"Sessions", "3 session(s), 1 turn(s) left open", tui.Warn},
		{"Commits", "linked up to eaa9ab0", tui.Good},
		{"Project", "/Users/x/repo", tui.Neutral},
	} {
		if got := statusOf(c.label, c.value); got != c.want {
			t.Errorf("%s %q = %v, want %v", c.label, c.value, got, c.want)
		}
	}
}
