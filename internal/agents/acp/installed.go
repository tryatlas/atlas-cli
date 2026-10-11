package acp

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The installed map, <data dir>/external-agents/installed.json: the only
// source of runnable agents (evidence-acp.md §1.3). Zed's agent_servers
// shape, written by the registry commands only.

// Installed entry types; "extension" is read as "registry".
const (
	TypeRegistry = "registry"
	TypeCustom   = "custom"
)

// knownKeys is the write order of an entry's fields; anything else the file
// carries is kept and written after them, sorted.
var knownKeys = []string{"type", "command", "args", "env", "default_mode", "default_config_options", "favorite_config_option_values"}

// Installed is one agent's entry. Unknown keys round-trip untouched.
type Installed struct {
	Type    string
	Command string
	Args    []string
	Env     map[string]string
	// DefaultMode is applied after session/new when the agent offers it.
	DefaultMode string
	rest        map[string]json.RawMessage
}

// InstalledMap maps agent ids to entries.
type InstalledMap map[string]*Installed

// InstalledPath is the map's file.
func InstalledPath() (string, error) {
	d, err := DataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, externalAgentDir, "installed.json"), nil
}

// LoadInstalled reads the map; missing or unreadable is empty.
func LoadInstalled(path string) InstalledMap {
	m := InstalledMap{}
	b, err := os.ReadFile(path) //nolint:gosec // G304: our data dir
	if err != nil {
		return m
	}
	var raw map[string]map[string]json.RawMessage
	if json.Unmarshal(b, &raw) != nil {
		return m
	}
	for id, fields := range raw {
		e := &Installed{rest: map[string]json.RawMessage{}}
		for k, v := range fields {
			var err error
			switch k {
			case "type":
				err = json.Unmarshal(v, &e.Type)
			case "command":
				err = json.Unmarshal(v, &e.Command)
			case "args":
				err = json.Unmarshal(v, &e.Args)
			case "env":
				err = json.Unmarshal(v, &e.Env)
			case "default_mode":
				err = json.Unmarshal(v, &e.DefaultMode)
			default:
				e.rest[k] = v
			}
			if err != nil {
				e.rest[k] = v
			}
		}
		if e.Type == "extension" {
			e.Type = TypeRegistry
		}
		if e.Type != TypeRegistry && e.Type != TypeCustom {
			continue
		}
		m[id] = e
	}
	return m
}

// CommandPath is a custom entry's command with ~/ expanded.
func (e *Installed) CommandPath() string { return expandHome(e.Command) }

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// MarshalJSON writes the known fields in order; empty ones are omitted.
func (e *Installed) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	n := 0
	put := func(k string, v any) {
		raw, err := json.Marshal(v)
		if err != nil {
			return
		}
		if n > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteByte(':')
		b.Write(raw)
		n++
	}
	put("type", e.Type)
	if e.Type == TypeCustom && e.Command != "" {
		put("command", e.Command)
	}
	if len(e.Args) > 0 {
		put("args", e.Args)
	}
	if len(e.Env) > 0 {
		put("env", e.Env)
	}
	if e.DefaultMode != "" {
		put("default_mode", e.DefaultMode)
	}
	var extra []string
	for k := range e.rest {
		extra = append(extra, k)
	}
	sort.Slice(extra, func(i, j int) bool {
		return keyRank(extra[i]) < keyRank(extra[j]) || (keyRank(extra[i]) == keyRank(extra[j]) && extra[i] < extra[j])
	})
	for _, k := range extra {
		v := bytes.TrimSpace(e.rest[k])
		if string(v) == "{}" || string(v) == "[]" || string(v) == "null" {
			continue // empty options are omitted on write
		}
		put(k, json.RawMessage(v))
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func keyRank(k string) int {
	for i, x := range knownKeys {
		if x == k {
			return i
		}
	}
	return len(knownKeys)
}

// Save writes the map, two-space indented like the desktop, ids sorted.
func (m InstalledMap) Save(path string) error {
	b, err := json.MarshalIndent(map[string]*Installed(m), "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b, 0o644)
}

// ErrNotInstalled is an id with no installed entry.
var ErrNotInstalled = errors.New("not installed")
