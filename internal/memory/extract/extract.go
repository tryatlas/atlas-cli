// Package extract distils durable memories from a captured session through
// the Atlas AI gateway, as the desktop's extractor does (evidence-memory.md
// §4.1): after at least 20 turns and 3 tool calls since the last pass, and
// once at session end; only with sharing on and the summariser not set to
// "local"; results land as entries with source "extractor", through the
// record's redactor. Unlike the desktop, the excerpt sent is the capture
// store's text, which was redacted when it was recorded.
package extract

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/clistate"
	"github.com/tryatlas/atlas-cli/internal/memory/record"
	"github.com/tryatlas/atlas-cli/internal/project"
)

// Gates and limits (the desktop's).
const (
	MinTurns     = 20
	MinToolCalls = 3
	ExcerptChars = 6000
	MaxEntries   = 8
	Timeout      = 60 * time.Second
)

// Options name one session and how to reach the gateway.
type Options struct {
	Root    string // the Project root (capture store)
	Agent   string // the desktop's agent id
	Session string // the agent's own session id
	Final   bool   // the session ended: extract whatever is new
	Gateway string // base URL, e.g. https://ai.tryatlas.cc/v1
	// Auth returns the bearer token and the active org id (may be "").
	Auth func(ctx context.Context) (token, org string, err error)
	HTTP *http.Client
	Now  func() time.Time
}

// ErrSkipped says a gate kept the pass from running.
var ErrSkipped = errors.New("extraction skipped")

// state is the extractor's progress for one session, in the desktop's file
// and format (crates/atlas-memory/src/extract.rs ExtractState):
// <scope>/.atlas/memory/extract-state/<sanitized session>.json.
type state struct {
	LastTurn        int64 `json:"last_extracted_turn_index"`
	ToolCallsSince  int   `json:"tool_calls_since_last"`
	ExtractionCount int   `json:"extraction_count"`
}

// sanitize keeps a session id filesystem-safe, as the desktop does.
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return r
		}
		return '_'
	}, s)
}

func statePath(scope, session string) string {
	return filepath.Join(filepath.Dir(record.Path(scope)), "extract-state", sanitize(session)+".json")
}

// legacyStatePath is where CLIs up to 0.4 kept it.
func legacyStatePath(root, session string) string {
	return filepath.Join(clistate.Dir(root), "extract", session+".json")
}

func loadState(scope, root, session string) state {
	var s state
	if b, err := os.ReadFile(statePath(scope, session)); err == nil {
		_ = json.Unmarshal(b, &s)
		return s
	}
	if b, err := os.ReadFile(legacyStatePath(root, session)); err == nil {
		var old struct {
			LastTurn        int64 `json:"last_extracted_turn"`
			ExtractionCount int   `json:"extraction_count"`
		}
		if json.Unmarshal(b, &old) == nil {
			s.LastTurn, s.ExtractionCount = old.LastTurn, old.ExtractionCount
		}
	}
	return s
}

// save writes atomically (temp file, rename), as the desktop does.
func (s state) save(scope, root, session string) error {
	p := statePath(scope, session)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := fmt.Sprintf("%s.%d.tmp", p, os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil { //nolint:gosec // G306: the app reads it; no secret
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	_ = os.Remove(legacyStatePath(root, session))
	return nil
}

// summariserLocal reads <root>/.atlas/memory-summarizer.json: mode "local"
// sends nothing anywhere; "provider" (a BYOK model) is the desktop's alone.
func summariserMode(root string) string {
	b, err := os.ReadFile(filepath.Join(root, store.DirName, "memory-summarizer.json")) //nolint:gosec // G304: the Project's own setting
	if err != nil {
		return ""
	}
	var v struct {
		Mode string `json:"mode"`
	}
	_ = json.Unmarshal(b, &v)
	return v.Mode
}

// Run does one pass and returns how many entries it wrote.
func Run(ctx context.Context, o Options) (int, error) {
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.HTTP == nil {
		o.HTTP = &http.Client{Timeout: Timeout}
	}
	if !project.MemorySharing(o.Root) {
		return 0, ErrSkipped
	}
	if m := summariserMode(o.Root); m == "local" || m == "provider" {
		return 0, ErrSkipped
	}
	// One pass per session at a time: a pass that finds another running
	// leaves the work to it.
	unlock, ok := tryLock(o.Root, o.Session)
	if !ok {
		return 0, ErrSkipped
	}
	defer unlock()
	scope := project.ScopeRoot(ctx, o.Root)
	st := loadState(scope, o.Root, o.Session)
	excerpt, last, tools, err := conversation(ctx, o.Root, o.Session, st.LastTurn)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrSkipped // the session is not in this store (yet)
	}
	if err != nil {
		return 0, err
	}
	turns := last - st.LastTurn
	switch {
	case turns <= 0 || strings.TrimSpace(excerpt) == "":
		return 0, ErrSkipped
	case !o.Final && (turns < MinTurns || tools < MinToolCalls):
		if tools != st.ToolCallsSince {
			st.ToolCallsSince = tools
			_ = st.save(scope, o.Root, o.Session)
		}
		return 0, ErrSkipped
	}
	token, org, err := o.Auth(ctx)
	if err != nil {
		return 0, err
	}
	entries, err := ask(ctx, o, token, org, excerpt)
	if err != nil {
		return 0, err
	}
	mem, err := record.Open(scope, true)
	if err != nil {
		return 0, err
	}
	defer mem.Close()
	n := 0
	at := o.Now().UnixMilli()
	for _, e := range entries {
		if n >= MaxEntries || !record.IsDurable(e.Kind) || strings.TrimSpace(e.Content) == "" {
			continue
		}
		c := e.Confidence
		if c <= 0 || c > 1 {
			c = 0.5
		}
		if _, _, err := mem.Remember(ctx, record.Write{Kind: e.Kind, Content: e.Content, Source: "extractor",
			Agent: o.Agent, Session: o.Session, Confidence: c, At: at, LogEvent: true}); err != nil {
			return n, err
		}
		n++
	}
	st.LastTurn = last
	st.ToolCallsSince = 0
	st.ExtractionCount++
	if o.Final && n > 0 {
		// The handoff was written at session end, before this pass: rewrite
		// it so the next session sees what was distilled.
		_ = mem.WriteHandoff(ctx, o.Session, o.Agent, scope, o.Now().UnixMilli())
	}
	return n, st.save(scope, o.Root, o.Session)
}

var injected = regexp.MustCompile(`(?s)<(system-reminder|local-command-caveat|local-command-stdout|command-message|task-notification)[^>]*>.*?</(system-reminder|local-command-caveat|local-command-stdout|command-message|task-notification)>`)

// conversation is the session's text after a turn, from the capture store:
// prompts, replies and the tools called, the last ExcerptChars of it, plus
// the session's last turn and the number of tool calls since.
func conversation(ctx context.Context, root, native string, after int64) (string, int64, int, error) {
	st, err := store.Open(root, false)
	if err != nil {
		return "", 0, 0, err
	}
	defer st.Close()
	db := st.DB()
	var id string
	if err := db.QueryRowContext(ctx, `SELECT id FROM agent_session WHERE workspace_id = ? AND native_session_id = ?
	    ORDER BY updated_at DESC LIMIT 1`, root, native).Scan(&id); err != nil {
		return "", 0, 0, err
	}
	var last int64
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(MAX(turn_seq), 0) FROM turn WHERE session_id = ?`, id).Scan(&last)
	var tools int
	_ = db.QueryRowContext(ctx, `SELECT count(*) FROM tool_call WHERE session_id = ? AND turn_seq > ?`, id, after).Scan(&tools)
	rows, err := db.QueryContext(ctx, `SELECT 'm', role, COALESCE(body, ''), seq FROM agent_message
	        WHERE session_id = ? AND turn_seq > ? AND mode = 'text'
	    UNION ALL SELECT 't', tool_name, COALESCE(title, ''), seq FROM tool_call WHERE session_id = ? AND turn_seq > ?
	    ORDER BY 4`, id, after, id, after)
	if err != nil {
		return "", 0, 0, err
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var typ, who, text string
		var seq int64
		if rows.Scan(&typ, &who, &text, &seq) != nil {
			continue
		}
		text = strings.TrimSpace(injected.ReplaceAllString(text, ""))
		if typ == "t" {
			fmt.Fprintf(&b, "[tool %s] %s\n", who, firstLine(text))
			continue
		}
		if text == "" {
			continue
		}
		label := "Assistant"
		if who == "user" {
			label = "User"
		}
		fmt.Fprintf(&b, "%s: %s\n\n", label, text)
	}
	s := b.String()
	if r := []rune(s); len(r) > ExcerptChars {
		s = string(r[len(r)-ExcerptChars:])
	}
	return s, last, tools, rows.Err()
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// Entry is one memory the model proposes.
type Entry struct {
	Kind       string  `json:"kind"`
	Content    string  `json:"content"`
	Confidence float64 `json:"confidence"`
}

// Prompt is what the model is asked; the excerpt follows it.
const Prompt = `You distil durable project memory from a coding-agent session in a software repository.
Read the session excerpt below and return only a JSON object, no prose:
{"entries":[{"kind":"decision|fact|failure|architecture|preference","content":"...","confidence":0.0}]}

Keep only what a future session in this repository needs and could not cheaply rediscover from the code:
- decision: a choice that was made and why
- fact: a durable truth about the project, its tools or its environment
- failure: a dead end or mistake, with the rule to follow next time
- architecture: how parts of the system fit together
- preference: something the user asked to be done a certain way from now on
Each content is one or two self-contained sentences. No session narration, no task status, no file lists, no secrets.
confidence is how sure you are the memory is true and durable (0 to 1).
Return {"entries":[]} when nothing qualifies. At most 8 entries.

Session excerpt:
`

func ask(ctx context.Context, o Options, token, org, excerpt string) ([]Entry, error) {
	model, err := defaultModel(ctx, o, token, org)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"model": model, "stream": false,
		"messages": []map[string]string{{"role": "user", "content": Prompt + excerpt}}})
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := call(ctx, o, token, org, http.MethodPost, "/chat/completions", body, &out); err != nil {
		return nil, err
	}
	if len(out.Choices) == 0 {
		return nil, errors.New("the gateway returned no choice")
	}
	return parseEntries(out.Choices[0].Message.Content)
}

// parseEntries reads {"entries":[…]} out of a reply that may wrap it in a
// code fence or prose.
func parseEntries(text string) ([]Entry, error) {
	i, j := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if i < 0 || j <= i {
		return nil, fmt.Errorf("no JSON object in the reply")
	}
	var v struct {
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(text[i:j+1]), &v); err != nil {
		return nil, err
	}
	return v.Entries, nil
}

// defaultModel is the gateway's model flagged default (the desktop's
// "gateway default model").
func defaultModel(ctx context.Context, o Options, token, org string) (string, error) {
	var out struct {
		Data []struct {
			ID      string `json:"id"`
			Default bool   `json:"default"`
		} `json:"data"`
	}
	if err := call(ctx, o, token, org, http.MethodGet, "/models", nil, &out); err != nil {
		return "", err
	}
	for _, m := range out.Data {
		if m.Default {
			return m.ID, nil
		}
	}
	if len(out.Data) > 0 {
		return out.Data[0].ID, nil
	}
	return "", errors.New("the gateway lists no model")
}

func call(ctx context.Context, o Options, token, org, method, path string, body []byte, out any) error {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(o.Gateway, "/")+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if org != "" {
		req.Header.Set("atlas-org", org)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gateway %s %s: %d", method, path, resp.StatusCode)
	}
	return json.Unmarshal(b, out)
}

// tryLock takes the session's extraction lock without waiting.
func tryLock(root, session string) (func(), bool) {
	p := legacyStatePath(root, session) + ".lock" // the CLI's own: .atlas/ holds only the app's files
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, false
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: the CLI's own lock file
	if err != nil {
		return nil, false
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		_ = f.Close()
		return nil, false
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, true
}
