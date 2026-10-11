package run

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tryatlas/atlas-cli/internal/agents/acp"
)

// CheckResult is one agent's handshake check.
type CheckResult struct {
	ID, Version, Kind string
	Agent             string // agentInfo name and version
	Protocol          int
	Caps              []string
	Auth              []string // "id (kind)"
	Session           string   // "ok", "sign-in required", or the error
	Err               error    // install, launch or initialize failure
	Took              time.Duration
}

// checkHost answers nothing: a check opens no prompt.
type checkHost struct{}

func (checkHost) Update(string, acp.Update) {}
func (checkHost) Permission(context.Context, acp.PermissionRequest) (string, bool) {
	return "", false
}
func (checkHost) Elicit(context.Context, acp.Elicitation) acp.ElicitationAnswer {
	return acp.ElicitationAnswer{Action: "cancel"}
}
func (checkHost) ElicitationComplete(string) {}

// Check installs (as a registry entry, without touching the installed map),
// launches, initializes, and tries session/new in an empty directory.
func (a Agents) Check(ctx context.Context, e acp.Entry, limit time.Duration) (r CheckResult) {
	start := time.Now()
	r = CheckResult{ID: e.ID, Version: e.Version}
	switch e.KindOn(acp.PlatformKey()) {
	case acp.KindBinary:
		r.Kind = "binary"
	case acp.KindNpx:
		r.Kind = "npx"
	default:
		r.Err = fmt.Errorf("no build for %s", acp.PlatformKey())
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	defer func() { r.Took = time.Since(start).Round(time.Second) }()
	res, err := acp.NewResolver(a.Progress)
	if err != nil {
		r.Err = err
		return r
	}
	reg := &acp.Registry{Agents: []acp.Entry{e}}
	cmd, err := res.Resolve(ctx, e.ID, &acp.Installed{Type: acp.TypeRegistry}, reg)
	if err != nil {
		r.Err = fmt.Errorf("install: %w", err)
		return r
	}
	dir, err := os.MkdirTemp("", "atlas-check-")
	if err != nil {
		r.Err = err
		return r
	}
	defer os.RemoveAll(dir)
	ag, err := acp.Connect(ctx, e.ID, cmd, dir, a.Version, checkHost{})
	if err != nil {
		r.Err = fmt.Errorf("launch: %w", err)
		return r
	}
	defer ag.Close()
	r.Protocol = ag.Init.ProtocolVersion
	if i := ag.Init.AgentInfo; i != nil {
		r.Agent = strings.TrimSpace(i.Name + " " + i.Version)
	}
	c := ag.Init.AgentCapabilities
	for _, x := range []struct {
		on   bool
		name string
	}{{c.LoadSession, "load"}, {c.CanResume(), "resume"}, {c.CanList(), "list"}, {c.CanClose(), "close"},
		{c.MCPCapabilities.HTTP, "mcp-http"}, {c.PromptCapabilities.Image, "image"}, {c.CanLogout(), "logout"},
		{c.ExtraDirs(), "extra-dirs"}} {
		if x.on {
			r.Caps = append(r.Caps, x.name)
		}
	}
	for _, m := range ag.Init.AuthMethods {
		k := m.Kind()
		if m.Meta.TerminalAuth != nil {
			k = "terminal-auth"
		}
		r.Auth = append(r.Auth, m.ID+" ("+k+")")
	}
	_, err = ag.NewSession(ctx, acp.SessionParams{Cwd: dir})
	switch {
	case err == nil:
		r.Session = "ok"
	case acp.IsAuthRequired(err):
		r.Session = "sign-in required"
	default:
		r.Session = "error: " + firstLine(err.Error())
	}
	return r
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
