package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/agents/ambient"
	"github.com/tryatlas/atlas-cli/internal/auth"
	gitlink "github.com/tryatlas/atlas-cli/internal/capture/git"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/listener"
	"github.com/tryatlas/atlas-cli/internal/procs"
	"github.com/tryatlas/atlas-cli/internal/project"
)

const defaultAnthropic = "https://api.anthropic.com"

// anthropicUpstream is where Claude traffic goes after the proxy: the
// ANTHROPIC_BASE_URL the user had (a gateway, Bedrock proxy), else the API.
func anthropicUpstream() (*url.URL, error) {
	raw := strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL"))
	if raw == "" || strings.Contains(raw, "/proxy/anthropic") {
		raw = defaultAnthropic
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("ANTHROPIC_BASE_URL %q is not a URL", raw)
	}
	return u, nil
}

// shellEnv is the environment for agents run in the shell: each adapter's
// variables, the listener port, and loopback kept off any proxy. shimDir,
// when set, is prepended to PATH (ADR-0004).
func shellEnv(port int, adapters []ambient.Adapter, shimDir string) []string {
	set := map[string]string{
		"ATLAS_PROXY_PORT": fmt.Sprint(port),
		"ATLAS_SHELL":      "1",
	}
	for _, a := range adapters {
		if s, err := a.Shell(ambient.Listener{Port: port}); err == nil {
			for k, v := range s.Env {
				set[k] = v
			}
		}
	}
	noProxy := os.Getenv("NO_PROXY")
	if noProxy == "" {
		noProxy = os.Getenv("no_proxy")
	}
	if !strings.Contains(noProxy, "127.0.0.1") {
		noProxy = strings.Trim(noProxy+",127.0.0.1,localhost", ",")
	}
	set["NO_PROXY"], set["no_proxy"] = noProxy, noProxy
	if shimDir != "" {
		set["PATH"] = shimDir + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := set[k]; !ok {
			env = append(env, kv)
		}
	}
	for k, v := range set {
		env = append(env, k+"="+v)
	}
	return env
}

// writeShims puts each adapter's wrapper scripts in a private directory that
// only the shell's PATH sees.
func writeShims(adapters []ambient.Adapter, port int) (string, error) {
	var shims map[string]string
	for _, a := range adapters {
		s, err := a.Shell(ambient.Listener{Port: port})
		if err != nil || len(s.Shims) == 0 {
			continue
		}
		if shims == nil {
			shims = map[string]string{}
		}
		for k, v := range s.Shims {
			shims[k] = v
		}
	}
	if len(shims) == 0 {
		return "", nil
	}
	dir, err := os.MkdirTemp("", "atlas-shims-")
	if err != nil {
		return "", err
	}
	for name, body := range shims {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil { //nolint:gosec // G306: an executable shim
			return "", err
		}
	}
	return dir, nil
}

func runShell(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	if os.Getenv("ATLAS_SHELL") == "1" {
		return errors.New("already inside an atlas shell")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Discover(ctx, cwd)
	if err != nil {
		return err
	}
	h, err := startHost(p.Root)
	if err != nil {
		return err
	}
	l, st := h.Listener, h.Store
	closed := false
	defer func() {
		if !closed {
			_, _ = h.Close()
		}
	}()

	adapters := ambient.For(p.Root, ambient.AtlasCommand())
	shimDir, err := writeShims(adapters, l.Port)
	if err != nil {
		return err
	}
	if shimDir != "" {
		defer os.RemoveAll(shimDir)
	}

	b, _ := st.Binding(ctx)
	fmt.Fprintf(out, "Atlas shell for %s\n", p.Root)
	fmt.Fprintf(out, "  capture: %s (Claude Code via the proxy on 127.0.0.1:%d)\n", onOff(b != nil && b.Enabled), l.Port)
	fmt.Fprintf(out, "  memory:  %s\n", onOff(project.MemorySharing(p.Root)))
	fmt.Fprintln(out, "Run claude as usual. Type exit to leave.")

	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	child := exec.CommandContext(context.Background(), sh, "-i") //nolint:gosec // G204: the user's own shell
	child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
	child.Env = shellEnv(l.Port, adapters, shimDir)
	child.Dir = cwd
	// The interactive shell handles ^C itself; this process waits it out.
	signal.Ignore(os.Interrupt, syscall.SIGQUIT)
	defer signal.Reset(os.Interrupt, syscall.SIGQUIT)
	if err := child.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return err
		}
	}
	closed = true
	if res, err := h.Close(); err == nil && res.Checkpoints > 0 {
		fmt.Fprintf(out, "Linked %d session commit(s).\n", res.Checkpoints)
	}
	fmt.Fprintln(out, "Left the Atlas shell.")
	return nil
}

// runCaptureLink walks new commits and links them to captured sessions.
func runCaptureLink(cmd *cobra.Command, _ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Discover(cmd.Context(), cwd)
	if err != nil {
		return err
	}
	lock, err := store.Lock(p.Root)
	if errors.Is(err, store.ErrLocked) {
		return errors.New(lockedHelp)
	}
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	st, err := store.Open(p.Root, false)
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := gitlink.Link(cmd.Context(), st, p.Root, time.Now)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Checked %d commit(s); %d checkpoint(s) written.\n", res.Commits, res.Checkpoints)
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func runEnv(cmd *cobra.Command, _ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Discover(cmd.Context(), cwd)
	if err != nil {
		return err
	}
	rf := listener.ReadRunFile(p.Root)
	if rf == nil || rf.Port == 0 {
		return errors.New("no capture listener is running for this Project; start one with atlas shell")
	}
	syntax, _ := cmd.Flags().GetString("shell")
	for _, kv := range shellEnv(rf.Port, ambient.For(p.Root, ambient.AtlasCommand()), "") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "ANTHROPIC_BASE_URL", "ATLAS_PROXY_PORT", "NO_PROXY", "no_proxy":
		default:
			continue
		}
		if syntax == "fish" {
			fmt.Fprintf(cmd.OutOrStdout(), "set -gx %s %s;\n", k, shQuote(v))
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "export %s=%s\n", k, shQuote(v))
		}
	}
	return nil
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// ── Hooks ────────────────────────────────────────────────────────────────────

// runHook is what Claude Code runs on each hook event. It never fails the
// agent: errors are swallowed and it exits 0.
func runHook(agent, event string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		raw, _ := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 4<<20))
		traceHook(agent, event, raw)
		var in struct {
			SessionID string `json:"session_id"`
			Cwd       string `json:"cwd"`
			Prompt    string `json:"prompt"`
		}
		_ = json.Unmarshal(raw, &in)
		cwd := in.Cwd
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		ctx, cancel := context.WithTimeout(cmd.Context(), 3*time.Second)
		defer cancel()
		p, perr := project.Discover(ctx, cwd)
		running := false
		if perr == nil && in.SessionID != "" {
			if rf := listener.ReadRunFile(p.Root); rf != nil {
				running = postHook(ctx, rf.Socket, listener.HookEvent{Agent: agent, Event: event,
					SessionID: in.SessionID, Cwd: cwd, Prompt: in.Prompt, PIDs: procs.Ancestors(os.Getpid(), 4)}) == nil
			}
		}
		if agent == "claude" && event == "session-start" {
			banner := map[string]any{"systemMessage": bannerText(ctx, p, running),
				"hookSpecificOutput": map[string]any{"hookEventName": "SessionStart"}}
			b, _ := json.Marshal(banner)
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
		}
		return nil
	}
}

func postHook(ctx context.Context, sock string, ev listener.HookEvent) error {
	body, _ := json.Marshal(ev)
	client := &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://atlas/hook", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("listener said %d", resp.StatusCode)
	}
	return nil
}

// bannerText is the one line Claude shows at session start:
// "Atlas: capture on · memory on · acme/backend". It is shown to the user
// only (systemMessage); nothing goes into the model's context (D6).
func bannerText(ctx context.Context, p *project.Project, running bool) string {
	if p == nil {
		return "Atlas: this folder is not an Atlas Project (atlas init)"
	}
	capture := "capture off"
	if b := bindingOf(ctx, p.Root); b != nil && b.Enabled {
		if running {
			capture = "capture on"
		} else {
			capture = "capture off (run claude inside atlas shell)"
		}
	}
	parts := []string{capture, "memory " + onOff(project.MemorySharing(p.Root))}
	if label := projectLabel(ctx, p.Root); label != "" {
		parts = append(parts, label)
	}
	return "Atlas: " + strings.Join(parts, " · ")
}

func bindingOf(ctx context.Context, root string) *store.Binding {
	st, err := store.Open(root, false)
	if err != nil {
		return nil
	}
	defer st.Close()
	b, _ := st.Binding(ctx)
	return b
}

// projectLabel is "<org name>/<project slug>" for a cloud binding, from the
// session file and the binding, without network calls.
func projectLabel(ctx context.Context, root string) string {
	b := bindingOf(ctx, root)
	if b == nil || b.Mode != store.ModeCloud {
		return ""
	}
	org := deref(b.OrgID)
	if st, err := auth.DefaultStore(); err == nil {
		if s, ok := st.Load(); ok && s.Identity != nil && s.Identity.Orgs != nil {
			for _, o := range *s.Identity.Orgs {
				if o.ID == org {
					org = o.Name
				}
			}
		}
	}
	slug := deref(b.Slug)
	if slug == "" {
		slug = deref(b.RemoteWorkspaceID)
	}
	return org + "/" + slug
}

// runStatusline prints Claude Code's status line: the user's own status line
// (when the CLI wrapped one) followed by Atlas's.
func runStatusline(cmd *cobra.Command, _ []string) error {
	raw, _ := io.ReadAll(io.LimitReader(cmd.InOrStdin(), 1<<20))
	var in struct {
		Cwd       string `json:"cwd"`
		Workspace struct {
			CurrentDir string `json:"current_dir"`
		} `json:"workspace"`
	}
	_ = json.Unmarshal(raw, &in)
	cwd := in.Workspace.CurrentDir
	if cwd == "" {
		cwd = in.Cwd
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
	defer cancel()
	line := "Atlas"
	if p, err := project.Discover(ctx, cwd); err == nil {
		parts := []string{"Atlas"}
		if label := projectLabel(ctx, p.Root); label != "" {
			parts = append(parts, label)
		}
		parts = append(parts, "memory "+onOff(project.MemorySharing(p.Root)))
		capture := false
		if b := bindingOf(ctx, p.Root); b != nil && b.Enabled {
			capture = listener.ReadRunFile(p.Root) != nil && os.Getenv("ATLAS_SHELL") == "1"
		}
		parts = append(parts, "capture "+onOff(capture))
		line = strings.Join(parts, " · ")
		if st, err := ambient.LoadState(p.Root); err == nil {
			if orig := ambient.WrappedStatusCommand(st); orig != "" {
				c := exec.CommandContext(ctx, "/bin/sh", "-c", orig) //nolint:gosec // G204: the user's own status line
				c.Stdin = bytes.NewReader(raw)
				if b, err := c.Output(); err == nil {
					if first := strings.TrimSpace(strings.SplitN(string(b), "\n", 2)[0]); first != "" {
						line = first + " | " + line
					}
				}
			}
		}
	}
	fmt.Fprintln(cmd.OutOrStdout(), line)
	return nil
}

// traceHook appends the hook's field names (never their values) to the file
// named by ATLAS_HOOK_TRACE, to record what each agent sends.
func traceHook(agent, event string, raw []byte) {
	path := os.Getenv("ATLAS_HOOK_TRACE")
	if path == "" {
		return
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	line, _ := json.Marshal(map[string]any{"agent": agent, "event": event, "fields": keys,
		"ppid": os.Getppid(), "at": time.Now().UTC().Format(time.RFC3339Nano)})
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: a path the user chose
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}
