// Package hooks captures sessions run in an agent's own UI through the
// agent's lifecycle hooks (ADR-0013). Each agent integration translates its
// native hook payload into one Event; the dispatcher does the rest: session
// state in the CLI state directory, the transcript read from the offset recorded at
// turn start, the git status diff, and delivery to the capture store.
package hooks

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"regexp"

	"github.com/tryatlas/cli/internal/capture/record"
	"github.com/tryatlas/cli/internal/capture/store"
)

// Kind is a normalised lifecycle event.
type Kind int

// The event kinds, in lifecycle order.
const (
	SessionStart Kind = iota + 1
	TurnStart
	TurnEnd
	Compaction
	SessionEnd
	SubagentStart
	SubagentEnd
	ToolUse
)

func (k Kind) String() string {
	switch k {
	case SessionStart:
		return "session-start"
	case TurnStart:
		return "turn-start"
	case TurnEnd:
		return "turn-end"
	case Compaction:
		return "compaction"
	case SessionEnd:
		return "session-end"
	case SubagentStart:
		return "subagent-start"
	case SubagentEnd:
		return "subagent-end"
	case ToolUse:
		return "tool-use"
	}
	return "unknown"
}

// Event is what an agent's hook payload says happened.
type Event struct {
	Kind       Kind
	SessionID  string // the agent's own session id
	Transcript string // the transcript path the payload names, if any
	Cwd        string // the payload's cwd, if any
	Prompt     string // TurnStart: the typed prompt
	Model      string
	Usage      *store.TokenTotals // a hook-provided usage total for the turn
	Files      []string           // ToolUse: files the tool changed
	Tool       *record.Tool       // ToolUse: the completed call, when the payload carries it
	ToolUseID  string
	// TurnEndFailed marks a turn that ended on an API error.
	TurnEndFailed bool
}

// sessionIDPattern is what a session id may look like before it becomes a
// file name in the hooks state directory.
var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)

// ErrBadSessionID is a session id that cannot be a file name.
var ErrBadSessionID = errors.New("the hook payload's session id is not usable")

// ValidSessionID reports whether id is safe to use as a file name.
func ValidSessionID(id string) bool { return sessionIDPattern.MatchString(id) }

// maxPayload bounds a hook payload read from stdin.
const maxPayload = 10 << 20

// ReadPayload reads one JSON value from a hook's stdin. It returns nothing
// when stdin is a terminal, and stops after one value even if the agent never
// closes the pipe.
func ReadPayload(r io.Reader) ([]byte, error) {
	if f, ok := r.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			return nil, nil
		}
	}
	dec := json.NewDecoder(io.LimitReader(r, maxPayload))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	return bytes.TrimSpace(raw), nil
}

// Str reads a string field of a decoded payload; a missing or non-string
// field is "".
func Str(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// Decode parses a payload into a map, tolerating an empty payload.
func Decode(payload []byte) (map[string]any, error) {
	m := map[string]any{}
	if len(bytes.TrimSpace(payload)) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return nil, err
	}
	return m, nil
}
