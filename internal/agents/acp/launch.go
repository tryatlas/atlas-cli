package acp

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Command is a resolved launch: path, args, and the env overlaid on the
// inherited environment.
type Command struct {
	Path string
	Args []string
	Env  map[string]string
}

// quirk is the desktop's per-id launch workaround (evidence-acp.md §2.4).
type quirk struct {
	Env     map[string]string // set (an empty value sets a blank, not unset)
	PassEnv []string          // copied from the host env when set
	Args    []string          // appended after the distribution args
}

// launchQuirks is the only id-keyed table in the CLI, copied from the
// desktop as data. It is explicitly not a list of supported agents. The
// memory rows are the desktop's default-on "Atlas-only memory" setting.
var launchQuirks = map[string]quirk{
	"claude-code": {Env: map[string]string{"ANTHROPIC_API_KEY": "", "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1"}},
	"codex": {PassEnv: []string{"CODEX_API_KEY", "OPENAI_API_KEY"},
		Args: []string{"-c", "memories.use_memories=false", "-c", "memories.generate_memories=false"}},
	"gemini": {Env: map[string]string{"SURFACE": "atlas"}},
}

// extra is the quirks and proxy layer for an id.
func extra(id string, host func(string) (string, bool)) (map[string]string, []string) {
	env := map[string]string{}
	q := launchQuirks[id]
	for k, v := range q.Env {
		env[k] = v
	}
	for _, k := range q.PassEnv {
		if v, ok := host(k); ok {
			env[k] = v
		}
	}
	for k, v := range proxyEnv(host) {
		env[k] = v
	}
	return env, q.Args
}

// proxyEnv re-exports the first proxy variable canonically and keeps
// loopback off the proxy (evidence-acp.md §2.3).
func proxyEnv(host func(string) (string, bool)) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		v, ok := host(k)
		if !ok || v == "" {
			continue
		}
		if strings.EqualFold(k, "HTTPS_PROXY") {
			out["HTTPS_PROXY"] = v
		} else {
			out["HTTP_PROXY"] = v
		}
		if np, ok := host("NO_PROXY"); ok {
			out["NO_PROXY"] = np
		} else if np, ok := host("no_proxy"); ok {
			out["NO_PROXY"] = np
		} else {
			out["NO_PROXY"] = "localhost,127.0.0.1"
		}
		break
	}
	return out
}

// layer merges env maps, later wins.
func layer(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// hostLookup reads the process env (CLAUDECODE is never passed on).
func hostLookup(k string) (string, bool) {
	if k == "CLAUDECODE" {
		return "", false
	}
	return os.LookupEnv(k)
}

// childEnv is the full environment: the process env without CLAUDECODE,
// then the command's overlay.
func childEnv(overlay map[string]string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDECODE" {
			continue
		}
		if _, ok := overlay[k]; ok {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+overlay[k])
	}
	return out
}

// Process is a running agent in its own process group.
type Process struct {
	cmd    *exec.Cmd
	Stdin  io.WriteCloser
	Stdout io.ReadCloser

	mu       sync.Mutex
	stderr   []string // ring of the last lines
	exited   chan struct{}
	exitErr  error
	drained  chan struct{}
	killOnce sync.Once
}

const stderrRing = 200

// Spawn starts the command with piped stdio in a new session (setsid).
func Spawn(c Command, dir string) (*Process, error) {
	cmd := exec.Command(c.Path, c.Args...) //nolint:gosec // G204: a resolved agent command
	cmd.Env = childEnv(c.Env)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Process{cmd: cmd, Stdin: stdin, Stdout: stdout, exited: make(chan struct{}), drained: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			p.mu.Lock()
			p.stderr = append(p.stderr, sc.Text())
			if len(p.stderr) > stderrRing {
				p.stderr = p.stderr[len(p.stderr)-stderrRing:]
			}
			p.mu.Unlock()
		}
		close(p.drained)
	}()
	go func() {
		err := cmd.Wait()
		p.mu.Lock()
		p.exitErr = err
		p.mu.Unlock()
		close(p.exited)
	}()
	return p, nil
}

// Exited closes when the process ends.
func (p *Process) Exited() <-chan struct{} { return p.exited }

// Stderr is the tail of stderr, after waiting up to 1 s for it to drain.
func (p *Process) Stderr() string {
	select {
	case <-p.drained:
	case <-time.After(stderrDrainGrace):
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	lines := p.stderr
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}

// ExitError describes how the process ended.
func (p *Process) ExitError() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitErr
}

// Kill ends the whole process group (codex-acp is node → codex.js →
// codex app-server; killing only node orphans the rest).
func (p *Process) Kill() {
	p.killOnce.Do(func() {
		_ = p.Stdin.Close()
		if p.cmd.Process != nil {
			_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
			_ = p.cmd.Process.Kill()
		}
	})
}

// ExitedError is an agent that died during connect: its exit status and
// the stderr that explains it.
type ExitedError struct {
	Status string
	Stderr string
}

func (e *ExitedError) Error() string {
	msg := "the agent exited (" + e.Status + ")"
	if e.Stderr != "" {
		msg += ":\n" + e.Stderr
	}
	return msg
}

func (p *Process) exitedError() *ExitedError {
	status := "exit status 0"
	if err := p.ExitError(); err != nil {
		status = err.Error()
	}
	return &ExitedError{Status: status, Stderr: p.Stderr()}
}

// waitExit waits up to d for the process to end.
func (p *Process) waitExit(ctx context.Context, d time.Duration) bool {
	select {
	case <-p.exited:
		return true
	case <-time.After(d):
		return false
	case <-ctx.Done():
		return false
	}
}

var errNoCommand = errors.New("no command")

func (c Command) String() string {
	if c.Path == "" {
		return errNoCommand.Error()
	}
	return fmt.Sprintf("%s %s", c.Path, strings.Join(c.Args, " "))
}
