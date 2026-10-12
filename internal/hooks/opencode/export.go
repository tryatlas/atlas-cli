package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tryatlas/cli/internal/capture/record"
	"github.com/tryatlas/cli/internal/hooks"
)

// OpenCode keeps its sessions in its own database, never in a file the
// plugin could name. The transcript is `opencode export <id>`: one JSON
// document {info, messages:[{info:{role, modelID, tokens}, parts:[…]}]},
// which the reader writes to the Project's export cache and reads back. The
// reader's unit is the message index.
//
// Position, at turn start, trusts a cache left by the previous turn end: it
// ends exactly where the last turn ended, and an export costs seconds the
// user would wait for at every prompt. Without one (a resumed session, a
// first turn) it exports and counts up to the prompt just sent. Read, at
// turn end, always exports afresh.

// CachePath is the export cache for a session: a subdirectory of the agent's
// state directory, so the session listing never reads it as state.
func CachePath(root, id string) string {
	return filepath.Join(hooks.Dir(root), ID, "export", id+".json")
}

// idOf is the session id a cache path names.
func idOf(path string) string { return strings.TrimSuffix(filepath.Base(path), ".json") }

// ExportTimeout bounds one `opencode export`.
const ExportTimeout = 30 * time.Second

// runExport runs `opencode export <id>` with stdout on out (a pipe can
// truncate a large export; a file cannot). Tests replace it.
var runExport = func(ctx context.Context, id string, out *os.File) error {
	cmd := exec.CommandContext(ctx, "opencode", "export", id) //nolint:gosec // G204: a validated session id to the agent's own export
	cmd.Stdout = out
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	switch {
	case err == nil:
		return nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("opencode export %s: timed out after %s", id, ExportTimeout)
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		return errors.New("opencode export: opencode is not on PATH")
	}
	if detail := errorLine(stderr.String()); detail != "" {
		return fmt.Errorf("opencode export %s: %s", id, detail)
	}
	return fmt.Errorf("opencode export %s: %w", id, err)
}

// ansi matches the colour codes OpenCode puts around its messages.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// errorLine is the first "error: …" line of the export's stderr, else "".
func errorLine(stderr string) string {
	for _, l := range strings.Split(ansi.ReplaceAllString(stderr, ""), "\n") {
		l = strings.TrimSpace(l)
		if len(l) > 6 && strings.EqualFold(l[:6], "error:") {
			return strings.TrimSpace(l[6:])
		}
	}
	return ""
}

// fetch exports the session into path (0600) and parses it. The export goes
// to a sibling temp file and replaces the cache only once it parses, so a
// failed or truncated export never loses a good cache.
func fetch(id, path string) (*session, error) {
	if !hooks.ValidSessionID(id) {
		return nil, hooks.ErrBadSessionID
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), id+".*.tmp") // 0600
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	ctx, cancel := context.WithTimeout(context.Background(), ExportTimeout)
	defer cancel()
	runErr := runExport(ctx, id, tmp)
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if runErr != nil {
		return nil, runErr
	}
	s, err := load(tmp.Name())
	if err != nil {
		return nil, fmt.Errorf("opencode export %s: %w", id, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return nil, err
	}
	return s, nil
}

// load parses a cached export.
func load(path string) (*session, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the Project's own export cache
	if err != nil {
		return nil, err
	}
	return parseExport(b)
}

// parseExport decodes one export document.
func parseExport(b []byte) (*session, error) {
	var s session
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// reader reads the export cache for a session.
type reader struct{}

// Position is the message count at the last turn boundary: the cache's when
// one exists, else a fresh export's without the user message just sent.
func (reader) Position(path string) (int, error) {
	if s, err := load(path); err == nil {
		return len(s.Messages), nil
	}
	s, err := fetch(idOf(path), path)
	if err != nil {
		return 0, err
	}
	n := len(s.Messages)
	for n > 0 && s.Messages[n-1].Info.Role == "user" {
		n--
	}
	return n, nil
}

// Settle has nothing to wait for: the export is taken when Read runs.
func (reader) Settle(context.Context, string) {}

// Read exports the session and returns the turn held by the messages from a
// position, and the new position.
func (reader) Read(path string, from int) (*hooks.Turn, int, error) {
	s, err := fetch(idOf(path), path)
	if err != nil {
		return nil, from, err
	}
	if from < 0 || from > len(s.Messages) {
		from = 0
	}
	return turnOf(s.Messages[from:]), len(s.Messages), nil
}

// ── The export document ──────────────────────────────────────────────────────

type session struct {
	Info     json.RawMessage `json:"info"`
	Messages []message       `json:"messages"`
}

type message struct {
	Info  messageInfo `json:"info"`
	Parts []part      `json:"parts"`
}

type messageInfo struct {
	ID      string  `json:"id"`
	Role    string  `json:"role"`
	ModelID string  `json:"modelID"`
	Tokens  *tokens `json:"tokens"`
}

type tokens struct {
	Input     int64 `json:"input"`
	Output    int64 `json:"output"`
	Reasoning int64 `json:"reasoning"`
	Cache     struct {
		Read  int64 `json:"read"`
		Write int64 `json:"write"`
	} `json:"cache"`
}

// part is one message part: text, reasoning, tool, or one of the kinds the
// turn ignores (step-start, step-finish, patch, file, …).
type part struct {
	ID     string     `json:"id"`
	Type   string     `json:"type"`
	Text   string     `json:"text"`
	Tool   string     `json:"tool"`
	CallID string     `json:"callID"`
	State  *toolState `json:"state"`
}

type toolState struct {
	Status   string          `json:"status"` // pending | running | completed | error
	Input    json.RawMessage `json:"input"`
	Output   string          `json:"output"`
	Error    string          `json:"error"`
	Metadata json.RawMessage `json:"metadata"` // per tool; apply_patch lists files
}

// files are the paths a tool's metadata says it wrote (apply_patch's
// files[].filePath); other tools name their file in the input, which the
// recorder's write-path rule reads.
func (s *toolState) files() []string {
	if s == nil || len(s.Metadata) == 0 {
		return nil
	}
	var meta struct {
		Files []struct {
			FilePath string `json:"filePath"`
		} `json:"files"`
	}
	if json.Unmarshal(s.Metadata, &meta) != nil {
		return nil
	}
	var out []string
	for _, f := range meta.Files {
		if f.FilePath != "" {
			out = append(out, f.FilePath)
		}
	}
	return out
}

// turnOf folds a slice of export messages into one turn: user text parts are
// the prompts, assistant text and reasoning parts the runs, tool parts the
// calls; tokens are summed over the assistant messages, the model is the
// last assistant message's.
func turnOf(msgs []message) *hooks.Turn {
	t := &hooks.Turn{}
	for _, m := range msgs {
		switch m.Info.Role {
		case "user":
			for _, p := range m.Parts {
				if p.Type != "text" {
					continue
				}
				if text := StripReminders(p.Text); text != "" {
					t.Prompts = append(t.Prompts, text)
				}
			}
		case "assistant":
			if m.Info.ModelID != "" {
				t.Model = m.Info.ModelID
			}
			if u := m.Info.Tokens; u != nil {
				t.HasUsage = true
				t.Usage.Input += u.Input
				t.Usage.Output += u.Output
				t.Usage.Reasoning += u.Reasoning
				t.Usage.CacheRead += u.Cache.Read
				t.Usage.CacheCreation += u.Cache.Write
			}
			for i, p := range m.Parts {
				id := p.ID
				if id == "" {
					id = m.Info.ID + "-" + strconv.Itoa(i)
				}
				switch p.Type {
				case "text":
					if strings.TrimSpace(p.Text) != "" {
						t.Items = append(t.Items, hooks.Item{ID: id, Mode: "text", Body: p.Text})
					}
				case "reasoning":
					if strings.TrimSpace(p.Text) != "" {
						t.Items = append(t.Items, hooks.Item{ID: id, Mode: "thinking", Body: p.Text})
					}
				case "tool":
					t.Items = append(t.Items, hooks.Item{Tool: toolOf(p, id)})
				}
			}
		}
	}
	return t
}

// toolOf translates a tool part. Its id is the callID, which the plugin's
// tool.execute.after hook also receives (opencode.go toolOf): one id for one
// call, however it arrives. The part id (prt_…) stands in when a part has no
// callID.
func toolOf(p part, fallbackID string) *record.Tool {
	tool := &record.Tool{NativeID: p.CallID, Name: p.Tool, Status: "completed"}
	if tool.NativeID == "" {
		tool.NativeID = p.ID
	}
	if tool.NativeID == "" {
		tool.NativeID = fallbackID
	}
	s := p.State
	if s == nil {
		return tool
	}
	switch s.Status {
	case "error":
		tool.Status = "failed"
	case "pending", "running", "completed":
		tool.Status = s.Status
	}
	if len(s.Input) > 0 && string(s.Input) != "null" {
		tool.Arguments = s.Input
	}
	res := s.Output
	if res == "" && s.Status == "error" {
		res = s.Error
	}
	if res != "" {
		tool.Result = &res
	}
	tool.Locations = s.files()
	return tool
}

// StripReminders removes the <system-reminder>…</system-reminder> blocks
// that orchestration plugins inject as user text, and trims the rest.
func StripReminders(text string) string {
	const open, closing = "<system-reminder>", "</system-reminder>"
	for {
		i := strings.Index(text, open)
		if i < 0 {
			break
		}
		j := strings.Index(text[i:], closing)
		if j < 0 {
			break
		}
		text = text[:i] + text[i+j+len(closing):]
	}
	return strings.TrimSpace(text)
}
