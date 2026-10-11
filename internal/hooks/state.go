package hooks

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

// Session state lives under <root>/.atlas/hooks/<agent>/<session id>.json:
// what the dispatcher needs between two hook invocations of one session.
// .atlas/ ignores itself in git.

// Dir is the hooks state directory of a Project.
func Dir(root string) string { return filepath.Join(root, store.DirName, "hooks") }

// LogPath is where hook errors go.
func LogPath(root string) string { return filepath.Join(root, store.DirName, "cli-hooks.log") }

// State is one native session as the hooks have seen it.
type State struct {
	Agent      string    `json:"agent"`
	SessionID  string    `json:"session_id"`
	Transcript string    `json:"transcript,omitempty"`
	Model      string    `json:"model,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Turn is the number of turns delivered so far.
	Turn int `json:"turn"`
	// InTurn is set between TurnStart and TurnEnd.
	InTurn bool `json:"in_turn,omitempty"`
	// Offset is the transcript position at the last turn boundary.
	Offset int `json:"offset"`
	// Prompt is the typed prompt of the open turn.
	Prompt string `json:"prompt,omitempty"`
	// Status is git status at turn start: path → true; nil when no snapshot
	// was taken (an empty map is a clean tree, so no omitempty).
	Status map[string]bool `json:"status"`
	// ToolFiles are files the agent's tool hooks reported during the turn.
	ToolFiles []string `json:"tool_files,omitempty"`
	// Started is set once the session start was delivered (some agents
	// fire their first prompt hook before their session-start hook).
	Started bool `json:"started,omitempty"`
	// Ended is set by SessionEnd.
	Ended bool `json:"ended,omitempty"`
	// Pending counts spooled deliveries waiting for a writer.
	Pending int `json:"pending,omitempty"`
}

func statePath(root, agent, id string) string {
	return filepath.Join(Dir(root), agent, id+".json")
}

// LoadState reads a session's state; a missing one is nil, nil.
func LoadState(root, agent, id string) (*State, error) {
	if !ValidSessionID(id) {
		return nil, ErrBadSessionID
	}
	b, err := os.ReadFile(statePath(root, agent, id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", statePath(root, agent, id), err)
	}
	return &s, nil
}

// Save writes the state atomically (0600: it holds the typed prompt).
func (s *State) Save(root string) error {
	if !ValidSessionID(s.SessionID) {
		return ErrBadSessionID
	}
	p := statePath(root, s.Agent, s.SessionID)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Remove deletes a session's state.
func Remove(root, agent, id string) error {
	if !ValidSessionID(id) {
		return ErrBadSessionID
	}
	err := os.Remove(statePath(root, agent, id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ListStates returns an agent's sessions, newest first; agent "" means all.
func ListStates(root, agent string) ([]*State, error) {
	var agents []string
	if agent != "" {
		agents = []string{agent}
	} else {
		ents, err := os.ReadDir(Dir(root))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, nil
			}
			return nil, err
		}
		for _, e := range ents {
			if e.IsDir() && e.Name() != "spool" {
				agents = append(agents, e.Name())
			}
		}
	}
	var out []*State
	for _, a := range agents {
		ents, err := os.ReadDir(filepath.Join(Dir(root), a))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if filepath.Ext(e.Name()) != ".json" {
				continue
			}
			b, err := os.ReadFile(filepath.Join(Dir(root), a, e.Name())) //nolint:gosec // G304: the Project's own state files
			if err != nil {
				continue
			}
			var s State
			if json.Unmarshal(b, &s) == nil && s.SessionID != "" {
				out = append(out, &s)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out, nil
}

// StaleAfter is how long a session with no hook activity is kept.
const StaleAfter = 14 * 24 * time.Hour

// Sweep deletes session states with nothing pending that saw no hook for
// StaleAfter.
func Sweep(root string, now time.Time) {
	states, _ := ListStates(root, "")
	for _, s := range states {
		if s.Pending == 0 && now.Sub(s.UpdatedAt) > StaleAfter {
			_ = Remove(root, s.Agent, s.SessionID)
		}
	}
}

// LogError appends one line to the hooks log and remembers it as the last
// error for atlas status.
func LogError(root string, agent, verb string, err error) {
	if err == nil {
		return
	}
	line := fmt.Sprintf("%s %s %s: %v\n", time.Now().UTC().Format(time.RFC3339), agent, verb, err)
	if f, ferr := os.OpenFile(LogPath(root), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); ferr == nil {
		_, _ = f.WriteString(line)
		_ = f.Close()
	}
	_ = os.MkdirAll(Dir(root), 0o755)
	_ = os.WriteFile(filepath.Join(Dir(root), "last-error"), []byte(line), 0o600)
}

// LastError is the most recent hook error, or "".
func LastError(root string) string {
	b, err := os.ReadFile(filepath.Join(Dir(root), "last-error"))
	if err != nil {
		return ""
	}
	return string(b)
}

// ClearLastError forgets the last error (after a successful delivery).
func ClearLastError(root string) { _ = os.Remove(filepath.Join(Dir(root), "last-error")) }

// CurrentSession is the agent's most recent live session in the Project, for
// a memory bridge that starts alongside it; "" when none is recent.
func CurrentSession(root, agent string) string {
	states, err := ListStates(root, agent)
	if err != nil {
		return ""
	}
	for _, s := range states {
		if !s.Ended && time.Since(s.UpdatedAt) < 2*time.Hour {
			return s.SessionID
		}
	}
	return ""
}
