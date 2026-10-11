package hooks

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

// Dispatcher runs one hook invocation for one Project.
type Dispatcher struct {
	Root    string // the Project root (holds .atlas/)
	Version string
	Now     func() time.Time
	// Deliver hands a session event or a captured turn to the capture store
	// (deliver.go); tests replace it.
	Deliver func(ctx context.Context, d Delivery) error
	// Banner is the SessionStart message; "" prints nothing.
	Banner string
}

// Delivery is what a hook hands to the capture side: a session start, a
// turn, or a session end for one native session.
type Delivery struct {
	Root    string               `json:"root"`
	Session record.Session       `json:"session"`
	Start   bool                 `json:"start,omitempty"`
	Turn    *record.CapturedTurn `json:"turn,omitempty"`
	End     bool                 `json:"end,omitempty"`
}

// Run handles one event. Every error is logged and swallowed by the caller;
// Run itself returns what to print on stdout.
func (d *Dispatcher) Run(ctx context.Context, a Agent, ev *Event) ([]byte, error) {
	if ev == nil || ev.SessionID == "" {
		return nil, nil
	}
	if !ValidSessionID(ev.SessionID) {
		return nil, ErrBadSessionID
	}
	now := d.Now()
	st, err := LoadState(d.Root, a.ID(), ev.SessionID)
	if err != nil {
		return nil, err
	}
	if st == nil {
		st = &State{Agent: a.ID(), SessionID: ev.SessionID, StartedAt: now}
	}
	st.UpdatedAt = now
	if ev.Transcript != "" {
		st.Transcript = ev.Transcript
	}
	if ev.Model != "" {
		st.Model = ev.Model
	}
	sess := record.Session{Agent: a.ID(), NativeID: ev.SessionID, Cwd: d.Root, Model: st.Model}
	if !st.Started && ev.Kind != SessionEnd {
		// The first hook of a session, whichever it is, announces it.
		st.Started = true
		if err := st.Save(d.Root); err != nil {
			return nil, err
		}
		_ = d.deliver(ctx, st, Delivery{Root: d.Root, Session: sess, Start: true})
	}

	switch ev.Kind {
	case SessionStart:
		Sweep(d.Root, now)
		st.Ended = false
		if err := st.Save(d.Root); err != nil {
			return nil, err
		}
		if d.Banner == "" {
			return nil, nil
		}
		return a.Response(SessionStart, d.Banner), nil

	case TurnStart:
		if st.InTurn && ev.Prompt == "" {
			// A second TurnStart without a TurnEnd and without a prompt
			// (Antigravity's per-model-call PreInvocation): keep the turn
			// open with its baseline.
			return nil, st.Save(d.Root)
		}
		st.InTurn = true
		st.Prompt = ev.Prompt
		st.ToolFiles = nil
		st.Status = gitStatus(ctx, d.Root)
		if r := a.Reader(); r != nil && st.Transcript != "" {
			if pos, err := r.Position(st.Transcript); err == nil {
				st.Offset = pos
			}
		}
		return nil, st.Save(d.Root)

	case ToolUse:
		for _, f := range ev.Files {
			st.ToolFiles = append(st.ToolFiles, relPath(d.Root, f))
		}
		return nil, st.Save(d.Root)

	case TurnEnd:
		turn, err := d.turnEnd(ctx, a, st, ev)
		if err != nil {
			_ = st.Save(d.Root)
			return nil, err
		}
		st.InTurn, st.Prompt, st.ToolFiles, st.Status = false, "", nil, nil
		if turn == nil {
			return nil, st.Save(d.Root)
		}
		st.Turn++
		if err := st.Save(d.Root); err != nil {
			return nil, err
		}
		sess.Model = st.Model
		return nil, d.deliver(ctx, st, Delivery{Root: d.Root, Session: sess, Turn: turn})

	case Compaction:
		return nil, st.Save(d.Root)

	case SessionEnd:
		// A session whose UI fired no turn hooks (Cursor's -p mode) still has
		// its transcript: capture it as one turn now.
		if st.Turn == 0 && !st.InTurn && st.Transcript != "" && a.Reader() != nil {
			st.Offset, st.Status = 0, nil
			if turn, err := d.turnEnd(ctx, a, st, ev); err == nil && turn != nil {
				st.Turn = 1
				sess.Model = st.Model
				_ = d.deliver(ctx, st, Delivery{Root: d.Root, Session: sess, Turn: turn})
			}
		}
		// The state stays (atlas agents sessions lists it with its resume
		// command) until Sweep retires it.
		st.Ended = true
		if err := st.Save(d.Root); err != nil {
			return nil, err
		}
		return nil, d.deliver(ctx, st, Delivery{Root: d.Root, Session: sess, End: true})

	case SubagentStart, SubagentEnd:
		return nil, st.Save(d.Root)
	}
	return nil, nil
}

// turnEnd builds the captured turn: the transcript since the turn's offset,
// the git status diff, and the hook's own usage when it has one.
func (d *Dispatcher) turnEnd(ctx context.Context, a Agent, st *State, ev *Event) (*record.CapturedTurn, error) {
	turn := &record.CapturedTurn{Prompt: st.Prompt, At: d.Now(), Model: st.Model}
	if r := a.Reader(); r != nil && st.Transcript != "" {
		r.Settle(ctx, st.Transcript) // some agents write the transcript after Stop
		t, pos, err := r.Read(st.Transcript, st.Offset)
		if err != nil {
			if !errors.Is(err, fs.ErrNotExist) {
				return nil, err
			}
			// No transcript to read: keep the turn with its prompt and the
			// files git saw change, and say so once.
			LogError(d.Root, a.ID(), "turn-end", fmt.Errorf("transcript %s is not there; recorded the prompt and file changes only", filepath.Base(st.Transcript)))
			t, pos = &Turn{}, st.Offset
		}
		st.Offset = pos
		if turn.Prompt == "" && len(t.Prompts) > 0 {
			turn.Prompt = strings.Join(t.Prompts, "\n\n")
		}
		turn.Items = t.Items
		if t.Model != "" {
			turn.Model, st.Model = t.Model, t.Model
		}
		if t.HasUsage {
			u := t.Usage
			turn.Usage = &u
		}
	}
	if ev.Usage != nil {
		turn.Usage = ev.Usage // the hook's own total wins
	}
	if ev.Model != "" {
		turn.Model = ev.Model
	}
	// Files the agent's tool hooks named, then whatever git says changed
	// during the turn that no tool call explains.
	explained := map[string]bool{}
	for _, it := range turn.Items {
		if it.Tool == nil {
			continue
		}
		for _, p := range record.WritePaths(it.Tool.Arguments, it.Tool.Locations, it.Tool.Diffs...) {
			rel, _ := record.ProjectPath(d.Root, p)
			explained[rel] = true
		}
	}
	for _, p := range st.ToolFiles {
		if !explained[p] {
			turn.Changed = append(turn.Changed, p)
			explained[p] = true
		}
	}
	if st.Status != nil {
		for _, p := range changedSince(d.Root, st.Status, gitStatus(ctx, d.Root)) {
			if !explained[p] {
				turn.Changed = append(turn.Changed, p)
			}
		}
	}
	if turn.Prompt == "" && len(turn.Items) == 0 && len(turn.Changed) == 0 && !ev.TurnEndFailed {
		return nil, nil // nothing happened (a turn that only read)
	}
	return turn, nil
}

// deliver hands off to the store; a failure spools (deliver.go) and is
// reported once in the hooks log.
func (d *Dispatcher) deliver(ctx context.Context, st *State, dl Delivery) error {
	if d.Deliver == nil {
		return errors.New("no delivery configured")
	}
	err := d.Deliver(ctx, dl)
	if errors.Is(err, ErrSpooled) {
		st.Pending++
		_ = st.Save(d.Root)
		return nil
	}
	return err
}

// Enabled reports that this Project captures: a store exists and its
// binding is on. It reads the store without the writer lock.
func Enabled(ctx context.Context, root string) bool {
	if !store.Exists(root) {
		return false
	}
	st, err := store.Open(root, false)
	if err != nil {
		return false
	}
	defer st.Close()
	b, err := st.Binding(ctx)
	return err == nil && b != nil && b.Enabled
}
