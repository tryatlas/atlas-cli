package chat

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// choice is one row of a picker.
type choice struct {
	label, hint string
	value       any
}

// picker is a single-choice list in an overlay.
type picker struct {
	title   string
	choices []choice
	cursor  int
	// done is called with the chosen index, or -1 on escape.
	done func(i int) tea.Cmd
}

func (p *picker) key(k string) (tea.Cmd, bool) {
	switch k {
	case "up", "ctrl+p", "shift+tab":
		if p.cursor > 0 {
			p.cursor--
		}
	case "down", "ctrl+n", "tab":
		if p.cursor < len(p.choices)-1 {
			p.cursor++
		}
	case "enter":
		return p.done(p.cursor), true
	case "esc", "ctrl+c":
		return p.done(-1), true
	default:
		if n, err := strconv.Atoi(k); err == nil && n >= 1 && n <= len(p.choices) {
			return p.done(n - 1), true
		}
	}
	return nil, false
}

func (p *picker) view(width int) string {
	var b strings.Builder
	b.WriteString(th.OverlayTitle.Render(p.title) + "\n")
	for i, c := range p.choices {
		line := fmt.Sprintf("%d. %s", i+1, c.label)
		if c.hint != "" {
			line += "  " + th.Faint.Render(c.hint)
		}
		if i == p.cursor {
			b.WriteString(th.Selected.Render("› "+line) + "\n")
		} else {
			b.WriteString(th.Option.Render("  "+line) + "\n")
		}
	}
	b.WriteString(th.Hint.Render("↑/↓ or 1-9 · enter to choose · esc to cancel"))
	return th.Overlay.Width(min(width, 100)).Render(b.String())
}

// ── elicitation form ─────────────────────────────────────────────────────────

// field is one property of a requested schema.
type field struct {
	name, title, desc, typ string
	enum                   []string
	required               bool
	value                  string // text form of the value
}

// formOverlay renders a form elicitation from its requestedSchema.
type formOverlay struct {
	message string
	fields  []*field
	cursor  int
	err     string
	reply   func(action string, content map[string]any)
}

// orderedKeys reads an object's keys in document order.
func orderedKeys(raw json.RawMessage) []string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return keys
		}
		k, _ := t.(string)
		keys = append(keys, k)
		var skip json.RawMessage
		if dec.Decode(&skip) != nil {
			return keys
		}
	}
	return keys
}

func newForm(message string, schema json.RawMessage, reply func(string, map[string]any)) *formOverlay {
	var s struct {
		Properties json.RawMessage `json:"properties"`
		Required   []string        `json:"required"`
	}
	_ = json.Unmarshal(schema, &s)
	var props map[string]struct {
		Type        string   `json:"type"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Enum        []string `json:"enum"`
		Default     any      `json:"default"`
	}
	_ = json.Unmarshal(s.Properties, &props)
	req := map[string]bool{}
	for _, r := range s.Required {
		req[r] = true
	}
	f := &formOverlay{message: message, reply: reply}
	for _, k := range orderedKeys(s.Properties) {
		p := props[k]
		fl := &field{name: k, title: p.Title, desc: p.Description, typ: p.Type, enum: p.Enum, required: req[k]}
		if fl.title == "" {
			fl.title = k
		}
		switch {
		case p.Default != nil:
			fl.value = fmt.Sprint(p.Default)
		case fl.typ == "boolean":
			fl.value = "false"
		case len(fl.enum) > 0:
			fl.value = fl.enum[0]
		}
		f.fields = append(f.fields, fl)
	}
	return f
}

// content converts the fields to typed values; it reports a field that is
// required and empty, or not a number.
func (f *formOverlay) content() (map[string]any, string) {
	out := map[string]any{}
	for _, fl := range f.fields {
		v := strings.TrimSpace(fl.value)
		if v == "" {
			if fl.required {
				return nil, fl.title + " is required"
			}
			continue
		}
		switch fl.typ {
		case "boolean":
			out[fl.name] = v == "true"
		case "number":
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil, fl.title + " must be a number"
			}
			out[fl.name] = n
		case "integer":
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, fl.title + " must be a whole number"
			}
			out[fl.name] = n
		default:
			out[fl.name] = v
		}
	}
	return out, ""
}

func (f *formOverlay) key(msg tea.KeyPressMsg) bool {
	k := msg.String()
	var cur *field
	if len(f.fields) > 0 {
		cur = f.fields[f.cursor]
	}
	switch k {
	case "esc":
		f.reply("cancel", nil)
		return true
	case "ctrl+d":
		f.reply("decline", nil)
		return true
	case "tab", "down":
		if f.cursor < len(f.fields)-1 {
			f.cursor++
		}
		return false
	case "shift+tab", "up":
		if f.cursor > 0 {
			f.cursor--
		}
		return false
	case "enter":
		c, problem := f.content()
		if problem != "" {
			f.err = problem
			return false
		}
		f.reply("accept", c)
		return true
	}
	if cur == nil {
		return false
	}
	switch {
	case cur.typ == "boolean" && (k == "space" || k == " " || k == "left" || k == "right"):
		if cur.value == "true" {
			cur.value = "false"
		} else {
			cur.value = "true"
		}
	case len(cur.enum) > 0 && (k == "left" || k == "right" || k == "space" || k == " "):
		i := 0
		for j, e := range cur.enum {
			if e == cur.value {
				i = j
			}
		}
		if k == "left" {
			i = (i - 1 + len(cur.enum)) % len(cur.enum)
		} else {
			i = (i + 1) % len(cur.enum)
		}
		cur.value = cur.enum[i]
	case k == "backspace":
		r := []rune(cur.value)
		if len(r) > 0 {
			cur.value = string(r[:len(r)-1])
		}
	default:
		if t := msg.Text; t != "" && cur.typ != "boolean" && len(cur.enum) == 0 {
			cur.value += t
		}
	}
	return false
}

func (f *formOverlay) view(width int) string {
	var b strings.Builder
	b.WriteString(th.OverlayTitle.Render("The agent asks") + "\n")
	if f.message != "" {
		b.WriteString(f.message + "\n\n")
	}
	for i, fl := range f.fields {
		label := fl.title
		if fl.required {
			label += " *"
		}
		val := fl.value
		switch {
		case fl.typ == "boolean":
			if val == "true" {
				val = "[x]"
			} else {
				val = "[ ]"
			}
		case len(fl.enum) > 0:
			val = "‹ " + val + " ›"
		}
		line := label + ": " + val
		if i == f.cursor {
			b.WriteString(th.Selected.Render("› "+line) + "\n")
			if fl.desc != "" {
				b.WriteString(th.Faint.Render("    "+fl.desc) + "\n")
			}
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	if f.err != "" {
		b.WriteString(th.Error.Render(f.err) + "\n")
	}
	b.WriteString(th.Hint.Render("tab to move · space or ←/→ to change · enter to send · ctrl+d to decline · esc to cancel"))
	return th.Overlay.Width(min(width, 100)).Render(b.String())
}

// urlOverlay is a URL elicitation, or a sign-in link.
type urlOverlay struct {
	id, message, url string
	reply            func(action string)
}

func (u *urlOverlay) key(k string) bool {
	switch k {
	case "enter":
		u.reply("accept")
		return true
	case "ctrl+d":
		u.reply("decline")
		return true
	case "esc":
		u.reply("cancel")
		return true
	case "o":
		openURL(u.url)
	}
	return false
}

func (u *urlOverlay) view(width int) string {
	body := th.OverlayTitle.Render("The agent asks you to open a link") + "\n"
	if u.message != "" {
		body += u.message + "\n"
	}
	body += th.Location.Render(u.url) + "\n" + th.Hint.Render("o to open in a browser · enter when done · ctrl+d to decline · esc to cancel")
	return th.Overlay.Width(min(width, 100)).Render(body)
}

// lineInput is a one-line text prompt (an image path).
type lineInput struct {
	title, value, err string
	done              func(value string, ok bool) (tea.Cmd, string)
}

func (l *lineInput) key(msg tea.KeyPressMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "esc":
		c, _ := l.done("", false)
		return c, true
	case "enter":
		c, problem := l.done(strings.TrimSpace(l.value), true)
		if problem != "" {
			l.err = problem
			return nil, false
		}
		return c, true
	case "backspace":
		r := []rune(l.value)
		if len(r) > 0 {
			l.value = string(r[:len(r)-1])
		}
	default:
		l.value += msg.Text
	}
	return nil, false
}

func (l *lineInput) paste(s string) { l.value += strings.TrimSpace(s) }

func (l *lineInput) view(width int) string {
	body := th.OverlayTitle.Render(l.title) + "\n› " + l.value + "▏"
	if l.err != "" {
		body += "\n" + th.Error.Render(l.err)
	}
	body += "\n" + th.Hint.Render("enter to attach · esc to cancel")
	return th.Overlay.Width(min(width, 100)).Render(body)
}
