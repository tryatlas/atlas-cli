package record

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/redact"
)

// A turn captured through an agent's hooks (ADR-0013): the hook read the
// agent's transcript at turn end and diffed git status, so the whole turn
// arrives at once.

// CapturedTurn is one turn as a hook delivers it.
type CapturedTurn struct {
	Prompt string         `json:"prompt"`
	Items  []CapturedItem `json:"items,omitempty"`
	Model  string         `json:"model,omitempty"`
	// Usage is the turn's token usage when the transcript or the hook gave
	// one; nil records nothing.
	Usage *store.TokenTotals `json:"usage,omitempty"`
	// Changed are Project-relative paths git saw change during the turn that
	// no tool call in Items explains (shell commands, subagents).
	Changed []string  `json:"changed,omitempty"`
	At      time.Time `json:"at"`
}

// CapturedItem is one assistant run or one tool call, in order.
type CapturedItem struct {
	ID   string `json:"id,omitempty"`   // message id
	Mode string `json:"mode,omitempty"` // text | thinking
	Body string `json:"body,omitempty"`
	Tool *Tool  `json:"tool,omitempty"`
}

// RecordTurn writes one captured turn: the open-and-closed turn row, the
// prompt (and the title on the first), the assistant runs, the tool calls
// with their file touches and edit patches, the unexplained changes, and the
// usage. It returns the turn number (0 when capture is off).
func (r *Recorder) RecordTurn(ctx context.Context, s Session, t CapturedTurn) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.enabled(ctx) {
		return 0, nil
	}
	l, err := r.session(ctx, s)
	if err != nil {
		return 0, err
	}
	at := t.At
	if at.IsZero() {
		at = r.Now()
	}
	l.turn++
	if err := r.Store.BeginTurn(ctx, l.id, l.turn, at); err != nil {
		return 0, err
	}
	stripped := StripUser(t.Prompt)
	if strings.TrimSpace(stripped) != "" {
		if flat, err := redact.Flat(stripped); err == nil {
			if title := Title(flat.Text); title != "" {
				_ = r.Store.SetTitleIfAbsent(ctx, l.id, title)
			}
		}
		if body, ok := r.redactAuto(ctx, l, stripped); ok {
			if _, err := r.Store.RecordMessage(ctx, store.Message{SessionID: l.id, TurnSeq: l.turn,
				NativeID: fmt.Sprintf("prompt-%d-%s", l.turn, store.SHA256Hex([]byte(t.Prompt))),
				Role:     "user", Mode: "text", Body: body, CreatedAt: at}); err != nil {
				return l.turn, err
			}
		}
	}
	var lastShell string // the call id unexplained changes are attached to
	var lastCall string
	n := 0
	for _, it := range t.Items {
		if it.Tool != nil {
			id, name, err := r.recordCapturedTool(ctx, l, *it.Tool, at)
			if err != nil {
				return l.turn, err
			}
			if id != "" {
				lastCall = id
				if name == "Bash" {
					lastShell = id
				}
			}
			continue
		}
		body := StripAssistant(it.Body)
		if strings.TrimSpace(body) == "" {
			continue
		}
		red, ok := r.redactAuto(ctx, l, body)
		if !ok {
			continue
		}
		n++
		id := it.ID
		if id == "" {
			id = fmt.Sprintf("run-%d-%d", l.turn, n)
		}
		mode := it.Mode
		if mode == "" {
			mode = "text"
		}
		if _, err := r.Store.RecordMessage(ctx, store.Message{SessionID: l.id, TurnSeq: l.turn, NativeID: id,
			Role: "assistant", Mode: mode, Body: red, CreatedAt: at}); err != nil {
			return l.turn, err
		}
	}
	if len(t.Changed) > 0 {
		call := lastShell
		if call == "" {
			call = lastCall
		}
		if call == "" {
			// Nothing in the transcript to hang the changes on: a shell-shaped
			// call standing for the turn's observed changes.
			id, _, err := r.Store.UpsertToolCall(ctx, store.ToolCall{SessionID: l.id, TurnSeq: l.turn,
				NativeID: fmt.Sprintf("changes-%d", l.turn), ToolName: "Bash", Title: opt("Changes observed during the turn"),
				Kind: opt(ACPKind("Bash")), Status: "completed", Locations: "[]"}, at)
			if err != nil {
				return l.turn, err
			}
			call = id
		}
		inHead := headFiles(r.Root)
		for _, p := range t.Changed {
			ft := store.FileTouch{ToolCallID: call, SessionID: l.id, TurnSeq: l.turn, Path: p, ExistedBefore: inHead[p]}
			if filepath.IsAbs(p) {
				ft.OutOfRepo = true
			}
			abs := p
			if !ft.OutOfRepo {
				abs = filepath.Join(r.Root, filepath.FromSlash(p))
			}
			if data, err := os.ReadFile(abs); err == nil { //nolint:gosec // G304: a path git reported changed
				h := store.SHA256Hex(data)
				ft.SHA256After, ft.SketchAfter = &h, Sketch(data)
			} else {
				ft.Deleted = true
			}
			if err := r.Store.AddFileTouch(ctx, ft, at); err != nil {
				return l.turn, err
			}
		}
	}
	if t.Usage != nil {
		if !l.seeded {
			cur, err := r.Store.UsageCursor(ctx, l.id)
			if err != nil {
				return l.turn, err
			}
			l.usage, l.seeded = cur, true
		}
		l.usage.Input += t.Usage.Input
		l.usage.Output += t.Usage.Output
		l.usage.CacheCreation += t.Usage.CacheCreation
		l.usage.CacheRead += t.Usage.CacheRead
		l.usage.Reasoning += t.Usage.Reasoning
		if err := r.Store.RecordUsage(ctx, l.id, l.turn, opt(t.Model), l.usage, at); err != nil {
			return l.turn, err
		}
	}
	return l.turn, r.Store.CompleteTurn(ctx, l.id, l.turn, at)
}

// recordCapturedTool writes one tool call from a transcript and samples the
// files it wrote. It returns the row id and the canonical name.
func (r *Recorder) recordCapturedTool(ctx context.Context, l *live, t Tool, at time.Time) (string, string, error) {
	if t.NativeID == "" {
		return "", "", nil
	}
	name := CanonicalToolName(t.Name, t.Title, t.Kind, t.Arguments)
	kind := t.Kind
	if kind == "" {
		kind = ACPKind(name)
	}
	title := t.Title
	if title == "" {
		title = t.Name
	}
	var args, result *string
	if a := bytes.TrimSpace(t.Arguments); len(a) > 0 && string(a) != "{}" && string(a) != "null" {
		red, ok := r.redactAuto(ctx, l, string(a))
		if !ok {
			return "", name, nil
		}
		args = &red
	}
	if t.Result != nil {
		red, ok := r.redactAuto(ctx, l, *t.Result)
		if !ok {
			return "", name, nil
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
	status := t.Status
	if status == "" || status == "pending" || status == "running" {
		status = "completed" // the turn ended, so the call did too
	}
	id, _, err := r.Store.UpsertToolCall(ctx, store.ToolCall{SessionID: l.id, TurnSeq: l.turn, NativeID: t.NativeID,
		ToolName: name, Title: opt(title), Kind: opt(kind), Status: status, Locations: locs,
		Arguments: args, Result: result, ResultBinary: t.Binary}, at)
	if err != nil {
		return "", name, err
	}
	if status == "completed" && (WritesFiles(name) || len(t.Diffs) > 0) && !l.touched[t.NativeID] {
		l.touched[t.NativeID] = true
		if err := r.sampleWrites(ctx, l, id, t.NativeID, WritePaths(t.Arguments, t.Locations, t.Diffs...), t.Arguments, t.Diffs, at); err != nil {
			return id, name, err
		}
	}
	return id, name, nil
}
