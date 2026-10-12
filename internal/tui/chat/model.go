package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/tryatlas/cli/internal/agents/acp"
)

// state is the session's phase.
type state int

const (
	stateIdle state = iota
	stateRunning
	stateCancelling
	stateDisconnected
	stateRestarting
)

// Results of background work.
type (
	promptDoneMsg struct {
		r   *acp.PromptResult
		err error
	}
	disconnectedMsg struct{}
	restartedMsg    struct{ err error }
	opDoneMsg       struct {
		what string
		err  error
	}
	sessionsMsg struct {
		list []acp.ListedSession
		err  error
	}
	resumedMsg struct{ err error }
)

// Model is the chat screen.
type Model struct {
	drv     Driver
	ctx     context.Context
	tr      *transcript
	vp      viewport.Model
	in      textarea.Model
	width   int
	height  int
	state   state
	follow  bool // keep the view at the bottom
	quitArm bool

	commands []acp.SlashCommand
	slash    int // cursor in the slash list
	images   []acp.ContentBlock
	imgNames []string

	// usage line
	used, size    *int64
	cost          string
	tokIn, tokOut int64

	// overlays: at most one is open; a permission or elicitation waits
	// behind another overlay until it closes.
	picker  *picker
	form    *formOverlay
	url     *urlOverlay
	line    *lineInput
	pending []tea.Msg
	asks    map[context.Context]func() // close a question the agent withdrew
}

// New builds the screen for a running session.
func New(ctx context.Context, drv Driver) *Model {
	in := textarea.New()
	in.Placeholder = "Message the agent · / for commands · ctrl+h for keys"
	in.ShowLineNumbers = false
	in.Prompt = "› "
	in.SetHeight(3)
	in.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j"))
	in.Focus()
	m := &Model{drv: drv, ctx: ctx, tr: newTranscript(), vp: viewport.New(), in: in, follow: true, asks: map[context.Context]func(){}}
	info := drv.Info()
	m.tr.add(kindNotice, fmt.Sprintf("%s · session %s · %s", info.AgentName, info.SessionID, info.Cwd))
	if !info.Capturing {
		m.tr.add(kindNotice, "Capture is off for this run: another Atlas window is recording this Project.")
	}
	return m
}

// Prefill puts text in the input, to send with enter.
func (m *Model) Prefill(text string) { m.in.SetValue(text) }

// Init starts watching the agent's connection.
func (m *Model) Init() tea.Cmd { return tea.Batch(textarea.Blink, m.watch()) }

func (m *Model) watch() tea.Cmd {
	done := m.drv.Done()
	return func() tea.Msg {
		<-done
		return disconnectedMsg{}
	}
}

func (m *Model) note(kind itemKind, s string) { m.tr.add(kind, s) }

// Update handles one message.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.width < 20 || m.height < 8 { // an unsized terminal
			m.width, m.height = max(m.width, 80), max(m.height, 24)
		}
		m.in.SetWidth(m.width)
		m.layout()
	case updateMsg:
		m.onUpdate(msg.u)
	case permMsg:
		if m.busyOverlay() {
			m.pending = append(m.pending, msg)
			break
		}
		m.openPermission(msg)
	case elicitMsg:
		if m.busyOverlay() {
			m.pending = append(m.pending, msg)
			break
		}
		m.openElicit(msg)
	case elicitDoneMsg:
		if m.url != nil && m.url.id == msg.id {
			m.url.reply("accept")
			m.url = nil
			m.next()
		}
	case dismissMsg:
		if f := m.asks[msg.ctx]; f != nil {
			f()
		}
	case promptDoneMsg:
		m.state = stateIdle
		switch {
		case msg.err != nil:
			m.note(kindError, "Error: "+msg.err.Error())
		case msg.r.StopReason != acp.StopEndTurn:
			m.note(kindNotice, "Stopped: "+msg.r.StopReason)
		}
		if msg.r != nil && msg.r.Usage != nil {
			m.tokIn += msg.r.Usage.InputTokens
			m.tokOut += msg.r.Usage.OutputTokens
		}
	case disconnectedMsg:
		if m.state != stateRestarting {
			m.state = stateDisconnected
			m.note(kindError, "The agent disconnected. "+m.drv.Exit()+"\nctrl+r restarts it and reopens this session.")
		}
	case restartedMsg:
		if msg.err != nil {
			m.state = stateDisconnected
			m.note(kindError, "Restart failed: "+msg.err.Error())
		} else {
			m.state = stateIdle
			m.note(kindNotice, "Restarted · session "+m.drv.Info().SessionID)
			cmds = append(cmds, m.watch())
		}
	case opDoneMsg:
		if msg.err != nil {
			m.note(kindError, msg.what+" failed: "+msg.err.Error())
		} else {
			m.note(kindNotice, msg.what)
		}
	case sessionsMsg:
		m.openSessions(msg)
	case openValueMsg:
		m.openValue(msg.o)
	case resumedMsg:
		m.state = stateIdle
		if msg.err != nil {
			m.note(kindError, "Could not open that session: "+msg.err.Error())
		} else {
			m.note(kindNotice, "Session "+m.drv.Info().SessionID)
		}
	case tea.PasteMsg:
		if m.line != nil {
			m.line.paste(msg.Content)
			break
		}
		var c tea.Cmd
		m.in, c = m.in.Update(msg)
		cmds = append(cmds, c)
	case tea.KeyPressMsg:
		cmds = append(cmds, m.onKey(msg))
	default:
		var c tea.Cmd
		m.in, c = m.in.Update(msg)
		cmds = append(cmds, c)
	}
	m.refresh()
	return m, tea.Batch(cmds...)
}

func (m *Model) busyOverlay() bool {
	return m.picker != nil || m.form != nil || m.url != nil || m.line != nil
}

// next opens the oldest question that waited behind another overlay.
func (m *Model) next() {
	for len(m.pending) > 0 && !m.busyOverlay() {
		p := m.pending[0]
		m.pending = m.pending[1:]
		switch p := p.(type) {
		case permMsg:
			if p.ctx.Err() == nil {
				m.openPermission(p)
			}
		case elicitMsg:
			if p.ctx.Err() == nil {
				m.openElicit(p)
			}
		}
	}
}

func (m *Model) onUpdate(u acp.Update) {
	m.tr.apply(u)
	switch u.Kind {
	case "available_commands_update":
		var cs []acp.SlashCommand
		if json.Unmarshal(u.AvailableCommands, &cs) == nil {
			m.commands = cs
		}
	case "usage_update":
		if u.Used != nil {
			m.used = u.Used
		}
		if u.Size != nil {
			m.size = u.Size
		}
		if u.Cost != nil {
			m.cost = fmt.Sprintf("%.2f %s", u.Cost.Amount, u.Cost.Currency)
		}
	case "session_info_update":
		if u.Title != nil && *u.Title != "" {
			m.note(kindNotice, "Title: "+*u.Title)
		}
	}
}

// ── keys ─────────────────────────────────────────────────────────────────────

func (m *Model) onKey(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	switch {
	case m.picker != nil:
		c, closed := m.picker.key(k)
		if closed {
			m.picker = nil
			m.next()
		}
		return c
	case m.form != nil:
		if m.form.key(msg) {
			m.form = nil
			m.next()
		}
		return nil
	case m.url != nil:
		if m.url.key(k) {
			m.url = nil
			m.next()
		}
		return nil
	case m.line != nil:
		c, closed := m.line.key(msg)
		if closed {
			m.line = nil
			m.next()
		}
		return c
	}
	if k != "ctrl+c" {
		m.quitArm = false
	}
	if sl := m.slashMatches(); len(sl) > 0 {
		switch k {
		case "up":
			if m.slash > 0 {
				m.slash--
			}
			return nil
		case "down":
			if m.slash < len(sl)-1 {
				m.slash++
			}
			return nil
		case "tab", "enter":
			c := sl[min(m.slash, len(sl)-1)]
			m.in.SetValue("/" + c.Name + " ")
			m.in.CursorEnd()
			m.slash = 0
			if k == "tab" || c.Input != nil {
				return nil
			}
		}
	}
	switch k {
	case "ctrl+c":
		if m.state == stateRunning {
			return m.cancel()
		}
		if m.quitArm || strings.TrimSpace(m.in.Value()) == "" {
			return tea.Quit
		}
		m.quitArm = true
		m.note(kindNotice, "Press ctrl+c again to quit.")
		return nil
	case "esc":
		if m.state == stateRunning {
			return m.cancel()
		}
		return nil
	case "enter":
		return m.send()
	case "ctrl+t":
		m.openModes()
		return nil
	case "ctrl+p":
		m.openConfig("model")
		return nil
	case "ctrl+g":
		m.openConfig("")
		return nil
	case "ctrl+o":
		m.openAttach()
		return nil
	case "ctrl+l":
		return m.listSessions()
	case "ctrl+n":
		return m.newSession()
	case "ctrl+r":
		return m.restart()
	case "ctrl+e":
		m.tr.expanded = !m.tr.expanded
		return nil
	case "ctrl+h", "f1":
		m.note(kindNotice, helpText(m.drv.Info()))
		return nil
	case "pgup", "pgdown", "ctrl+u", "ctrl+d":
		var c tea.Cmd
		m.vp, c = m.vp.Update(msg)
		m.follow = m.vp.AtBottom()
		return c
	}
	var c tea.Cmd
	m.in, c = m.in.Update(msg)
	m.slash = min(m.slash, max(0, len(m.slashMatches())-1))
	return c
}

func helpText(info Info) string {
	lines := []string{
		"enter send · alt+enter newline · esc or ctrl+c cancel a turn · ctrl+c twice quit",
		"ctrl+l sessions · ctrl+n new session · ctrl+r restart the agent · ctrl+e expand thinking · pgup/pgdown scroll",
	}
	var pick []string
	if info.Modes != nil && len(info.Modes.AvailableModes) > 0 {
		pick = append(pick, "ctrl+t mode")
	}
	if hasCategory(info.Config, "model") {
		pick = append(pick, "ctrl+p model")
	}
	if len(info.Config) > 0 {
		pick = append(pick, "ctrl+g settings")
	}
	if info.Caps.PromptCapabilities.Image {
		pick = append(pick, "ctrl+o attach an image")
	}
	if len(pick) > 0 {
		lines = append(lines, strings.Join(pick, " · "))
	}
	return strings.Join(lines, "\n")
}

func hasCategory(opts []acp.ConfigOption, cat string) bool {
	for _, o := range opts {
		if o.Category == cat {
			return true
		}
	}
	return false
}

// slashMatches lists the advertised commands matching "/prefix" while the
// input is a bare command name.
func (m *Model) slashMatches() []acp.SlashCommand {
	v := m.in.Value()
	if !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \n") || len(m.commands) == 0 {
		return nil
	}
	prefix := strings.ToLower(v[1:])
	var out []acp.SlashCommand
	for _, c := range m.commands {
		if strings.HasPrefix(strings.ToLower(c.Name), prefix) {
			out = append(out, c)
		}
	}
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}

// send starts a turn: the text first, then attached images.
func (m *Model) send() tea.Cmd {
	text := strings.TrimRight(m.in.Value(), "\n")
	if strings.TrimSpace(text) == "" && len(m.images) == 0 {
		return nil
	}
	switch m.state {
	case stateRunning, stateCancelling:
		m.note(kindNotice, "The agent is still working: esc cancels the turn.")
		return nil
	case stateDisconnected, stateRestarting:
		m.note(kindNotice, "The agent is not connected: ctrl+r restarts it.")
		return nil
	}
	images := m.images
	label := text
	for _, n := range m.imgNames {
		label += "\n[image " + n + "]"
	}
	m.tr.add(kindUser, label)
	m.in.Reset()
	m.images, m.imgNames = nil, nil
	m.state = stateRunning
	m.follow = true
	drv, ctx := m.drv, m.ctx
	return func() tea.Msg {
		r, err := drv.Prompt(ctx, text, images)
		return promptDoneMsg{r, err}
	}
}

func (m *Model) cancel() tea.Cmd {
	if m.state != stateRunning {
		return nil
	}
	m.state = stateCancelling
	m.note(kindNotice, "Cancelling…")
	m.drv.Cancel()
	return nil
}

// ── agent questions ──────────────────────────────────────────────────────────

func (m *Model) openPermission(p permMsg) {
	var tc struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(p.req.ToolCall, &tc)
	title := "The agent asks to proceed"
	if tc.Title != "" {
		title = "The agent asks: " + firstLine(tc.Title)
	}
	pk := &picker{title: title}
	for _, o := range p.req.Options {
		pk.choices = append(pk.choices, choice{label: o.Name, hint: strings.ReplaceAll(o.Kind, "_", " "), value: o.OptionID})
	}
	answered := false
	pk.done = func(i int) tea.Cmd {
		if answered {
			return nil
		}
		answered = true
		delete(m.asks, p.ctx)
		if i < 0 {
			p.reply <- permAnswer{}
			return nil
		}
		p.reply <- permAnswer{id: pk.choices[i].value.(string), ok: true}
		return nil
	}
	m.asks[p.ctx] = func() {
		if m.picker == pk {
			m.picker = nil
			m.next()
		}
		delete(m.asks, p.ctx)
	}
	m.picker = pk
}

func (m *Model) openElicit(e elicitMsg) {
	once := false
	reply := func(action string, content map[string]any) {
		if once {
			return
		}
		once = true
		delete(m.asks, e.ctx)
		e.reply <- acp.ElicitationAnswer{Action: action, Content: content}
	}
	if e.e.Mode == "url" {
		u := &urlOverlay{id: e.e.ElicitationID, message: e.e.Message, url: e.e.URL, reply: func(a string) { reply(a, nil) }}
		m.url = u
		m.asks[e.ctx] = func() {
			if m.url == u {
				m.url = nil
				m.next()
			}
		}
		return
	}
	f := newForm(e.e.Message, e.e.RequestedSchema, reply)
	m.form = f
	m.asks[e.ctx] = func() {
		if m.form == f {
			m.form = nil
			m.next()
		}
	}
}

// ── pickers ──────────────────────────────────────────────────────────────────

func (m *Model) openModes() {
	info := m.drv.Info()
	if info.Modes == nil || len(info.Modes.AvailableModes) == 0 {
		m.note(kindNotice, "This agent offers no modes.")
		return
	}
	pk := &picker{title: "Mode"}
	for i, md := range info.Modes.AvailableModes {
		hint := md.Description
		if md.ID == info.Modes.CurrentModeID {
			pk.cursor = i
			hint = "current · " + hint
		}
		pk.choices = append(pk.choices, choice{label: md.Name, hint: strings.TrimSuffix(hint, " · "), value: md.ID})
	}
	drv, ctx := m.drv, m.ctx
	pk.done = func(i int) tea.Cmd {
		if i < 0 {
			return nil
		}
		id, name := pk.choices[i].value.(string), pk.choices[i].label
		return func() tea.Msg { return opDoneMsg{"Mode: " + name, drv.SetMode(ctx, id)} }
	}
	m.picker = pk
}

// openConfig picks a config option (of a category, or any), then its value.
func (m *Model) openConfig(category string) {
	info := m.drv.Info()
	var opts []acp.ConfigOption
	for _, o := range info.Config {
		if category == "" || o.Category == category {
			opts = append(opts, o)
		}
	}
	switch {
	case len(opts) == 0 && category == "model":
		m.note(kindNotice, "This agent offers no model choice.")
		return
	case len(opts) == 0:
		m.note(kindNotice, "This agent offers no settings.")
		return
	case len(opts) == 1:
		m.openValue(opts[0])
		return
	}
	pk := &picker{title: "Settings"}
	for _, o := range opts {
		pk.choices = append(pk.choices, choice{label: o.Name, hint: currentLabel(o), value: o})
	}
	pk.done = func(i int) tea.Cmd {
		if i >= 0 {
			o := pk.choices[i].value.(acp.ConfigOption)
			// Open the value picker after this one closes.
			return func() tea.Msg { return openValueMsg{o} }
		}
		return nil
	}
	m.picker = pk
}

type openValueMsg struct{ o acp.ConfigOption }

func currentLabel(o acp.ConfigOption) string {
	if o.Type == "boolean" {
		return string(o.CurrentValue)
	}
	cur := o.Current()
	for _, c := range o.Choices() {
		if c.Value == cur {
			return c.Name
		}
	}
	return cur
}

func (m *Model) openValue(o acp.ConfigOption) {
	drv, ctx := m.drv, m.ctx
	if o.Type == "boolean" {
		var on bool
		_ = json.Unmarshal(o.CurrentValue, &on)
		pk := &picker{title: o.Name, choices: []choice{{label: "On", value: true}, {label: "Off", value: false}}}
		if !on {
			pk.cursor = 1
		}
		pk.done = func(i int) tea.Cmd {
			if i < 0 {
				return nil
			}
			v := pk.choices[i].value.(bool)
			return func() tea.Msg { return opDoneMsg{o.Name + ": " + pk.choices[i].label, drv.SetConfig(ctx, o.ID, v)} }
		}
		m.picker = pk
		return
	}
	pk := &picker{title: o.Name}
	cur := o.Current()
	for i, c := range o.Choices() {
		hint := c.Group
		if c.Value == cur {
			pk.cursor = i
			hint = strings.TrimSpace("current " + hint)
		}
		pk.choices = append(pk.choices, choice{label: c.Name, hint: hint, value: c.Value})
	}
	pk.done = func(i int) tea.Cmd {
		if i < 0 {
			return nil
		}
		v, label := pk.choices[i].value.(string), pk.choices[i].label
		return func() tea.Msg { return opDoneMsg{o.Name + ": " + label, drv.SetConfig(ctx, o.ID, v)} }
	}
	m.picker = pk
}

func (m *Model) openAttach() {
	if !m.drv.Info().Caps.PromptCapabilities.Image {
		m.note(kindNotice, "This agent does not accept images.")
		return
	}
	m.line = &lineInput{title: "Attach an image (path)", done: func(path string, ok bool) (tea.Cmd, string) {
		if !ok || path == "" {
			return nil, ""
		}
		block, err := acp.ImageBlock(path)
		if err != nil {
			return nil, err.Error()
		}
		m.images = append(m.images, block)
		m.imgNames = append(m.imgNames, filepath.Base(path))
		return nil, ""
	}}
}

// ── sessions and restart ─────────────────────────────────────────────────────

func (m *Model) listSessions() tea.Cmd {
	if !m.drv.Info().Caps.CanList() {
		m.note(kindNotice, "This agent cannot list its sessions.")
		return nil
	}
	drv, ctx := m.drv, m.ctx
	return func() tea.Msg {
		l, err := drv.ListSessions(ctx)
		return sessionsMsg{l, err}
	}
}

func (m *Model) openSessions(msg sessionsMsg) {
	if msg.err != nil {
		m.note(kindError, "Could not list sessions: "+msg.err.Error())
		return
	}
	if len(msg.list) == 0 {
		m.note(kindNotice, "No earlier sessions here.")
		return
	}
	sort.SliceStable(msg.list, func(i, j int) bool { return msg.list[i].UpdatedAt > msg.list[j].UpdatedAt })
	pk := &picker{title: "Resume a session"}
	for _, s := range msg.list {
		title := s.Title
		if title == "" {
			title = s.SessionID
		}
		pk.choices = append(pk.choices, choice{label: truncate(firstLine(title), 70), hint: when(s.UpdatedAt), value: s.SessionID})
	}
	drv, ctx := m.drv, m.ctx
	pk.done = func(i int) tea.Cmd {
		if i < 0 || m.state == stateRunning {
			return nil
		}
		id := pk.choices[i].value.(string)
		m.tr = newTranscript()
		m.tr.add(kindNotice, "Opening session "+id+"…")
		m.state = stateRestarting
		return func() tea.Msg { return resumedMsg{drv.Resume(ctx, id)} }
	}
	m.picker = pk
}

func when(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Local().Format("Jan 2 15:04")
}

func (m *Model) newSession() tea.Cmd {
	if m.state == stateRunning {
		return nil
	}
	m.tr = newTranscript()
	m.state = stateRestarting
	drv, ctx := m.drv, m.ctx
	return func() tea.Msg { return resumedMsg{drv.NewSession(ctx)} }
}

func (m *Model) restart() tea.Cmd {
	if m.state == stateRunning {
		m.note(kindNotice, "Cancel the turn first (esc).")
		return nil
	}
	m.state = stateRestarting
	m.note(kindNotice, "Restarting the agent…")
	drv, ctx := m.drv, m.ctx
	return func() tea.Msg { return restartedMsg{drv.Restart(ctx)} }
}

// ── layout and view ──────────────────────────────────────────────────────────

func (m *Model) layout() {
	inH := m.in.Height() + 1
	bar := 2
	h := m.height - inH - bar
	if h < 3 {
		h = 3
	}
	m.vp.SetWidth(m.width)
	m.vp.SetHeight(h)
}

func (m *Model) refresh() {
	if m.width == 0 {
		return
	}
	m.vp.SetContent(m.tr.render(m.width-1, m.drv.TerminalOutput))
	if m.follow {
		m.vp.GotoBottom()
	}
}

func (m *Model) statusLine() string {
	info := m.drv.Info()
	parts := []string{th.BarValue.Render(info.AgentName)}
	if info.Modes != nil && info.Modes.CurrentModeID != "" {
		name := info.Modes.CurrentModeID
		for _, md := range info.Modes.AvailableModes {
			if md.ID == name {
				name = md.Name
			}
		}
		parts = append(parts, th.BarKey.Render("mode ")+name)
	}
	for _, o := range info.Config {
		if o.Category == "model" {
			parts = append(parts, th.BarKey.Render("model ")+currentLabel(o))
		}
	}
	switch m.state {
	case stateRunning:
		parts = append(parts, th.StatusRunning.Render("working… (esc to cancel)"))
	case stateCancelling:
		parts = append(parts, th.StatusRunning.Render("cancelling…"))
	case stateDisconnected:
		parts = append(parts, th.StatusFailed.Render("disconnected (ctrl+r to restart)"))
	case stateRestarting:
		parts = append(parts, th.StatusRunning.Render("starting…"))
	}
	return th.Bar.Render(strings.Join(parts, th.Faint.Render(" · ")))
}

func compact(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func (m *Model) usageLine() string {
	var parts []string
	if m.used != nil && m.size != nil && *m.size > 0 {
		parts = append(parts, fmt.Sprintf("context %s/%s (%d%%)", compact(*m.used), compact(*m.size), *m.used*100 / *m.size))
	}
	if m.tokIn+m.tokOut > 0 {
		parts = append(parts, fmt.Sprintf("tokens in %s · out %s", compact(m.tokIn), compact(m.tokOut)))
	}
	if m.cost != "" {
		parts = append(parts, m.cost)
	}
	if len(m.imgNames) > 0 {
		parts = append(parts, "attached: "+strings.Join(m.imgNames, ", "))
	}
	if !m.drv.Info().Capturing {
		parts = append(parts, "capture off")
	}
	return th.Usage.Render(strings.Join(parts, " · "))
}

// View draws the screen.
func (m *Model) View() tea.View {
	if m.width == 0 {
		return tea.NewView("")
	}
	body := m.vp.View()
	var over string
	switch {
	case m.picker != nil:
		over = m.picker.view(m.width)
	case m.form != nil:
		over = m.form.view(m.width)
	case m.url != nil:
		over = m.url.view(m.width)
	case m.line != nil:
		over = m.line.view(m.width)
	}
	if sl := m.slashMatches(); over == "" && len(sl) > 0 {
		var b strings.Builder
		for i, c := range sl {
			line := "/" + c.Name + "  " + th.Faint.Render(c.Description)
			if i == m.slash {
				b.WriteString(th.Selected.Render("› "+line) + "\n")
			} else {
				b.WriteString("  " + line + "\n")
			}
		}
		over = th.Overlay.Width(min(m.width, 100)).Render(strings.TrimRight(b.String(), "\n"))
	}
	if over != "" {
		// The overlay sits at the bottom of the transcript area.
		lines := strings.Split(body, "\n")
		ol := strings.Split(over, "\n")
		if len(ol) < len(lines) {
			lines = append(lines[:len(lines)-len(ol)], ol...)
			body = strings.Join(lines, "\n")
		} else {
			body = over
		}
	}
	screen := lipgloss.JoinVertical(lipgloss.Left, body, m.statusLine(), m.usageLine(), m.in.View())
	v := tea.NewView(screen)
	v.AltScreen = true
	return v
}

// openURL opens a link in the browser.
func openURL(u string) {
	name := "xdg-open"
	if runtime.GOOS == "darwin" {
		name = "open"
	}
	_ = exec.Command(name, u).Start() //nolint:gosec // G204: a URL the agent offered, opened on the user's key press
}
