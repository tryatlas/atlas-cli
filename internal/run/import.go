package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tryatlas/atlas-cli/internal/agents/acp"
	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/clistate"
)

// Importing sessions run in an agent's own UI (ADR-0012): each installed
// agent that can list and load sessions is asked, over ACP, for this
// folder's sessions; a new or changed one is replayed with session/load and
// its turns are recorded like the desktop's (source acp, the agent's session
// id). Nothing reads an agent's files.

// ImportStatePath is the CLI's own record of what was imported:
// {"<agent>": {"<session id>": "<when it was last imported>"}}.
func ImportStatePath(root string) string { return clistate.Path(root, "import.json") }

type importState map[string]map[string]string

func loadImportState(root string) importState {
	st := importState{}
	b, err := os.ReadFile(ImportStatePath(root))
	if err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (s importState) save(root string) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := ImportStatePath(root) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil { //nolint:gosec // G306: holds no secret
		return err
	}
	return os.Rename(tmp, ImportStatePath(root))
}

// ImportResult is one agent's pass.
type ImportResult struct {
	Agent    string
	Listed   int // sessions the agent has for this folder
	Replayed int // sessions loaded because they were new or changed
	Turns    int // turns written
	Note     string
	Err      error
}

// collector gathers a replay's updates.
type collector struct {
	mu      sync.Mutex
	session string
	updates []acp.Update
}

func (c *collector) Update(sessionID string, u acp.Update) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sessionID == c.session {
		c.updates = append(c.updates, u)
	}
}
func (c *collector) Permission(context.Context, acp.PermissionRequest) (string, bool) {
	return "", false
}
func (c *collector) Elicit(context.Context, acp.Elicitation) acp.ElicitationAnswer {
	return acp.ElicitationAnswer{Action: "cancel"}
}
func (c *collector) ElicitationComplete(string) {}

func (c *collector) start(id string) {
	c.mu.Lock()
	c.session, c.updates = id, nil
	c.mu.Unlock()
}

func (c *collector) take() []acp.Update {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.updates
	c.updates = nil
	return u
}

// Turns rebuilds turns from a replay: a user message starts a turn; agent
// text and thinking runs and tool calls follow in order, a tool call
// merged across its sightings.
func Turns(updates []acp.Update) []record.ImportedTurn {
	var turns []record.ImportedTurn
	cur := -1
	lastKind := ""
	runs := map[string]int{} // run key → item index in the current turn
	tools := map[string]int{}
	n := 0
	newTurn := func() {
		turns = append(turns, record.ImportedTurn{})
		cur = len(turns) - 1
		runs, tools = map[string]int{}, map[string]int{}
	}
	for _, u := range updates {
		switch u.Kind {
		case "user_message_chunk":
			if cur < 0 || lastKind != "user" {
				newTurn()
			}
			turns[cur].Prompt += u.Block().TextOf()
			lastKind = "user"
		case "agent_message_chunk", "agent_thought_chunk":
			if cur < 0 {
				newTurn()
			}
			mode := "text"
			if u.Kind == "agent_thought_chunk" {
				mode = "thinking"
			}
			text := u.Block().TextOf()
			key := mode + "\x00" + u.MessageID
			if i, ok := runs[key]; ok && lastKind == mode && (u.MessageID != "" || i == len(turns[cur].Items)-1) {
				turns[cur].Items[i].Body += text
			} else {
				n++
				id := u.MessageID
				if id == "" || ok {
					id = fmt.Sprintf("replay-%d-%d", cur+1, n)
				}
				turns[cur].Items = append(turns[cur].Items, record.ImportedItem{NativeID: id, Mode: mode, Body: text})
				runs[key] = len(turns[cur].Items) - 1
			}
			lastKind = mode
		case "tool_call", "tool_call_update":
			if cur < 0 {
				newTurn()
			}
			t := toolOf(u)
			if i, ok := tools[u.ToolCallID]; ok {
				merged := merge(*turns[cur].Items[i].Tool, t)
				turns[cur].Items[i].Tool = &merged
			} else {
				turns[cur].Items = append(turns[cur].Items, record.ImportedItem{NativeID: u.ToolCallID, Tool: &t})
				tools[u.ToolCallID] = len(turns[cur].Items) - 1
			}
			lastKind = "tool"
		}
	}
	return turns
}

// Importer imports native-UI sessions for one Project. It keeps one
// connection per agent between passes (an agent can take tens of seconds to
// start) and reconnects one that went away.
type Importer struct {
	Agents Agents
	Host   *Host
	Root   string

	mu    sync.Mutex
	conns map[string]*importConn
	force chan struct{}
}

type importConn struct {
	ag  *acp.Agent
	col *collector
}

// NewImporter returns an importer for a host's Project.
func NewImporter(a Agents, h *Host, root string) *Importer {
	return &Importer{Agents: a, Host: h, Root: root, conns: map[string]*importConn{}, force: make(chan struct{}, 1)}
}

// ImportNow asks a running Loop for a pass now.
func (im *Importer) ImportNow() {
	select {
	case im.force <- struct{}{}:
	default:
	}
}

// Loop imports every interval, and on ImportNow, until ctx ends.
func (im *Importer) Loop(ctx context.Context, interval time.Duration, onPass func([]ImportResult, error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		res, err := im.Run(ctx, nil, false)
		if onPass != nil {
			onPass(res, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-im.force:
		}
	}
}

// Close ends every agent connection.
func (im *Importer) Close() {
	im.mu.Lock()
	defer im.mu.Unlock()
	for id, c := range im.conns {
		c.ag.Close()
		delete(im.conns, id)
	}
}

func (im *Importer) conn(ctx context.Context, id string) (*importConn, error) {
	im.mu.Lock()
	c := im.conns[id]
	im.mu.Unlock()
	if c != nil {
		select {
		case <-c.ag.Done():
			c.ag.Close()
			c = nil
		default:
			return c, nil
		}
	}
	col := &collector{}
	ag, _, err := im.Agents.Launch(ctx, id, im.Root, col)
	if err != nil {
		return nil, err
	}
	c = &importConn{ag: ag, col: col}
	im.mu.Lock()
	im.conns[id] = c
	im.mu.Unlock()
	return c, nil
}

// Run does one pass over the installed agents (or the named ones).
func (im *Importer) Run(ctx context.Context, only []string, dryRun bool) ([]ImportResult, error) {
	if im.Host == nil || im.Host.Recorder == nil {
		return nil, errors.New("capture is not available here (atlas init, or another Atlas window holds the store)")
	}
	installed, _, err := im.Agents.Installed()
	if err != nil {
		return nil, err
	}
	var ids []string
	if len(only) > 0 {
		ids = only
	} else {
		for id := range installed {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	state := loadImportState(im.Root)
	var out []ImportResult
	for _, id := range ids {
		if ctx.Err() != nil {
			break
		}
		if installed[id] == nil {
			out = append(out, ImportResult{Agent: id, Err: fmt.Errorf("%s: %w", id, acp.ErrNotInstalled)})
			continue
		}
		out = append(out, im.importAgent(ctx, id, state, dryRun))
		if !dryRun {
			if err := state.save(im.Root); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

func (im *Importer) importAgent(ctx context.Context, id string, state importState, dryRun bool) ImportResult {
	r := ImportResult{Agent: id}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	c, err := im.conn(ctx, id)
	if err != nil {
		r.Err = err
		return r
	}
	ag, col, root := c.ag, c.col, im.Root
	caps := ag.Init.AgentCapabilities
	if !caps.CanList() || !caps.LoadSession {
		r.Note = "cannot list and load its sessions over ACP"
		return r
	}
	var listed []acp.ListedSession
	cursor := ""
	for range 20 {
		l, next, err := ag.ListSessions(ctx, root, cursor)
		if err != nil {
			r.Err = err
			return r
		}
		listed = append(listed, l...)
		if next == "" {
			break
		}
		cursor = next
	}
	r.Listed = len(listed)
	seen := state[id]
	if seen == nil {
		seen = map[string]string{}
		state[id] = seen
	}
	for _, s := range listed {
		if s.Cwd != "" && filepath.Clean(s.Cwd) != root {
			continue
		}
		if prev, ok := seen[s.SessionID]; ok && !newer(s.UpdatedAt, prev) {
			continue
		}
		r.Replayed++
		if dryRun {
			continue
		}
		col.start(s.SessionID)
		if _, err := ag.LoadSession(ctx, acp.SessionParams{Cwd: root, SessionID: s.SessionID}); err != nil {
			r.Note = strings.TrimSpace(r.Note + " " + s.SessionID + ": " + firstLine(err.Error()))
			continue
		}
		turns := Turns(col.take())
		_ = ag.CloseSession(ctx, s.SessionID)
		n, err := im.Host.Recorder.ImportTurns(ctx, record.Session{Agent: id, NativeID: s.SessionID, Cwd: root}, turns, time.Now())
		if err != nil {
			r.Err = err
			return r
		}
		r.Turns += n
		// Loading a session bumps its updatedAt on some agents: remember when
		// this import finished, so only later activity replays it again.
		seen[s.SessionID] = time.Now().UTC().Format(time.RFC3339Nano)
	}
	return r
}

// newer reports that a listed updatedAt is after the stored import time;
// an unreadable or missing timestamp counts as newer.
func newer(updated, imported string) bool {
	u, err1 := time.Parse(time.RFC3339Nano, updated)
	i, err2 := time.Parse(time.RFC3339Nano, imported)
	if err1 != nil || err2 != nil {
		return updated == "" || updated != imported
	}
	return u.After(i)
}
