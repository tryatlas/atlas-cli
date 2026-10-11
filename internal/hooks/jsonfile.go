package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// JSON config files the integrations edit: read as raw maps so every key
// Atlas does not own survives byte for byte, written atomically.

// LoadJSONObject reads a JSON object file; a missing file is an empty map.
// A file holding something other than an object is an error, so a user's
// malformed config is never overwritten.
func LoadJSONObject(path string) (map[string]json.RawMessage, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the agent config the user asked to edit
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

// SaveJSON writes v as indented JSON (no HTML escaping) atomically with the
// given mode, creating the parent directory.
func SaveJSON(path string, v any, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return err
	}
	tmp := path + ".atlas-tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Raw marshals v to a RawMessage without HTML escaping (hook commands hold
// > and &).
func Raw(v any) json.RawMessage {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// CommandHook is a Claude-shaped hook entry: {"type":"command","command":…},
// plus a timeout when the agent wants one.
type CommandHook struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// Matcher is a Claude-shaped hook group: an optional matcher and its hooks.
type Matcher struct {
	Matcher *string       `json:"matcher,omitempty"`
	Hooks   []CommandHook `json:"hooks"`
}

// ClaudeShaped edits the Claude-style hooks object shared by Claude Code,
// Codex and Factory Droid: hooks.<Type> = [ {matcher, hooks:[…]} ].
type ClaudeShaped struct {
	Path  string
	Types []ClaudeHook
	// EmptyMatcher is what a hook without a matcher carries: "" (Claude,
	// Droid) or null (Codex).
	NullMatcher bool
	Mode        fs.FileMode
}

// ClaudeHook is one hook Atlas installs.
type ClaudeHook struct {
	Type    string // SessionStart, Stop, …
	Matcher string // "" for none
	Command string // the wrapped command
	Timeout int
}

// Install merges Atlas's hooks into the file, pruning Atlas entries whose
// command is no longer wanted. It reports whether the file changed.
func (c ClaudeShaped) Install(force bool) (bool, error) {
	root, err := LoadJSONObject(c.Path)
	if err != nil {
		return false, err
	}
	hooks := map[string]json.RawMessage{}
	if raw, ok := root["hooks"]; ok && len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &hooks); err != nil {
			return false, fmt.Errorf("%s: hooks is not an object: %w", c.Path, err)
		}
	}
	wanted := map[string]bool{}
	for _, h := range c.Types {
		wanted[h.Command] = true
	}
	changed := false
	byType := map[string][]ClaudeHook{}
	for _, h := range c.Types {
		byType[h.Type] = append(byType[h.Type], h)
	}
	// Prune stale Atlas entries everywhere, including hook types Atlas no
	// longer installs.
	for typ, raw := range hooks {
		var groups []Matcher
		if err := json.Unmarshal(raw, &groups); err != nil {
			continue // not our shape: leave the user's value alone
		}
		out, pruned := pruneAtlas(groups, wanted, force)
		if pruned {
			changed = true
			if len(out) == 0 {
				delete(hooks, typ)
			} else {
				hooks[typ] = Raw(out)
			}
		}
	}
	for typ, list := range byType {
		var groups []Matcher
		if raw, ok := hooks[typ]; ok {
			if err := json.Unmarshal(raw, &groups); err != nil {
				return false, fmt.Errorf("%s: hooks.%s is not a list: %w", c.Path, typ, err)
			}
		}
		for _, h := range list {
			if hasCommand(groups, h.Matcher, h.Command) {
				continue
			}
			groups = addCommand(groups, h, c.NullMatcher)
			changed = true
		}
		hooks[typ] = Raw(groups)
	}
	if !changed {
		return false, nil
	}
	root["hooks"] = Raw(hooks)
	mode := c.Mode
	if mode == 0 {
		mode = 0o600
	}
	return true, SaveJSON(c.Path, root, mode)
}

// Uninstall removes every Atlas entry.
func (c ClaudeShaped) Uninstall() error {
	root, err := LoadJSONObject(c.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	raw, ok := root["hooks"]
	if !ok {
		return nil
	}
	hooks := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &hooks); err != nil {
		return nil //nolint:nilerr // not our shape: nothing of ours can be in it
	}
	changed := false
	for typ, raw := range hooks {
		var groups []Matcher
		if json.Unmarshal(raw, &groups) != nil {
			continue
		}
		out, pruned := pruneAtlas(groups, nil, true)
		if !pruned {
			continue
		}
		changed = true
		if len(out) == 0 {
			delete(hooks, typ)
		} else {
			hooks[typ] = Raw(out)
		}
	}
	if !changed {
		return nil
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = Raw(hooks)
	}
	mode := c.Mode
	if mode == 0 {
		mode = 0o600
	}
	return SaveJSON(c.Path, root, mode)
}

// State compares the file with what Install would write.
func (c ClaudeShaped) State() InstallState {
	root, err := LoadJSONObject(c.Path)
	if err != nil {
		return Absent
	}
	raw, ok := root["hooks"]
	if !ok {
		return Absent
	}
	hooks := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &hooks) != nil {
		return Absent
	}
	some, all := false, true
	for _, h := range c.Types {
		var groups []Matcher
		_ = json.Unmarshal(hooks[h.Type], &groups)
		if hasCommand(groups, h.Matcher, h.Command) {
			some = true
		} else {
			all = false
		}
	}
	switch {
	case all:
		return Current
	case some:
		return Outdated
	}
	// An Atlas entry with an old command counts as outdated.
	for _, raw := range hooks {
		var groups []Matcher
		if json.Unmarshal(raw, &groups) != nil {
			continue
		}
		for _, g := range groups {
			for _, h := range g.Hooks {
				if Owned(h.Command) {
					return Outdated
				}
			}
		}
	}
	return Absent
}

func matcherOf(g Matcher) string {
	if g.Matcher == nil {
		return ""
	}
	return *g.Matcher
}

func hasCommand(groups []Matcher, matcher, command string) bool {
	for _, g := range groups {
		if matcherOf(g) != matcher {
			continue
		}
		for _, h := range g.Hooks {
			if h.Command == command {
				return true
			}
		}
	}
	return false
}

func addCommand(groups []Matcher, h ClaudeHook, nullMatcher bool) []Matcher {
	entry := CommandHook{Type: "command", Command: h.Command, Timeout: h.Timeout}
	for i, g := range groups {
		if matcherOf(g) == h.Matcher {
			groups[i].Hooks = append(groups[i].Hooks, entry)
			return groups
		}
	}
	var m *string
	if h.Matcher != "" || !nullMatcher {
		s := h.Matcher
		m = &s
	}
	return append(groups, Matcher{Matcher: m, Hooks: []CommandHook{entry}})
}

// pruneAtlas drops Atlas entries not in wanted (every Atlas entry when all
// is set) and groups left empty.
func pruneAtlas(groups []Matcher, wanted map[string]bool, all bool) ([]Matcher, bool) {
	pruned := false
	var out []Matcher
	for _, g := range groups {
		var keep []CommandHook
		for _, h := range g.Hooks {
			if Owned(h.Command) && (all || !wanted[h.Command]) {
				pruned = true
				continue
			}
			keep = append(keep, h)
		}
		if len(keep) == 0 && len(g.Hooks) > 0 {
			pruned = true
			continue
		}
		g.Hooks = keep
		out = append(out, g)
	}
	return out, pruned
}
