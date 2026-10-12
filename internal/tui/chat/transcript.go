package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"

	"github.com/tryatlas/cli/internal/agents/acp"
	"github.com/tryatlas/cli/internal/tui"
)

var th = tui.Default

type itemKind int

const (
	kindUser itemKind = iota
	kindText
	kindThinking
	kindTool
	kindPlan
	kindNotice
	kindError
)

// item is one block of the transcript.
type item struct {
	kind  itemKind
	msgID string
	text  strings.Builder
	tool  *toolCard
	plan  []acp.PlanEntry
}

// toolCard is everything the sightings of one tool call said.
type toolCard struct {
	id, title, kind, status string
	locations               []acp.Location
	diffs                   []acp.ToolContent
	texts                   []string
	terminals               []string // client-side terminal ids
	output                  strings.Builder
	exit                    string
}

// transcript is the ordered conversation.
type transcript struct {
	items    []*item
	tools    map[string]*toolCard
	expanded bool // show thinking in full
}

func newTranscript() *transcript { return &transcript{tools: map[string]*toolCard{}} }

func (t *transcript) add(k itemKind, text string) *item {
	it := &item{kind: k}
	it.text.WriteString(text)
	t.items = append(t.items, it)
	return it
}

// chunk appends to the last item of the same kind and message, else starts
// one.
func (t *transcript) chunk(k itemKind, msgID, text string) {
	if text == "" {
		return
	}
	if n := len(t.items); n > 0 {
		last := t.items[n-1]
		if last.kind == k && (msgID == "" || last.msgID == "" || last.msgID == msgID) {
			last.text.WriteString(text)
			if last.msgID == "" {
				last.msgID = msgID
			}
			return
		}
	}
	it := t.add(k, text)
	it.msgID = msgID
}

// apply folds one session/update into the transcript.
func (t *transcript) apply(u acp.Update) {
	switch u.Kind {
	case "user_message_chunk":
		t.chunk(kindUser, u.MessageID, u.Block().TextOf())
	case "agent_message_chunk":
		t.chunk(kindText, u.MessageID, u.Block().TextOf())
	case "agent_thought_chunk":
		t.chunk(kindThinking, u.MessageID, u.Block().TextOf())
	case "tool_call", "tool_call_update":
		c := t.tools[u.ToolCallID]
		if c == nil {
			c = &toolCard{id: u.ToolCallID, status: "pending"}
			t.tools[u.ToolCallID] = c
			it := &item{kind: kindTool, tool: c}
			t.items = append(t.items, it)
		}
		if u.Title != nil && *u.Title != "" {
			c.title = *u.Title
		}
		if u.ToolKind != "" {
			c.kind = u.ToolKind
		}
		if u.Status != "" {
			c.status = u.Status
		}
		if len(u.Locations) > 0 {
			c.locations = u.Locations
		}
		if contents := u.ToolContents(); len(contents) > 0 {
			c.diffs, c.texts, c.terminals = nil, nil, nil
			for _, x := range contents {
				switch x.Type {
				case "diff":
					c.diffs = append(c.diffs, x)
				case "content":
					if x.Content != nil && x.Content.TextOf() != "" {
						c.texts = append(c.texts, x.Content.TextOf())
					}
				case "terminal":
					c.terminals = append(c.terminals, x.TerminalID)
				}
			}
		}
		t.meta(c, u.Meta)
	case "plan":
		var entries []acp.PlanEntry
		if json.Unmarshal(u.Entries, &entries) == nil {
			// One plan block, updated in place.
			for _, it := range t.items {
				if it.kind == kindPlan {
					it.plan = entries
					return
				}
			}
			it := &item{kind: kindPlan, plan: entries}
			t.items = append(t.items, it)
		}
	}
}

// meta reads the agent-owned terminal stream (_meta.terminal_output and
// terminal_exit on tool call updates).
func (t *transcript) meta(c *toolCard, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var m struct {
		Output *struct {
			Data string `json:"data"`
		} `json:"terminal_output"`
		Exit *struct {
			ExitCode *int    `json:"exit_code"`
			Signal   *string `json:"signal"`
		} `json:"terminal_exit"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return
	}
	if m.Output != nil {
		c.output.WriteString(m.Output.Data)
	}
	if m.Exit != nil {
		switch {
		case m.Exit.ExitCode != nil:
			c.exit = fmt.Sprintf("exit %d", *m.Exit.ExitCode)
		case m.Exit.Signal != nil:
			c.exit = "signal " + *m.Exit.Signal
		}
	}
}

// render draws the transcript at a width.
func (t *transcript) render(width int, terminal func(string) string) string {
	if width < 20 {
		width = 20
	}
	wrap := lipgloss.NewStyle().Width(width)
	var parts []string
	for _, it := range t.items {
		switch it.kind {
		case kindUser:
			parts = append(parts, th.User.Render("› ")+wrap.Width(width-2).Render(it.text.String()))
		case kindText:
			parts = append(parts, th.Assistant.Render(wrap.Render(strings.TrimRight(it.text.String(), "\n"))))
		case kindThinking:
			s := strings.TrimSpace(it.text.String())
			if !t.expanded {
				lines := strings.Split(s, "\n")
				if len(lines) > 3 {
					s = strings.Join(lines[:3], "\n") + "\n… (ctrl+e to expand)"
				}
			}
			parts = append(parts, th.Thinking.Render(wrap.Render(s)))
		case kindTool:
			parts = append(parts, renderTool(it.tool, width, terminal))
		case kindPlan:
			parts = append(parts, renderPlan(it.plan))
		case kindNotice:
			parts = append(parts, th.Notice.Render(wrap.Render(it.text.String())))
		case kindError:
			parts = append(parts, th.Error.Render(wrap.Render(it.text.String())))
		}
	}
	return strings.Join(parts, "\n\n")
}

func statusStyle(s string) lipgloss.Style {
	switch s {
	case "completed":
		return th.StatusDone
	case "failed":
		return th.StatusFailed
	case "in_progress":
		return th.StatusRunning
	}
	return th.StatusPending
}

func statusGlyph(s string) string {
	switch s {
	case "completed":
		return "✓"
	case "failed":
		return "✗"
	case "in_progress":
		return "●"
	}
	return "○"
}

const maxCardDiff = 40

func renderTool(c *toolCard, width int, terminal func(string) string) string {
	title := c.title
	if title == "" {
		title = "tool call"
	}
	head := statusStyle(c.status).Render(statusGlyph(c.status)) + " " + th.ToolTitle.Render(firstLine(title))
	meta := c.status
	if c.kind != "" {
		meta = c.kind + " · " + meta
	}
	lines := []string{head + "  " + th.ToolMeta.Render(meta)}
	for _, l := range c.locations {
		loc := l.Path
		if l.Line != nil {
			loc += fmt.Sprintf(":%d", *l.Line)
		}
		lines = append(lines, "  "+th.Location.Render(loc))
	}
	shown := 0
	for _, d := range c.diffs {
		if len(c.diffs) > 1 || len(c.locations) == 0 {
			lines = append(lines, "  "+th.Location.Render(d.Path))
		}
		for _, l := range Diff(d.OldText, d.NewText, 3) {
			if shown == maxCardDiff {
				lines = append(lines, "  "+th.Faint.Render("…"))
				break
			}
			shown++
			text := truncate(l.Text, width-4)
			switch l.Op {
			case '+':
				lines = append(lines, "  "+th.DiffAdd.Render("+ "+text))
			case '-':
				lines = append(lines, "  "+th.DiffDel.Render("- "+text))
			case '@':
				lines = append(lines, "  "+th.DiffHunk.Render("⋯ "+text))
			default:
				lines = append(lines, "  "+th.Faint.Render("  "+text))
			}
		}
	}
	out := c.output.String()
	for _, id := range c.terminals {
		if terminal != nil {
			out += terminal(id)
		}
	}
	if out = strings.TrimRight(out, "\n"); out != "" {
		tl := strings.Split(out, "\n")
		if len(tl) > 8 {
			tl = append([]string{"…"}, tl[len(tl)-8:]...)
		}
		for _, l := range tl {
			lines = append(lines, "  "+th.Terminal.Render("│ "+truncate(l, width-4)))
		}
	}
	if c.exit != "" {
		lines = append(lines, "  "+th.ToolMeta.Render(c.exit))
	}
	if len(c.diffs) == 0 && out == "" && c.status == "failed" && len(c.texts) > 0 {
		lines = append(lines, "  "+th.StatusFailed.Render(truncate(firstLine(c.texts[0]), width-4)))
	}
	return strings.Join(lines, "\n")
}

func renderPlan(entries []acp.PlanEntry) string {
	lines := []string{th.Bold.Render("Plan")}
	for _, e := range entries {
		switch e.Status {
		case "completed":
			lines = append(lines, th.PlanDone.Render("  ✓ "+e.Content))
		case "in_progress":
			lines = append(lines, th.PlanActive.Render("  ● "+e.Content))
		default:
			lines = append(lines, th.PlanPending.Render("  ○ "+e.Content))
		}
	}
	return strings.Join(lines, "\n")
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

func truncate(s string, w int) string {
	if w < 4 {
		w = 4
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}
