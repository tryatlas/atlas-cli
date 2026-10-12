package hooks

import (
	"bytes"
	"context"
	"encoding/json"
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

// Delivery is what a hook hands to the capture side for one native session:
// a session start, an opening prompt, one completed tool call, the turn's
// close (or a whole turn), or a session end.
type Delivery struct {
	Root    string               `json:"root"`
	Session record.Session       `json:"session"`
	Start   bool                 `json:"start,omitempty"`
	Open    *OpenTurn            `json:"open,omitempty"`
	Tool    *record.Tool         `json:"tool,omitempty"`
	Turn    *record.CapturedTurn `json:"turn,omitempty"`
	End     bool                 `json:"end,omitempty"`
	// Changed are Project-relative paths git saw change while Tool (a
	// shell call) ran; they are recorded as its file touches.
	Changed []string `json:"changed,omitempty"`
	// At is when the hook fired, so a spooled delivery keeps its time.
	At time.Time `json:"at,omitempty"`
	// LinkFrom, when set, asks the writer to link the commits after it to
	// the touches just recorded (commits made during the turn).
	LinkFrom string `json:"link_from,omitempty"`
	// Plan is the session's whole plan after an incremental task tool
	// changed it, folded as plan_set.
	Plan *PlanFold `json:"plan,omitempty"`
}

// OpenTurn is the prompt that opens a turn as it is submitted.
type OpenTurn struct {
	Prompt string    `json:"prompt"`
	At     time.Time `json:"at"`
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
	// Hooks of one session can fire concurrently (parallel tool calls):
	// one at a time per session, so the state's read-modify-write holds.
	unlock, err := lockSession(d.Root, a.ID(), ev.SessionID)
	if err != nil {
		return nil, err
	}
	defer unlock()
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
		_ = d.deliver(ctx, st, Delivery{Root: d.Root, Session: sess, Start: true, At: now})
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
		st.ToolFiles, st.Delivered, st.Tools, st.Open = nil, nil, nil, false
		st.Status = gitStatus(ctx, d.Root)
		st.Head = gitHead(ctx, d.Root)
		st.TurnHead = st.Head
		if r := a.Reader(); r != nil && st.Transcript != "" {
			if pos, err := r.Position(st.Transcript); err == nil {
				st.Offset = pos
			}
		}
		if ev.Prompt != "" {
			// The prompt opens the turn now; tool calls follow as they
			// complete, and the reply closes it (per-event capture).
			st.Open = true
			if err := st.Save(d.Root); err != nil {
				return nil, err
			}
			return nil, d.deliver(ctx, st, Delivery{Root: d.Root, Session: sess, Open: &OpenTurn{Prompt: ev.Prompt, At: now}, At: now})
		}
		return nil, st.Save(d.Root)

	case ToolUse:
		for _, f := range ev.Files {
			st.ToolFiles = append(st.ToolFiles, relPath(d.Root, f))
		}
		if ev.Tool != nil && ev.Tool.NativeID != "" {
			if !st.Open {
				st.Tools = append(st.Tools, *ev.Tool)
				_ = trackTasks(st, *ev.Tool) // the turn-end delivery folds the plan
				return nil, st.Save(d.Root)
			}
			st.Delivered = append(st.Delivered, toolKeys(*ev.Tool))
			for _, p := range writtenPaths(*ev.Tool) {
				rel, _ := record.ProjectPath(d.Root, p)
				st.Delivered = append(st.Delivered, []string{"path:" + rel}) // explained: not a shell change
			}
			dl := Delivery{Root: d.Root, Session: sess, Tool: ev.Tool, At: now}
			dl.Plan = trackTasks(st, *ev.Tool)
			if record.CanonicalToolName(ev.Tool.Name, ev.Tool.Title, ev.Tool.Kind, ev.Tool.Arguments) == "Bash" && st.Status != nil {
				// What the shell command changed is known now: its touches,
				// and the baseline for the rest of the turn moves past them.
				after := gitStatus(ctx, d.Root)
				head := gitHead(ctx, d.Root)
				explained := explainedPaths(st.Delivered)
				changed := changedSince(d.Root, st.Status, after)
				if head != "" && head != st.Head {
					// The command committed: what it committed is no longer
					// in git status but is its work all the same.
					changed = append(changed, committedSince(ctx, d.Root, st.Head)...)
				}
				for _, p := range changed {
					if !explained[p] {
						dl.Changed = append(dl.Changed, p)
						st.Delivered = append(st.Delivered, []string{"path:" + p})
						explained[p] = true
					}
				}
				if after != nil {
					st.Status = after
				}
				if head != "" {
					st.Head = head
				}
			}
			if st.TurnHead != "" && gitHead(ctx, d.Root) != st.TurnHead {
				dl.LinkFrom = st.TurnHead
			}
			if err := st.Save(d.Root); err != nil {
				return nil, err
			}
			return nil, d.deliver(ctx, st, dl)
		}
		return nil, st.Save(d.Root)

	case TurnEnd:
		turn, err := d.turnEnd(ctx, a, st, ev)
		if err != nil {
			_ = st.Save(d.Root)
			return nil, err
		}
		st.InTurn, st.Prompt, st.ToolFiles, st.Status = false, "", nil, nil
		st.Open, st.Delivered, st.Tools = false, nil, nil
		if turn == nil {
			return nil, st.Save(d.Root)
		}
		st.Turn++
		linkFrom := ""
		if st.TurnHead != "" && gitHead(ctx, d.Root) != st.TurnHead {
			linkFrom = st.TurnHead // commits made during the turn
		}
		st.Head, st.TurnHead = "", ""
		// Task tool calls the hooks did not deliver (or that came before
		// the turn opened) still build the plan, once each.
		var plan *PlanFold
		for _, it := range turn.Items {
			if it.Tool != nil {
				if p := trackTasks(st, *it.Tool); p != nil {
					plan = p
				}
			}
		}
		if plan == nil && len(st.Tasks) > 0 && hadTaskCalls(turn.Items) {
			plan = planFromTasks(st.Tasks)
		}
		if err := st.Save(d.Root); err != nil {
			return nil, err
		}
		sess.Model = st.Model
		return nil, d.deliver(ctx, st, Delivery{Root: d.Root, Session: sess, Turn: turn, At: now, LinkFrom: linkFrom, Plan: plan})

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
				_ = d.deliver(ctx, st, Delivery{Root: d.Root, Session: sess, Turn: turn, At: now})
			}
		}
		// The state stays (atlas agents sessions lists it with its resume
		// command) until Sweep retires it.
		st.Ended = true
		if err := st.Save(d.Root); err != nil {
			return nil, err
		}
		return nil, d.deliver(ctx, st, Delivery{Root: d.Root, Session: sess, End: true, At: now})

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
	// Tool calls already delivered during the open turn are not recorded
	// again from the transcript; calls the hooks saw while no turn was open
	// join now.
	if st.Open {
		turn.Open = true
		turn.Items = dropDelivered(turn.Items, st.Delivered)
	}
	for i := range st.Tools {
		turn.Items = append(turn.Items, record.CapturedItem{Tool: &st.Tools[i]})
	}
	// Files the agent's tool hooks named, then whatever git says changed
	// during the turn that no tool call explains (delivered ones included).
	explained := explainedPaths(st.Delivered)
	for _, it := range turn.Items {
		if it.Tool == nil {
			continue
		}
		for _, p := range writtenPaths(*it.Tool) {
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
		changed := changedSince(d.Root, st.Status, gitStatus(ctx, d.Root))
		if head := gitHead(ctx, d.Root); head != "" && st.Head != "" && head != st.Head {
			changed = append(changed, committedSince(ctx, d.Root, st.Head)...)
		}
		for _, p := range changed {
			if !explained[p] {
				turn.Changed = append(turn.Changed, p)
				explained[p] = true
			}
		}
	}
	if turn.Prompt == "" && len(turn.Items) == 0 && len(turn.Changed) == 0 && !ev.TurnEndFailed && !turn.Open {
		return nil, nil // nothing happened (a turn that only read)
	}
	return turn, nil
}

// toolKeys identifies a tool call across the hook payload and the
// transcript, whose ids and argument shapes may differ: the agent's call
// id; the canonical name with a hash of the arguments (unless they are
// empty); for a shell call its command text (a string, or an argv array
// joined); for a call that writes files, each written path.
func toolKeys(t record.Tool) []string {
	keys := []string{"id:" + t.NativeID}
	name := record.CanonicalToolName(t.Name, t.Title, t.Kind, t.Arguments)
	if len(bytes.TrimSpace(t.Arguments)) > 0 {
		var compact bytes.Buffer
		if json.Compact(&compact, t.Arguments) == nil && compact.String() != "{}" && compact.String() != "null" {
			keys = append(keys, "args:"+strings.ToLower(name)+":"+store.SHA256Hex(compact.Bytes()))
		}
		var m struct {
			Command json.RawMessage `json:"command"`
			Cmd     json.RawMessage `json:"cmd"`
		}
		if json.Unmarshal(t.Arguments, &m) == nil {
			if c := commandText(m.Command) + commandText(m.Cmd); c != "" {
				keys = append(keys, "cmd:"+store.SHA256Hex([]byte(c)))
			}
		}
	}
	if record.WritesFiles(name) || len(t.Diffs) > 0 {
		for _, p := range record.WritePaths(t.Arguments, t.Locations, t.Diffs...) {
			keys = append(keys, "write:"+filepath.ToSlash(filepath.Clean(p)))
		}
	}
	return keys
}

// commandText flattens a command argument: a string, or an argv array.
func commandText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var argv []string
	if json.Unmarshal(raw, &argv) == nil {
		return strings.TrimSpace(strings.Join(argv, " "))
	}
	return ""
}

// explainedPaths are the Project paths delivered tool calls wrote.
func explainedPaths(delivered [][]string) map[string]bool {
	out := map[string]bool{}
	for _, g := range delivered {
		for _, k := range g {
			if rel, ok := strings.CutPrefix(k, "path:"); ok {
				out[rel] = true
			}
		}
	}
	return out
}

// dropDelivered removes the transcript's tool items already recorded from a
// hook, each delivered call matching at most one item. When the agent's hook
// ids match its transcript ids (any item's id was delivered) ids decide
// alone, so two distinct calls with the same command stay distinct; an item
// matched by id is kept, marked NoSample, so its result completes the row.
// Otherwise an item matches a delivered call by arguments, by command text,
// or by every path it wrote, and is dropped.
func dropDelivered(items []record.CapturedItem, delivered [][]string) []record.CapturedItem {
	groups := make([][]string, 0, len(delivered))
	for _, g := range delivered {
		if len(g) > 0 && !strings.HasPrefix(g[0], "path:") {
			groups = append(groups, g)
		}
	}
	has := func(g []string, key string) bool {
		for _, k := range g {
			if k == key {
				return true
			}
		}
		return false
	}
	idsMatch := false
	for _, it := range items {
		if it.Tool == nil {
			continue
		}
		for _, g := range groups {
			if has(g, "id:"+it.Tool.NativeID) {
				idsMatch = true
			}
		}
	}
	match := func(t record.Tool) int {
		keys := toolKeys(t)
		for i, g := range groups {
			if has(g, keys[0]) {
				return i
			}
		}
		if idsMatch {
			return -1
		}
		for i, g := range groups {
			for _, k := range keys[1:] {
				if !strings.HasPrefix(k, "write:") && has(g, k) {
					return i
				}
			}
		}
		// Every written path delivered (possibly by several calls): a
		// duplicate too; the first writing group is consumed.
		writes, first := 0, -1
		for _, k := range keys {
			if !strings.HasPrefix(k, "write:") {
				continue
			}
			found := -1
			for i, g := range groups {
				if has(g, k) {
					found = i
					break
				}
			}
			if found < 0 {
				return -1
			}
			writes++
			if first < 0 {
				first = found
			}
		}
		if writes > 0 {
			return first
		}
		return -1
	}
	kept := items[:0]
	for _, it := range items {
		if it.Tool != nil {
			if i := match(*it.Tool); i >= 0 {
				byID := has(groups[i], "id:"+it.Tool.NativeID)
				groups = append(groups[:i], groups[i+1:]...)
				if !byID {
					continue
				}
				// Same row: let the transcript complete it (result, status)
				// without sampling its writes again.
				it.Tool.NoSample = true
			}
		}
		kept = append(kept, it)
	}
	return kept
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

// writtenPaths are the files a tool call wrote. A call that only read or
// searched names files too (its locations), but explains no change: a file
// the agent read and then committed is the commit's change.
func writtenPaths(t record.Tool) []string {
	name := record.CanonicalToolName(t.Name, t.Title, t.Kind, t.Arguments)
	if !record.WritesFiles(name) && len(t.Diffs) == 0 {
		return nil
	}
	return record.WritePaths(t.Arguments, t.Locations, t.Diffs...)
}
