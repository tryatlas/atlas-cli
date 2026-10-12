// Package mcpconfig writes the atlas_memory MCP server into each agent's own
// MCP configuration so a session run in the agent's UI reaches shared memory
// (ADR-0013 §6, SPEC §7.3). The entry is a stdio bridge with no secret:
// atlas-cli mcp-bridge --project <root> --agent <id>.
package mcpconfig

import (
	"encoding/json"
	"sort"
	"sync"

	"github.com/tryatlas/cli/internal/hooks"
)

// ServerName is the entry's key: the desktop's server name.
const ServerName = "atlas_memory"

// Args are the bridge arguments for a Project and agent.
func Args(root, agentID string) []string {
	return []string{"mcp-bridge", "--project", root, "--agent", agentID}
}

// Writer edits one agent's MCP config.
type Writer interface {
	// Path is the file written under root.
	Path(root string) string
	// Write adds or refreshes the entry; it reports whether the file changed.
	Write(root string) (bool, error)
	// Remove deletes the entry.
	Remove(root string) error
	// State reports the entry's presence and currency.
	State(root string) hooks.InstallState
}

var (
	mu      sync.RWMutex
	writers = map[string]Writer{}
)

// Register adds a writer for an agent id.
func Register(agentID string, w Writer) {
	mu.Lock()
	defer mu.Unlock()
	writers[agentID] = w
}

// For returns the writer for an agent, if any.
func For(agentID string) (Writer, bool) {
	mu.RLock()
	defer mu.RUnlock()
	w, ok := writers[agentID]
	return w, ok
}

// IDs lists the agents with a writer.
func IDs() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(writers))
	for id := range writers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// StdioEntry is the common JSON shape {"command":"atlas-cli","args":[…]} with
// an optional "type":"stdio".
func StdioEntry(root, agentID string, withType bool) json.RawMessage {
	m := map[string]any{"command": hooks.Bin, "args": Args(root, agentID)}
	if withType {
		m["type"] = "stdio"
	}
	return hooks.Raw(m)
}

// JSONServers edits a JSON file whose MCP servers live under one key
// (mcpServers for Claude Code, Cursor and Copilot; mcp for OpenCode).
type JSONServers struct {
	File     string // path under root
	Key      string // the object holding servers
	AgentID  string
	WithType bool
	// Entry overrides the default stdio entry when set.
	Entry func(root string) json.RawMessage
}

// Path is the config file under root.
func (j JSONServers) Path(root string) string { return root + "/" + j.File }

func (j JSONServers) entry(root string) json.RawMessage {
	if j.Entry != nil {
		return j.Entry(root)
	}
	return StdioEntry(root, j.AgentID, j.WithType)
}

// Write adds or refreshes the atlas_memory entry.
func (j JSONServers) Write(root string) (bool, error) {
	obj, err := hooks.LoadJSONObject(j.Path(root))
	if err != nil {
		return false, err
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := obj[j.Key]; ok && len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return false, err
		}
	}
	want := j.entry(root)
	if cur, ok := servers[ServerName]; ok && jsonEqual(cur, want) {
		return false, nil
	}
	servers[ServerName] = want
	obj[j.Key] = hooks.Raw(servers)
	return true, hooks.SaveJSON(j.Path(root), obj, 0o644)
}

// Remove deletes the atlas_memory entry.
func (j JSONServers) Remove(root string) error {
	obj, err := hooks.LoadJSONObject(j.Path(root))
	if err != nil {
		return err
	}
	raw, ok := obj[j.Key]
	if !ok {
		return nil
	}
	servers := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &servers) != nil {
		return nil //nolint:nilerr // not our shape: nothing of ours in it
	}
	if _, ok := servers[ServerName]; !ok {
		return nil
	}
	delete(servers, ServerName)
	if len(servers) == 0 {
		delete(obj, j.Key)
	} else {
		obj[j.Key] = hooks.Raw(servers)
	}
	return hooks.SaveJSON(j.Path(root), obj, 0o644)
}

// State compares the entry with what Write would put there.
func (j JSONServers) State(root string) hooks.InstallState {
	obj, err := hooks.LoadJSONObject(j.Path(root))
	if err != nil {
		return hooks.Absent
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := obj[j.Key]; ok {
		_ = json.Unmarshal(raw, &servers)
	}
	cur, ok := servers[ServerName]
	switch {
	case !ok:
		return hooks.Absent
	case jsonEqual(cur, j.entry(root)):
		return hooks.Current
	}
	return hooks.Outdated
}

func jsonEqual(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}
