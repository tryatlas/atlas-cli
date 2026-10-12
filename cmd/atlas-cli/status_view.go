package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/tryatlas/atlas-cli/internal/tui"
)

// statusGroups puts atlas-cli status's lines under the card's sections.
var statusGroups = []struct {
	title  string
	labels []string
}{
	{"Account", []string{"Account", "Org"}},
	{"Project", []string{"Project", "Binding", "Memory at", "Memory", "Capture", "Recorder"}},
	{"Capture and sync", []string{"Sessions", "Commits", "Sync", "Record"}},
	{"Agents", []string{"Listener", "Agents", "Git hook", "Hooks", "Hook err"}},
}

// statusOf reads a value's first words for its dot.
func statusOf(label, value string) tui.State {
	v := strings.ToLower(value)
	for _, zero := range []string{"0 failed", "0 pending", "0 need attention"} {
		v = strings.ReplaceAll(v, " "+zero, "")
	}
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(v, w) {
				return true
			}
		}
		return false
	}
	switch {
	case label == "Hook err" || has("failed", "broken", "refused", "unreadable", "error"):
		return tui.Bad
	case has("need attention", "outdated", "spooled", "not installed", "left open", "pending"):
		return tui.Warn
	case strings.HasPrefix(v, "off") || strings.HasPrefix(v, "paused") || strings.HasPrefix(v, "none") || strings.HasPrefix(v, "not ") || has("not signed in", "not initialised", "sync off"):
		return tui.Idle
	case strings.HasPrefix(v, "on") || strings.HasPrefix(v, "ok") || strings.HasPrefix(v, "idle") || has("intact", "linked up to", "sync on"):
		return tui.Good
	}
	return tui.Neutral
}

// renderStatus draws status lines as a card: the header, then a section per
// group, each value with its dot.
func renderStatus(w io.Writer, lines string) {
	values := map[string]string{}
	var order []string
	for _, l := range strings.Split(strings.TrimRight(lines, "\n"), "\n") {
		label, value, ok := strings.Cut(l, ":")
		if !ok || strings.HasPrefix(l, " ") {
			if len(order) > 0 { // a continuation of the previous value
				values[order[len(order)-1]] += " " + strings.TrimSpace(l)
			}
			continue
		}
		label = strings.TrimSpace(label)
		values[label] = strings.TrimSpace(value)
		order = append(order, label)
	}
	tui.Header(w, "status")
	seen := map[string]bool{}
	for _, g := range statusGroups {
		var has bool
		for _, l := range g.labels {
			if _, ok := values[l]; ok {
				has = true
			}
		}
		if !has {
			continue
		}
		tui.Section(w, g.title)
		for _, l := range g.labels {
			v, ok := values[l]
			if !ok {
				continue
			}
			seen[l] = true
			tui.Field(w, statusLabel(l), tui.Dot(statusOf(l, v), v))
		}
	}
	for _, l := range order {
		if !seen[l] {
			tui.Field(w, statusLabel(l), values[l])
		}
	}
	fmt.Fprintln(w)
}

func statusLabel(l string) string {
	switch l {
	case "Org":
		return "Organisation"
	case "Hook err":
		return "Last hook error"
	}
	return l
}
