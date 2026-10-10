package record

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/redact"
)

// SourceACP and SourceProxy are agent_session.source values. Proxy-captured
// sessions are written as acp until the desktop and ingest both accept proxy
// (ADR-0002, D14).
const (
	SourceACP   = "acp"
	SourceProxy = "proxy"
)

// Recorder writes one Project's capture. It is the single writer: callers
// from many goroutines are serialised.
type Recorder struct {
	Store  *store.Store
	Root   string // canonical Project root (workspace_id)
	Source string
	Now    func() time.Time

	mu       sync.Mutex
	sessions map[string]*live
}

// live is what the recorder remembers about a session in this process.
type live struct {
	id      string
	turn    int64
	model   string
	usage   store.TokenTotals
	seeded  bool
	anchor  map[string]bool // git status paths at the last anchor
	touched map[string]bool // native call ids whose writes were sampled
	calls   map[string]Tool // what earlier sightings said about each call
}

// New returns a recorder for an open store.
func New(st *store.Store, root string) *Recorder {
	return &Recorder{Store: st, Root: root, Source: SourceACP, Now: time.Now, sessions: map[string]*live{}}
}

// Session names one agent session.
type Session struct {
	Agent    string // the registry id of the agent
	NativeID string // the agent's own session id
	Cwd      string
	Model    string
}

func (r *Recorder) enabled(ctx context.Context) bool {
	b, err := r.Store.Binding(ctx)
	return err == nil && b != nil && b.Enabled
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// session upserts the session row and returns its live state.
func (r *Recorder) session(ctx context.Context, s Session) (*live, error) {
	k := s.Agent + "\x00" + s.NativeID
	l := r.sessions[k]
	var branch *string
	if l == nil {
		branch = opt(gitOut(r.Root, "symbolic-ref", "--quiet", "--short", "HEAD"))
	}
	id, err := r.Store.UpsertSession(ctx, store.SessionKey{WorkspaceID: r.Root, Source: r.Source, NativeID: s.NativeID},
		opt(s.Agent), opt(s.Model), opt(s.Cwd), branch, r.Now())
	if err != nil {
		return nil, err
	}
	if l == nil {
		turn, err := r.Store.MaxTurnSeq(ctx, id)
		if err != nil {
			return nil, err
		}
		l = &live{id: id, turn: turn, touched: map[string]bool{}, calls: map[string]Tool{}}
		r.sessions[k] = l
	}
	if s.Model != "" {
		l.model = s.Model
	}
	return l, nil
}

// redactAuto runs the JSON-aware redactor and merges the counts; a failure
// flags the session and stores nothing for that content.
func (r *Recorder) redactAuto(ctx context.Context, l *live, text string) (string, bool) {
	res, err := redact.Auto(text)
	if err != nil {
		_ = r.Store.FlagNeedsAttention(ctx, l.id, "redaction failed, content was not stored: "+err.Error())
		return "", false
	}
	r.addCounts(ctx, l, res.Counts)
	return res.Text, true
}

func (r *Recorder) addCounts(ctx context.Context, l *live, c redact.Counts) {
	if c.Total() == 0 {
		return
	}
	m := map[string]int{}
	for k, v := range c {
		m[string(k)] = v
	}
	_ = r.Store.AddRedactionCounts(ctx, l.id, m)
}

// Prompt starts a turn: the session row, an open turn, the title from the
// first prompt, and the user message. It returns the turn number (0 when
// capture is off).
func (r *Recorder) Prompt(ctx context.Context, s Session, prompt string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled(ctx) {
		return 0, nil
	}
	l, err := r.session(ctx, s)
	if err != nil {
		return 0, err
	}
	l.turn++
	now := r.Now()
	if err := r.Store.BeginTurn(ctx, l.id, l.turn, now); err != nil {
		return 0, err
	}
	l.anchor = gitStatus(r.Root)

	stripped := StripUser(prompt)
	flat, err := redact.Flat(stripped)
	if err == nil {
		if t := Title(flat.Text); t != "" {
			_ = r.Store.SetTitleIfAbsent(ctx, l.id, t)
		}
	}
	body, ok := r.redactAuto(ctx, l, stripped)
	if !ok {
		return l.turn, nil
	}
	_, err = r.Store.RecordMessage(ctx, store.Message{SessionID: l.id, TurnSeq: l.turn,
		NativeID: fmt.Sprintf("prompt-%d-%s", l.turn, store.SHA256Hex([]byte(prompt))),
		Role:     "user", Mode: "text", Body: body, CreatedAt: now})
	return l.turn, err
}

// Turn reports a session's current turn and whether it is known.
func (r *Recorder) Turn(s Session) (int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.sessions[s.Agent+"\x00"+s.NativeID]
	if l == nil {
		return 0, false
	}
	return l.turn, true
}

// Message writes one assistant run (text or thinking) once per native id.
// A body that strips to blank is skipped.
func (r *Recorder) Message(ctx context.Context, s Session, nativeID, mode, body string, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled(ctx) {
		return nil
	}
	l, err := r.session(ctx, s)
	if err != nil || l.turn == 0 {
		return err
	}
	body = StripAssistant(body)
	if strings.TrimSpace(body) == "" {
		return nil
	}
	red, ok := r.redactAuto(ctx, l, body)
	if !ok {
		return nil
	}
	_, err = r.Store.RecordMessage(ctx, store.Message{SessionID: l.id, TurnSeq: l.turn, NativeID: nativeID,
		Role: "assistant", Mode: mode, Body: red, CreatedAt: at})
	return err
}

// Tool is one sighting of a tool call.
type Tool struct {
	NativeID  string
	Name      string // the wire name
	Title     string
	Kind      string // ACP kind, when the wire gives one
	Status    string // pending | running | completed | failed
	Arguments json.RawMessage
	Result    *string
	Binary    []byte
	Locations []string
}

// ToolCall upserts the call; on its first completed sighting it samples the
// files it wrote and stores the edit patch (SPEC §6.4).
func (r *Recorder) ToolCall(ctx context.Context, s Session, t Tool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled(ctx) {
		return nil
	}
	l, err := r.session(ctx, s)
	if err != nil || l.turn == 0 {
		return err
	}
	// A later sighting (a tool result) may carry only the status and result:
	// fill in what earlier sightings said.
	if prev, ok := l.calls[t.NativeID]; ok {
		if len(t.Arguments) == 0 {
			t.Arguments = prev.Arguments
		}
		if t.Name == "" {
			t.Name = prev.Name
		}
		if t.Title == "" {
			t.Title = prev.Title
		}
		if t.Kind == "" {
			t.Kind = prev.Kind
		}
		if len(t.Locations) == 0 {
			t.Locations = prev.Locations
		}
	}
	l.calls[t.NativeID] = Tool{Name: t.Name, Title: t.Title, Kind: t.Kind, Arguments: t.Arguments, Locations: t.Locations}
	name := CanonicalToolName(t.Name, t.Title, t.Kind, t.Arguments)
	kind := t.Kind
	if kind == "" {
		kind = ACPKind(name)
	}
	title := t.Title
	if title == "" {
		title = t.Name
	}
	var args *string
	if a := bytes.TrimSpace(t.Arguments); len(a) > 0 && string(a) != "{}" && string(a) != "null" {
		red, ok := r.redactAuto(ctx, l, string(a))
		if !ok {
			return nil
		}
		args = &red
	}
	var result *string
	if t.Result != nil {
		red, ok := r.redactAuto(ctx, l, *t.Result)
		if !ok {
			return nil
		}
		result = &red
	}
	locs := "[]"
	if len(t.Locations) > 0 {
		var list []map[string]string
		for _, p := range t.Locations {
			list = append(list, map[string]string{"path": p})
		}
		b, _ := json.Marshal(list)
		locs = string(b)
	}
	now := r.Now()
	id, _, err := r.Store.UpsertToolCall(ctx, store.ToolCall{SessionID: l.id, TurnSeq: l.turn, NativeID: t.NativeID,
		ToolName: name, Title: opt(title), Kind: opt(kind), Status: t.Status, Locations: locs,
		Arguments: args, Result: result, ResultBinary: t.Binary}, now)
	if err != nil {
		return err
	}
	if t.Status != "completed" || l.touched[t.NativeID] {
		if t.Status == "failed" {
			l.touched[t.NativeID] = true // a failed call records no writes
		}
		return nil
	}
	l.touched[t.NativeID] = true
	switch {
	case WritesFiles(name):
		return r.sampleWrites(ctx, l, id, WritePaths(t.Arguments, t.Locations), t.Arguments, now)
	case name == "Bash":
		return r.sampleShell(ctx, l, id, now)
	}
	return nil
}

// sampleWrites reads each written file now: sha256, sketch, deleted when the
// read fails; existed_before from HEAD.
func (r *Recorder) sampleWrites(ctx context.Context, l *live, callID string, paths []string, args json.RawMessage, now time.Time) error {
	inHead := headFiles(r.Root)
	for i, p := range paths {
		rel, out := ProjectPath(r.Root, p)
		abs := rel
		if !out {
			abs = filepath.Join(r.Root, filepath.FromSlash(rel))
		}
		ft := store.FileTouch{ToolCallID: callID, SessionID: l.id, TurnSeq: l.turn, Path: rel, OutOfRepo: out,
			ExistedBefore: !out && inHead[rel]}
		if data, err := os.ReadFile(abs); err == nil { //nolint:gosec // G304: a path the agent wrote
			h := store.SHA256Hex(data)
			ft.SHA256After, ft.SketchAfter = &h, Sketch(data)
		} else {
			ft.Deleted = true
		}
		if err := r.Store.AddFileTouch(ctx, ft, now); err != nil {
			return err
		}
		if l.anchor != nil && !out {
			// Explained by this call: a later shell command did not write it.
			l.anchor[rel] = true
		}
		if i == 0 {
			if patch := EditPatch(args); patch != "" {
				red, ok := r.redactAuto(ctx, l, patch)
				if ok {
					if err := r.Store.AddAgentEdit(ctx, callID, l.id, l.turn, rel, red, now); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// sampleShell records the files a shell command changed: paths dirty now
// that were clean at the anchor (the prompt, or the previous shell call). The
// proxy sees a shell call only after it ran, so the anchor is always the
// prompt-time one (SPEC §6.4).
func (r *Recorder) sampleShell(ctx context.Context, l *live, callID string, now time.Time) error {
	after := gitStatus(r.Root)
	if l.anchor == nil || after == nil {
		l.anchor = after
		return nil
	}
	inHead := headFiles(r.Root)
	for p := range after {
		if l.anchor[p] {
			continue
		}
		ft := store.FileTouch{ToolCallID: callID, SessionID: l.id, TurnSeq: l.turn, Path: p, ExistedBefore: inHead[p]}
		if data, err := os.ReadFile(filepath.Join(r.Root, filepath.FromSlash(p))); err == nil {
			h := store.SHA256Hex(data)
			ft.SHA256After, ft.SketchAfter = &h, Sketch(data)
		} else {
			ft.Deleted = true
		}
		if err := r.Store.AddFileTouch(ctx, ft, now); err != nil {
			return err
		}
	}
	l.anchor = after
	return nil
}

// Usage adds one request's usage. The recorder keeps a running sum per
// session (seeded from usage_cursor) and feeds the cumulative recorder, so
// the diff equals this request's usage (ADR-0005).
func (r *Recorder) Usage(ctx context.Context, s Session, model string, req store.TokenTotals) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled(ctx) {
		return nil
	}
	l, err := r.session(ctx, s)
	if err != nil || l.turn == 0 {
		return err
	}
	if !l.seeded {
		cur, err := r.Store.UsageCursor(ctx, l.id)
		if err != nil {
			return err
		}
		l.usage, l.seeded = cur, true
	}
	l.usage.Input += req.Input
	l.usage.Output += req.Output
	l.usage.CacheCreation += req.CacheCreation
	l.usage.CacheRead += req.CacheRead
	l.usage.Reasoning += req.Reasoning
	return r.Store.RecordUsage(ctx, l.id, l.turn, opt(model), l.usage, r.Now())
}

// FinishTurn completes the session's open turn.
func (r *Recorder) FinishTurn(ctx context.Context, s Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	l := r.sessions[s.Agent+"\x00"+s.NativeID]
	if l == nil || l.turn == 0 {
		return nil
	}
	return r.Store.CompleteTurn(ctx, l.id, l.turn, r.Now())
}

// ── git (read-only) ──────────────────────────────────────────────────────────

func gitRaw(dir string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: fixed read-only git subcommands
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	return cmd.Output()
}

func gitOut(dir string, args ...string) string {
	b, err := gitRaw(dir, args...)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// gitStatus is the set of paths git status reports (rename origins included).
func gitStatus(root string) map[string]bool {
	b, err := gitRaw(root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return nil
	}
	out := map[string]bool{}
	fields := strings.Split(string(b), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 {
			continue
		}
		out[f[3:]] = true
		if f[0] == 'R' || f[0] == 'C' { // the next field is the origin
			if i+1 < len(fields) && fields[i+1] != "" {
				out[fields[i+1]] = true
			}
			i++
		}
	}
	return out
}

// headFiles is the set of paths tracked at HEAD.
func headFiles(root string) map[string]bool {
	b, err := gitRaw(root, "ls-tree", "-r", "-z", "--name-only", "HEAD")
	out := map[string]bool{}
	if err != nil {
		return out
	}
	for _, p := range strings.Split(string(b), "\x00") {
		if p != "" {
			out[p] = true
		}
	}
	return out
}
