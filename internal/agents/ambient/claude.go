package ambient

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var _ Adapter = Claude{}

// ClaudeID is the plugin id the CLI records Claude Code sessions under (SPEC §5.1).
const ClaudeID = "claude-code"

// ServerName is the MCP entry name agents see (D10): tool calls render as
// "atlas - memory_search".
const ServerName = "atlas"

// claudeHooks are the events the adapter installs and the hook subcommand
// each runs (SPEC §7.4).
var claudeHooks = []struct{ event, sub string }{
	{"SessionStart", "session-start"},
	{"UserPromptSubmit", "user-prompt-submit"},
	{"Stop", "stop"},
	{"SessionEnd", "session-end"},
}

// Claude is the Claude Code adapter for one Project (ADR-0000).
type Claude struct {
	Dir   string // the Project root (launch dir)
	Atlas string // how agents invoke this binary
}

// AtlasCommand is "atlas" when that name on PATH is this binary, else the
// absolute path of this binary.
func AtlasCommand() string {
	exe, err := os.Executable()
	if err != nil {
		return "atlas"
	}
	exe, _ = filepath.EvalSymlinks(exe)
	if p, err := exec.LookPath("atlas"); err == nil {
		if r, err := filepath.EvalSymlinks(p); err == nil && r == exe {
			return "atlas"
		}
	}
	return exe
}

// ID is the plugin id sessions are recorded under.
func (c Claude) ID() string { return ClaudeID }

// Detect reports whether the claude command is installed, and its version.
func (c Claude) Detect(ctx context.Context) (bool, string) { return version(ctx, "claude") }

// Configured reports a Project that already uses Claude Code.
func (c Claude) Configured() bool {
	for _, p := range []string{filepath.Join(c.Dir, ".claude"), c.mcpPath()} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// Dialect is Anthropic Messages.
func (c Claude) Dialect() Dialect { return Anthropic }

// Shell points Claude Code at the proxy (SPEC §7.2). The user's previous
// ANTHROPIC_BASE_URL is the proxy's upstream; the CLI never sets a blank
// ANTHROPIC_API_KEY (that is a desktop quirk for ACP).
func (c Claude) Shell(l Listener) (ShellSetup, error) {
	return ShellSetup{Env: map[string]string{
		"ANTHROPIC_BASE_URL": fmt.Sprintf("http://127.0.0.1:%d/proxy/anthropic", l.Port),
	}}, nil
}

func (c Claude) mcpPath() string      { return filepath.Join(c.Dir, ".mcp.json") }
func (c Claude) settingsPath() string { return filepath.Join(c.Dir, ".claude", "settings.json") }
func (c Claude) localSettingsPath() string {
	return filepath.Join(c.Dir, ".claude", "settings.local.json")
}

// ── MCP (memory) ─────────────────────────────────────────────────────────────

// InstallMCP writes the stdio bridge entry into .mcp.json (no token: ADR-0003)
// and approves it in .claude/settings.local.json so Claude does not ask.
func (c Claude) InstallMCP(st *State) error {
	mcp, err := LoadFile(c.mcpPath())
	if err != nil {
		return err
	}
	entry := Obj()
	entry.Set("type", Val("stdio"))
	entry.Set("command", Val(c.Atlas))
	entry.Set("args", Val([]string{"mcp-bridge", "--project", c.Dir, "--agent", ClaudeID}))
	mcp.Child("mcpServers").Set(ServerName, entry)
	if err := SaveFile(c.mcpPath(), mcp, false); err != nil {
		return err
	}
	local, err := LoadFile(c.localSettingsPath())
	if err != nil {
		return err
	}
	if addToStringArray(local, "enabledMcpjsonServers", ServerName) {
		st.SetAgent(ClaudeID, "approvedMcp", true)
	}
	if err := SaveFile(c.localSettingsPath(), local, false); err != nil {
		return err
	}
	st.SetAgent(ClaudeID, "mcp", true)
	return nil
}

// RemoveMCP takes out exactly what InstallMCP added.
func (c Claude) RemoveMCP(st *State) error {
	mcp, err := LoadFile(c.mcpPath())
	if err != nil {
		return err
	}
	if servers := mcp.Get("mcpServers"); servers != nil && isOurBridge(servers.Get(ServerName)) {
		servers.Delete(ServerName)
		if len(servers.Keys) == 0 {
			mcp.Delete("mcpServers")
		}
		if err := SaveFile(c.mcpPath(), mcp, true); err != nil {
			return err
		}
	}
	if st.AgentFlag(ClaudeID, "approvedMcp") {
		local, err := LoadFile(c.localSettingsPath())
		if err != nil {
			return err
		}
		removeFromStringArray(local, "enabledMcpjsonServers", ServerName)
		if err := SaveFile(c.localSettingsPath(), local, true); err != nil {
			return err
		}
	}
	st.DeleteAgentKeys(ClaudeID, "mcp", "approvedMcp")
	return nil
}

func isOurBridge(n *Node) bool {
	if n == nil {
		return false
	}
	args := n.Get("args")
	if args == nil || args.Kind != 'a' || len(args.Items) == 0 {
		return false
	}
	first, _ := args.Items[0].String()
	return first == "mcp-bridge"
}

// ── Hooks (capture identity) ─────────────────────────────────────────────────

func (c Claude) hookCommand(sub string) string {
	return quote(c.Atlas) + " hook claude " + sub
}

func isOurHook(cmd string) bool {
	for _, h := range claudeHooks {
		if strings.HasSuffix(cmd, " hook claude "+h.sub) {
			return true
		}
	}
	return false
}

// InstallHooks adds one command hook per event to .claude/settings.json,
// leaving the user's own hooks alone.
func (c Claude) InstallHooks(st *State) error {
	s, err := LoadFile(c.settingsPath())
	if err != nil {
		return err
	}
	hooks := s.Child("hooks")
	for _, h := range claudeHooks {
		list := hooks.Get(h.event)
		if list == nil || list.Kind != 'a' {
			list = Arr()
			hooks.Set(h.event, list)
		}
		if hasOurHook(list) {
			continue
		}
		cmd := Obj()
		cmd.Set("type", Val("command"))
		cmd.Set("command", Val(c.hookCommand(h.sub)))
		cmd.Set("timeout", Val(10))
		group := Obj()
		inner := Arr()
		inner.Items = append(inner.Items, cmd)
		group.Set("hooks", inner)
		list.Items = append(list.Items, group)
	}
	if err := SaveFile(c.settingsPath(), s, false); err != nil {
		return err
	}
	st.SetAgent(ClaudeID, "hooks", true)
	return nil
}

func hasOurHook(list *Node) bool {
	for _, g := range list.Items {
		if inner := g.Get("hooks"); inner != nil {
			for _, h := range inner.Items {
				if cmd, _ := h.Get("command").String(); isOurHook(cmd) {
					return true
				}
			}
		}
	}
	return false
}

// RemoveHooks deletes the CLI's hook commands and any group or event list
// they leave empty.
func (c Claude) RemoveHooks(st *State) error {
	s, err := LoadFile(c.settingsPath())
	if err != nil {
		return err
	}
	if hooks := s.Get("hooks"); hooks != nil && hooks.Kind == 'o' {
		for _, ev := range append([]string(nil), hooks.Keys...) {
			list := hooks.Get(ev)
			if list == nil || list.Kind != 'a' {
				continue
			}
			var keep []*Node
			for _, g := range list.Items {
				inner := g.Get("hooks")
				if inner == nil || inner.Kind != 'a' {
					keep = append(keep, g)
					continue
				}
				var rest []*Node
				for _, h := range inner.Items {
					if cmd, _ := h.Get("command").String(); !isOurHook(cmd) {
						rest = append(rest, h)
					}
				}
				if len(rest) > 0 {
					inner.Items = rest
					keep = append(keep, g)
				}
			}
			if len(keep) == 0 {
				hooks.Delete(ev)
			} else {
				list.Items = keep
			}
		}
		if len(hooks.Keys) == 0 {
			s.Delete("hooks")
		}
	}
	if err := SaveFile(c.settingsPath(), s, true); err != nil {
		return err
	}
	st.DeleteAgentKeys(ClaudeID, "hooks")
	return nil
}

// ── Status line (visibility) ─────────────────────────────────────────────────

func (c Claude) statusCommand() string { return quote(c.Atlas) + " statusline claude" }

// InstallStatus sets the status line to the CLI's; an existing one is
// remembered and wrapped (the CLI runs it and appends).
func (c Claude) InstallStatus(st *State) error {
	s, err := LoadFile(c.settingsPath())
	if err != nil {
		return err
	}
	if cur := s.Get("statusLine"); cur != nil {
		if cmd, _ := cur.Get("command").String(); cmd != "" && !strings.HasSuffix(cmd, " statusline claude") {
			st.SetAgentString(ClaudeID, "wrappedStatusLine", string(cur.Marshal()))
		}
	}
	line := Obj()
	line.Set("type", Val("command"))
	line.Set("command", Val(c.statusCommand()))
	s.Set("statusLine", line)
	if err := SaveFile(c.settingsPath(), s, false); err != nil {
		return err
	}
	st.SetAgent(ClaudeID, "statusLine", true)
	return nil
}

// RemoveStatus restores the wrapped status line, or removes the CLI's.
func (c Claude) RemoveStatus(st *State) error {
	s, err := LoadFile(c.settingsPath())
	if err != nil {
		return err
	}
	if cur := s.Get("statusLine"); cur != nil {
		if cmd, _ := cur.Get("command").String(); strings.HasSuffix(cmd, " statusline claude") {
			if raw := st.AgentString(ClaudeID, "wrappedStatusLine"); raw != "" {
				if orig, err := Parse([]byte(raw)); err == nil {
					s.Set("statusLine", orig)
				}
			} else {
				s.Delete("statusLine")
			}
		}
	}
	if err := SaveFile(c.settingsPath(), s, true); err != nil {
		return err
	}
	st.DeleteAgentKeys(ClaudeID, "statusLine", "wrappedStatusLine")
	return nil
}

// WrappedStatusCommand is the user's original status line command, if any.
func WrappedStatusCommand(st *State) string {
	raw := st.AgentString(ClaudeID, "wrappedStatusLine")
	if raw == "" {
		return ""
	}
	n, err := Parse([]byte(raw))
	if err != nil {
		return ""
	}
	cmd, _ := n.Get("command").String()
	return cmd
}

// ── helpers ──────────────────────────────────────────────────────────────────

// addToStringArray appends v to obj[key] when missing; true when it added.
func addToStringArray(obj *Node, key, v string) bool {
	arr := obj.Get(key)
	if arr == nil || arr.Kind != 'a' {
		arr = Arr()
		obj.Set(key, arr)
	}
	for _, it := range arr.Items {
		if s, _ := it.String(); s == v {
			return false
		}
	}
	arr.Items = append(arr.Items, Val(v))
	return true
}

func removeFromStringArray(obj *Node, key, v string) {
	arr := obj.Get(key)
	if arr == nil || arr.Kind != 'a' {
		return
	}
	var keep []*Node
	for _, it := range arr.Items {
		if s, _ := it.String(); s != v {
			keep = append(keep, it)
		}
	}
	if len(keep) == 0 {
		obj.Delete(key)
	} else {
		arr.Items = keep
	}
}
