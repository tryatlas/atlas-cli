package ambient

import (
	"path/filepath"
)

// State is .atlas/cli.json: what the CLI installed, so removal is exact
// (SPEC §4.3). Shape: {"version":1,"capture":{"proxy":true},
// "agents":{"claude-code":{"mcp":true,"hooks":true,"statusLine":true,…}}}.
type State struct {
	path string
	root *Node
}

// LoadState reads <dir>/.atlas/cli.json (missing → empty).
func LoadState(dir string) (*State, error) {
	p := filepath.Join(dir, ".atlas", "cli.json")
	n, err := LoadFile(p)
	if err != nil {
		return nil, err
	}
	if n.Get("version") == nil {
		n.Set("version", Val(1))
	}
	return &State{path: p, root: n}, nil
}

// Save writes the state.
func (s *State) Save() error { return SaveFile(s.path, s.root, false) }

func (s *State) agent(id string) *Node { return s.root.Child("agents").Child(id) }

// SetCaptureProxy records that the shell routes agents through the proxy.
func (s *State) SetCaptureProxy(on bool) { s.root.Child("capture").Set("proxy", Val(on)) }

// SetAgent sets a boolean flag for an agent.
func (s *State) SetAgent(id, key string, v bool) { s.agent(id).Set(key, Val(v)) }

// SetAgentString sets a string for an agent.
func (s *State) SetAgentString(id, key, v string) { s.agent(id).Set(key, Val(v)) }

// AgentFlag reads a boolean flag.
func (s *State) AgentFlag(id, key string) bool {
	n := s.root.Get("agents").Get(id).Get(key)
	return n != nil && n.Kind == 'v' && string(n.Raw) == "true"
}

// AgentString reads a string.
func (s *State) AgentString(id, key string) string {
	v, _ := s.root.Get("agents").Get(id).Get(key).String()
	return v
}

// DeleteAgentKeys removes keys, and the agent when nothing is left.
func (s *State) DeleteAgentKeys(id string, keys ...string) {
	agents := s.root.Get("agents")
	a := agents.Get(id)
	if a == nil {
		return
	}
	for _, k := range keys {
		a.Delete(k)
	}
	if len(a.Keys) == 0 {
		agents.Delete(id)
	}
}
