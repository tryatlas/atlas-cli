package run

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"

	"github.com/tryatlas/atlas-cli/internal/agents/acp"
)

// Agents is the registry plus the installed map.
type Agents struct {
	Version  string
	Progress acp.Progress
}

// Catalog loads the registry cache-first.
func (a Agents) Catalog(ctx context.Context, force bool) (*acp.Registry, error) {
	c, err := acp.NewCatalog(a.Version)
	if err != nil {
		return nil, err
	}
	return c.Load(ctx, force)
}

// Installed reads the installed map and its path.
func (a Agents) Installed() (acp.InstalledMap, string, error) {
	p, err := acp.InstalledPath()
	if err != nil {
		return nil, "", err
	}
	return acp.LoadInstalled(p), p, nil
}

// Ensure returns an agent's installed entry, writing a registry entry on
// first use for an id the registry lists (install = write the entry).
func (a Agents) Ensure(ctx context.Context, id string) (*acp.Installed, *acp.Registry, error) {
	m, path, err := a.Installed()
	if err != nil {
		return nil, nil, err
	}
	inst := m[id]
	var reg *acp.Registry
	if inst == nil || inst.Type == acp.TypeRegistry {
		r, rerr := a.Catalog(ctx, false)
		if r == nil {
			return nil, nil, rerr
		}
		reg = r
		if _, ok := r.Find(id); !ok {
			if r2, err := a.Catalog(ctx, true); err == nil && r2 != nil {
				reg = r2 // an id not in the cache refreshes it once
			}
		}
	}
	if inst == nil {
		e, ok := reg.Find(id)
		if !ok {
			return nil, nil, fmt.Errorf("%s: %w (atlas agents list)", id, acp.ErrNotInRegistry)
		}
		if !e.Supported() {
			return nil, nil, fmt.Errorf("%s does not run on %s", id, acp.PlatformKey())
		}
		inst = &acp.Installed{Type: acp.TypeRegistry}
		m[id] = inst
		if err := m.Save(path); err != nil {
			return nil, nil, err
		}
	}
	return inst, reg, nil
}

// Launch resolves (installing on first connect) and connects, the whole
// within the 20 minute connect deadline.
func (a Agents) Launch(ctx context.Context, id, dir string, host acp.Host) (*acp.Agent, *acp.Installed, error) {
	ctx, cancel := context.WithTimeout(ctx, acp.ConnectDeadline)
	defer cancel()
	inst, reg, err := a.Ensure(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	res, err := acp.NewResolver(a.Progress)
	if err != nil {
		return nil, nil, err
	}
	cmd, err := res.Resolve(ctx, id, inst, reg)
	if err != nil {
		return nil, nil, err
	}
	ag, err := acp.Connect(ctx, id, cmd, dir, a.Version, host)
	if err != nil {
		return nil, nil, err
	}
	return ag, inst, nil
}

// LoginCommand builds a terminal sign-in: the typed method runs the agent's
// own binary with its launch args plus the method's args and env; a
// _meta["terminal-auth"] spec runs exactly as given. Anything else has no
// command: Atlas never guesses one.
func LoginCommand(launch acp.Command, m acp.AuthMethod) (acp.Command, bool) {
	if m.Type == "terminal" {
		env := map[string]string{}
		for k, v := range launch.Env {
			env[k] = v
		}
		for k, v := range m.Env {
			env[k] = v
		}
		return acp.Command{Path: launch.Path, Args: append(append([]string{}, launch.Args...), m.Args...), Env: env}, true
	}
	if t := m.Meta.TerminalAuth; t != nil && t.Command != "" {
		return acp.Command{Path: t.Command, Args: t.Args, Env: t.Env}, true
	}
	return acp.Command{}, false
}

var httpsURL = regexp.MustCompile(`https://[^\s"'<>]+`)

// FirstURL is the first https URL on a line.
func FirstURL(line string) string { return httpsURL.FindString(line) }

// Login runs a terminal sign-in. On a terminal (tty) it gets the user's
// terminal; otherwise stdin is closed and output is streamed line by line
// to out with the first https URL reported through onURL.
func Login(ctx context.Context, cmd acp.Command, tty bool, out io.Writer, onURL func(string)) error {
	c := exec.CommandContext(ctx, cmd.Path, cmd.Args...) //nolint:gosec // G204: the agent's advertised login command
	env := os.Environ()
	keys := make([]string, 0, len(cmd.Env))
	for k := range cmd.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env = append(env, k+"="+cmd.Env[k])
	}
	c.Env = env
	if tty {
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
		return c.Run()
	}
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	stdout, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := c.StderrPipe()
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return err
	}
	var once sync.Once
	var wg sync.WaitGroup
	stream := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			line := sc.Text()
			fmt.Fprintln(out, line)
			if u := FirstURL(line); u != "" && onURL != nil {
				once.Do(func() { onURL(u) })
			}
		}
	}
	wg.Add(2)
	go stream(stdout)
	go stream(stderr)
	wg.Wait()
	if err := c.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("the sign-in command failed (exit %d)", ee.ExitCode())
		}
		return err
	}
	return nil
}

// MethodSummary is one line per auth method, for listings.
func MethodSummary(ms []acp.AuthMethod) string {
	var parts []string
	for _, m := range ms {
		parts = append(parts, m.ID+" ("+m.Kind()+")")
	}
	return strings.Join(parts, ", ")
}
