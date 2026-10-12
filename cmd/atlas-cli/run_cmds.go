package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/spf13/cobra"

	tea "charm.land/bubbletea/v2"

	"github.com/tryatlas/cli/internal/agents/acp"
	"github.com/tryatlas/cli/internal/project"
	atlasrun "github.com/tryatlas/cli/internal/run"
	"github.com/tryatlas/cli/internal/tui"
	"github.com/tryatlas/cli/internal/tui/chat"
)

func newRunCmd() *cobra.Command {
	c := withRun(leaf("run <agent>", "Run an ACP agent from the registry, with shared memory and full capture", cobra.ExactArgs(1)), runRun)
	c.Flags().String("prompt", "", "the first prompt (with --no-tui: send it, print the reply, and exit after the turn)")
	c.Flags().Bool("no-tui", false, "no chat screen: run one --prompt and print to the terminal")
	c.Flags().Bool("yes", false, "allow every permission the agent asks for (headless)")
	c.Flags().String("resume", "", "continue an earlier ACP session id (session/load or session/resume)")
	c.Flags().StringArray("image", nil, "attach an image to the --prompt (png, jpeg, gif, webp; repeatable)")
	return c
}

func runRun(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	id := args[0]
	prompt, _ := cmd.Flags().GetString("prompt")
	noTUI, _ := cmd.Flags().GetBool("no-tui")
	if prompt == "" || (!noTUI && interactive()) {
		if !interactive() {
			return errNoTUI
		}
		return runChat(cmd, id, prompt)
	}
	yes, _ := cmd.Flags().GetBool("yes")
	resume, _ := cmd.Flags().GetString("resume")
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if cwd, err = project.Canonical(cwd); err != nil {
		return err
	}
	p, err := project.Discover(ctx, cwd)
	if err != nil {
		return err
	}
	opts := hostOptions(false)
	opts.CaptureOptional = true
	h, err := atlasrun.StartHost(p.Root, opts)
	if err != nil {
		return err
	}
	if !h.Capturing() {
		fmt.Fprintln(stderr, "atlas-cli: capture is off for this run: another Atlas window is recording this Project")
	}
	tty := interactive()
	view := &headless{out: stdout, err: stderr, yes: yes, tty: tty, in: bufio.NewReader(cmd.InOrStdin())}
	sess := atlasrun.NewSession(id, cwd, h, view)
	if tty {
		sess.ChooseAuth = chooseAuth
	}
	ag, inst, err := agentsStore().Launch(ctx, id, cwd, sess)
	if err != nil {
		_, _ = h.Close()
		return err
	}
	sess.Agent = ag
	finish := func() {
		sess.Close(context.Background())
		ag.Close()
		if res, err := h.Close(); err == nil && res.Checkpoints > 0 {
			fmt.Fprintf(stderr, "Linked %d session commit(s).\n", res.Checkpoints)
		}
	}
	login := func(ctx context.Context, m acp.AuthMethod) error {
		c, ok := atlasrun.LoginCommand(ag.Command, m)
		if !ok {
			return fmt.Errorf("%s offers no sign-in command", ag.Name())
		}
		fmt.Fprintf(stderr, "Signing in to %s (%s)…\n", ag.Name(), m.Name)
		return atlasrun.Login(ctx, c, tty, stderr, func(u string) { fmt.Fprintf(stderr, "Open %s to sign in.\n", u) })
	}
	if resume != "" {
		err = sess.Reopen(ctx, resume)
	} else {
		err = sess.Open(ctx, login)
	}
	if err != nil {
		finish()
		return err
	}
	sess.ApplyDefaultMode(ctx, inst.DefaultMode)
	fmt.Fprintf(stderr, "%s · session %s\n", ag.Name(), sess.ID)

	// Ctrl-C cancels the turn (5 s grace); a second one stops waiting.
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	pctx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		n := 0
		for range sig {
			n++
			if n == 1 {
				fmt.Fprintln(stderr, "\nCancelling…")
				ag.Cancel(sess.ID)
				continue
			}
			stop()
		}
	}()
	images, err := imageBlocks(cmd)
	if err != nil {
		finish()
		return err
	}
	if len(images) > 0 && !ag.Init.AgentCapabilities.PromptCapabilities.Image {
		finish()
		return fmt.Errorf("%s does not accept images", ag.Name())
	}
	view.mu.Lock()
	view.live = true
	view.mu.Unlock()
	r, err := sess.Prompt(pctx, prompt, images, nil)
	view.endLine()
	if err != nil {
		if errors.Is(err, acp.ErrClosed) {
			err = ag.Exited()
		}
		finish()
		return err
	}
	if r.StopReason != acp.StopEndTurn {
		fmt.Fprintf(stderr, "Stopped: %s\n", r.StopReason)
	}
	finish()
	if r.StopReason == acp.StopRefusal {
		return errors.New("the agent refused the prompt")
	}
	return nil
}

// headless prints the reply to stdout and everything else to stderr, and
// answers permission prompts on the terminal (or by policy without one).
type headless struct {
	out, err io.Writer
	yes, tty bool
	in       *bufio.Reader

	mu      sync.Mutex
	live    bool // the prompt was sent: a session/load replay is not printed
	midLine bool
	shown   map[string]string // tool call id → last status printed
}

func (h *headless) endLine() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.midLine {
		fmt.Fprintln(h.out)
		h.midLine = false
	}
}

func (h *headless) Update(_ string, u acp.Update) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.live {
		return
	}
	if h.shown == nil {
		h.shown = map[string]string{}
	}
	switch u.Kind {
	case "agent_message_chunk":
		if t := u.Block().TextOf(); t != "" {
			fmt.Fprint(h.out, t)
			h.midLine = !strings.HasSuffix(t, "\n")
		}
	case "tool_call", "tool_call_update":
		title := h.shown["title:"+u.ToolCallID]
		if u.Title != nil && *u.Title != "" {
			title = *u.Title
			h.shown["title:"+u.ToolCallID] = title
		}
		st := u.Status
		if st == "" || st == h.shown[u.ToolCallID] {
			return
		}
		h.shown[u.ToolCallID] = st
		if h.midLine {
			fmt.Fprintln(h.out)
			h.midLine = false
		}
		fmt.Fprintf(h.err, "  • %s [%s]\n", title, st)
	case "plan":
		var entries []acp.PlanEntry
		if json.Unmarshal(u.Entries, &entries) == nil && len(entries) > 0 {
			fmt.Fprintln(h.err, "  Plan:")
			for _, e := range entries {
				fmt.Fprintf(h.err, "    [%s] %s\n", e.Status, e.Content)
			}
		}
	}
}

// Permission: --yes picks the first allow option; on a terminal the user
// chooses; otherwise the first reject option.
func (h *headless) Permission(ctx context.Context, req acp.PermissionRequest) (string, bool) {
	var tc struct {
		Title string `json:"title"`
	}
	_ = json.Unmarshal(req.ToolCall, &tc)
	pick := func(prefix string) (string, bool) {
		for _, o := range req.Options {
			if strings.HasPrefix(o.Kind, prefix) {
				return o.OptionID, true
			}
		}
		return "", false
	}
	if h.yes {
		id, ok := pick("allow_once") // never widen the agent's own approval mode
		if !ok {
			id, ok = pick("allow")
		}
		if ok {
			h.note("  ✓ allowed: %s\n", tc.Title)
			return id, true
		}
	}
	if !h.tty {
		h.note("  ✗ refused (no terminal; pass --yes to allow): %s\n", tc.Title)
		if id, ok := pick("reject_once"); ok {
			return id, true
		}
		return pick("reject")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.midLine {
		fmt.Fprintln(h.out)
		h.midLine = false
	}
	fmt.Fprintf(h.err, "The agent asks: %s\n", tc.Title)
	for i, o := range req.Options {
		fmt.Fprintf(h.err, "  %d) %s\n", i+1, o.Name)
	}
	answer := make(chan string, 1)
	go func() {
		fmt.Fprint(h.err, "Choose: ")
		line, _ := h.in.ReadString('\n')
		answer <- strings.TrimSpace(line)
	}()
	select {
	case <-ctx.Done():
		return "", false
	case a := <-answer:
		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(req.Options) {
			return req.Options[n-1].OptionID, true
		}
		return "", false
	}
}

func (h *headless) note(format string, a ...any) {
	h.mu.Lock()
	defer h.mu.Unlock()
	fmt.Fprintf(h.err, format, a...)
}

// Elicit: a URL is shown and accepted once the user presses Enter; forms
// need the chat screen and are declined.
func (h *headless) Elicit(ctx context.Context, e acp.Elicitation) acp.ElicitationAnswer {
	if e.Mode == "url" && h.tty {
		h.note("%s\nOpen %s, then press Enter.\n", e.Message, e.URL)
		done := make(chan struct{})
		go func() { _, _ = h.in.ReadString('\n'); close(done) }()
		select {
		case <-done:
			return acp.ElicitationAnswer{Action: "accept"}
		case <-ctx.Done():
			return acp.ElicitationAnswer{Action: "cancel"}
		}
	}
	h.note("  The agent asked for input (%s), which needs the chat screen: declined.\n", e.Mode)
	return acp.ElicitationAnswer{Action: "decline"}
}

func (h *headless) ElicitationComplete(string) {}

// runChat opens the chat screen on a session.
func runChat(cmd *cobra.Command, id, first string) error {
	ctx := cmd.Context()
	stderr := cmd.ErrOrStderr()
	resume, _ := cmd.Flags().GetString("resume")
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if cwd, err = project.Canonical(cwd); err != nil {
		return err
	}
	p, err := project.Discover(ctx, cwd)
	if err != nil {
		return err
	}
	opts := hostOptions(false)
	opts.CaptureOptional = true
	h, err := atlasrun.StartHost(p.Root, opts)
	if err != nil {
		return err
	}
	bridge := chat.NewBridge()
	c := &atlasrun.Chat{Agents: agentsStore(), Host: h, AgentID: id, Cwd: cwd, View: bridge,
		ChooseAuth: chooseAuth,
		Login: func(ctx context.Context, ag *acp.Agent, m acp.AuthMethod) error {
			lc, ok := atlasrun.LoginCommand(ag.Command, m)
			if !ok {
				return fmt.Errorf("%s offers no sign-in command", ag.Name())
			}
			fmt.Fprintf(stderr, "Signing in to %s (%s)…\n", ag.Name(), m.Name)
			return atlasrun.Login(ctx, lc, true, stderr, nil)
		}}
	fmt.Fprintf(stderr, "Starting %s…\n", id)
	if err := c.Start(ctx, resume); err != nil {
		_, _ = h.Close()
		return err
	}
	c.Login, c.ChooseAuth = nil, nil // the screen owns the terminal from here
	m := chat.New(ctx, c)
	if first != "" {
		m.Prefill(first)
	}
	prog := tea.NewProgram(m, tea.WithContext(ctx))
	bridge.Attach(prog)
	// A terminal closing or a kill still ends the session cleanly: memory's
	// session_end, the commit link and the last sync pass.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; ok {
			prog.Quit()
		}
	}()
	_, runErr := prog.Run()
	c.Close(context.Background())
	res, err := h.Close()
	if err == nil && res.Checkpoints > 0 {
		fmt.Fprintf(stderr, "Linked %d session commit(s).\n", res.Checkpoints)
	}
	if runErr != nil && !errors.Is(runErr, tea.ErrProgramKilled) && !errors.Is(runErr, context.Canceled) {
		return runErr
	}
	return nil
}

// chooseAuth asks which of the agent's sign-in methods to use.
func chooseAuth(ms []acp.AuthMethod) (acp.AuthMethod, bool) {
	labels := make([]string, len(ms))
	for i, m := range ms {
		labels[i] = m.Name
		if m.Description != "" {
			labels[i] += " — " + m.Description
		}
	}
	i, err := tui.Select("The agent needs you to sign in. How?", labels, 0)
	if err != nil {
		return acp.AuthMethod{}, false
	}
	return ms[i], true
}

// imageBlocks reads the --image files.
func imageBlocks(cmd *cobra.Command) ([]acp.ContentBlock, error) {
	paths, _ := cmd.Flags().GetStringArray("image")
	var out []acp.ContentBlock
	for _, p := range paths {
		b, err := acp.ImageBlock(p)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}
