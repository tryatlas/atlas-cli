package store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/tryatlas/cli/internal/timefmt"
)

// Size limits (SPEC §6.3): payloads strictly larger than SpillBytes go to
// blobs/; previews are the first PreviewBytes on a character boundary.
const (
	SpillBytes   = 64 * 1024
	PreviewBytes = 2 * 1024
)

// NewID mints "<prefix>" + a UUID v4 in simple form (as-, am-, tc-, ft-, ae-, cp-).
func NewID(prefix string) string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return prefix + hex.EncodeToString(b[:])
}

// SHA256Hex is lowercase hex sha256.
func SHA256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ── Blobs ────────────────────────────────────────────────────────────────────

// PutBlob stores bytes content-addressed at blobs/<hh>/<sha256> (write to
// .tmp, fsync, rename; a no-op when present) and returns the key.
func (s *Store) PutBlob(data []byte) (string, error) {
	key := SHA256Hex(data)
	dir := filepath.Join(s.Root, DirName, "blobs", key[:2])
	path := filepath.Join(dir, key)
	if _, err := os.Stat(path); err == nil {
		return key, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // G304: inside the store
	if err != nil {
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: inside the store
		_ = d.Sync()
		_ = d.Close()
	}
	return key, nil
}

// spill keeps a text inline, or stores it as a blob when it is too large.
func (s *Store) spill(text *string) (inline, ref *string, err error) {
	if text == nil {
		return nil, nil, nil
	}
	if len(*text) <= SpillBytes {
		return text, nil, nil
	}
	key, err := s.PutBlob([]byte(*text))
	if err != nil {
		return nil, nil, err
	}
	return nil, &key, nil
}

// Preview is the first ≤2 KiB of a body, cut on a character boundary.
func Preview(body string) string {
	if len(body) <= PreviewBytes {
		return body
	}
	cut := PreviewBytes
	for cut > 0 && !utf8.RuneStart(body[cut]) {
		cut--
	}
	return body[:cut]
}

// ── Sessions and turns ───────────────────────────────────────────────────────

// SessionKey identifies a session: one row per (workspace, source, native id).
type SessionKey struct {
	WorkspaceID string
	Source      string
	NativeID    string
}

// initialSync is the sync_state of a new row: local for a local binding,
// pending for a cloud one.
func (s *Store) initialSync(ctx context.Context) string {
	if b, err := s.Binding(ctx); err == nil && b != nil && b.Mode == ModeCloud {
		return "pending"
	}
	return "local"
}

// resync is the clause that sends a changed row again.
const resync = `sync_state = CASE WHEN sync_state = 'sent' THEN 'pending' ELSE sync_state END`

// UpsertSession inserts a session or updates it: agent, model and cwd take the
// new value when given, branch keeps the first one (store.rs:203-258).
func (s *Store) UpsertSession(ctx context.Context, k SessionKey, agent, model, cwd, branch *string, now time.Time) (string, error) {
	ts := timefmt.RFC3339(now)
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM agent_session WHERE workspace_id = ? AND source = ? AND native_session_id = ?`,
		k.WorkspaceID, k.Source, k.NativeID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = NewID("as-")
		_, err = s.db.ExecContext(ctx, `INSERT INTO agent_session (id, workspace_id, source, native_session_id, agent, model, cwd,
		        started_at, updated_at, sync_state, branch, last_activity_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, k.WorkspaceID, k.Source, k.NativeID, agent, model, cwd, ts, ts, s.initialSync(ctx), branch, ts)
		return id, err
	}
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `UPDATE agent_session SET agent = COALESCE(?, agent), model = COALESCE(?, model),
	        branch = COALESCE(branch, ?), cwd = COALESCE(?, cwd), updated_at = ? WHERE id = ?`,
		agent, model, branch, cwd, ts, id)
	return id, err
}

// MaxTurnSeq seeds a session's turn counter.
func (s *Store) MaxTurnSeq(ctx context.Context, sessionID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(turn_seq), 0) FROM turn WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}

// BeginTurn opens a turn (a re-run is a no-op).
func (s *Store) BeginTurn(ctx context.Context, sessionID string, turnSeq int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO turn (session_id, turn_seq, state, started_at) VALUES (?, ?, 'open', ?)
	    ON CONFLICT DO NOTHING`, sessionID, turnSeq, timefmt.RFC3339(now))
	return err
}

// CompleteTurn closes an open turn and bumps last_activity_at.
func (s *Store) CompleteTurn(ctx context.Context, sessionID string, turnSeq int64, now time.Time) error {
	ts := timefmt.RFC3339(now)
	if _, err := s.db.ExecContext(ctx, `UPDATE turn SET state = 'completed', ended_at = ? WHERE session_id = ? AND turn_seq = ? AND state = 'open'`,
		ts, sessionID, turnSeq); err != nil {
		return err
	}
	return s.bumpActivity(ctx, s.db, sessionID, ts)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (s *Store) bumpActivity(ctx context.Context, x execer, sessionID, ts string) error {
	_, err := x.ExecContext(ctx, `UPDATE agent_session SET updated_at = ?, last_activity_at =
	        CASE WHEN last_activity_at IS NULL OR last_activity_at < ? THEN ? ELSE last_activity_at END WHERE id = ?`,
		ts, ts, ts, sessionID)
	return err
}

// SetTitleIfAbsent writes the title only when there is none (the first prompt wins).
func (s *Store) SetTitleIfAbsent(ctx context.Context, sessionID, title string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_session SET title = ?, `+resync+`
	    WHERE id = ? AND (title IS NULL OR title = '')`, title, sessionID)
	return err
}

// NoteBranch records the branch once.
func (s *Store) NoteBranch(ctx context.Context, sessionID, branch string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_session SET branch = ? WHERE id = ? AND branch IS NULL`, branch, sessionID)
	return err
}

// FlagNeedsAttention marks a session whose content could not be stored.
func (s *Store) FlagNeedsAttention(ctx context.Context, sessionID, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agent_session SET needs_attention = 1, attention_reason = ? WHERE id = ?`, reason, sessionID)
	return err
}

// redactionCategories is the order new categories are appended in.
var redactionCategories = []string{"entropy", "vendor_rule", "provider_token", "credentialed_uri", "connection_string", "credential_value"}

// AddRedactionCounts merges counts into agent_session.redaction_counts,
// keeping the keys already there in their order and appending new ones.
func (s *Store) AddRedactionCounts(ctx context.Context, sessionID string, counts map[string]int) error {
	if len(counts) == 0 {
		return nil
	}
	var raw string
	if err := s.db.QueryRowContext(ctx, `SELECT redaction_counts FROM agent_session WHERE id = ?`, sessionID).Scan(&raw); err != nil {
		return err
	}
	keys, vals := orderedCounts(raw)
	for _, c := range redactionCategories {
		n := counts[c]
		if n == 0 {
			continue
		}
		if _, ok := vals[c]; !ok {
			keys = append(keys, c)
		}
		vals[c] += n
	}
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%d", k, vals[k])
	}
	b.WriteByte('}')
	_, err := s.db.ExecContext(ctx, `UPDATE agent_session SET redaction_counts = ? WHERE id = ?`, b.String(), sessionID)
	return err
}

// orderedCounts reads {"k": n, …} keeping key order.
func orderedCounts(raw string) ([]string, map[string]int) {
	vals := map[string]int{}
	var keys []string
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return keys, vals
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			break
		}
		k, _ := t.(string)
		var n int
		if dec.Decode(&n) != nil {
			break
		}
		keys = append(keys, k)
		vals[k] = n
	}
	return keys, vals
}

// ── Messages ─────────────────────────────────────────────────────────────────

// Message is one user, assistant or system row. Body is already redacted.
type Message struct {
	SessionID string
	TurnSeq   int64
	NativeID  string
	Role      string // user | assistant | system
	Mode      string // text | tool | thinking
	Body      string
	CreatedAt time.Time
}

// nextSeq increments the store-wide counter inside a transaction.
func nextSeq(ctx context.Context, tx *sql.Tx) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE counter SET value = value + 1 WHERE name = 'seq'`); err != nil {
		return 0, err
	}
	var n int64
	err := tx.QueryRowContext(ctx, `SELECT value FROM counter WHERE name = 'seq'`).Scan(&n)
	return n, err
}

// RecordMessage writes a message once per native id; false when it was
// already there.
func (s *Store) RecordMessage(ctx context.Context, m Message) (bool, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM agent_message WHERE session_id = ? AND native_message_id = ?`,
		m.SessionID, m.NativeID).Scan(&exists); err != nil {
		return false, err
	}
	if exists > 0 {
		return false, nil
	}
	body := m.Body
	inline, ref, err := s.spill(&body)
	if err != nil {
		return false, err
	}
	syncState := s.initialSync(ctx) // before the transaction: one connection only
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return false, err
	}
	ts := timefmt.RFC3339(m.CreatedAt)
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_message (id, session_id, seq, turn_seq, native_message_id, role, mode,
	        preview, body, body_ref, body_bytes, content_hash, created_at, sync_state)
	    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		NewID("am-"), m.SessionID, seq, m.TurnSeq, m.NativeID, m.Role, m.Mode, Preview(body), inline, ref,
		len(body), SHA256Hex([]byte(body)), ts, syncState); err != nil {
		return false, err
	}
	if err := s.bumpActivity(ctx, tx, m.SessionID, ts); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ── Tool calls, file touches, edits ──────────────────────────────────────────

// ToolCall is one sighting of a tool call. Arguments and Result are already
// redacted; ResultBinary holds a non-UTF-8 result stored verbatim.
type ToolCall struct {
	SessionID    string
	TurnSeq      int64
	NativeID     string
	ToolName     string
	Title        *string
	Kind         *string
	Status       string // pending | running | completed | failed
	Locations    string // JSON array; "[]" on an update means no news
	Arguments    *string
	Result       *string
	ResultBinary []byte
}

// UpsertToolCall inserts or updates by (session, native id) with the
// desktop's COALESCE rules and returns the row id and its previous status
// ("" for a new row).
func (s *Store) UpsertToolCall(ctx context.Context, tc ToolCall, now time.Time) (string, string, error) {
	if tc.Locations == "" {
		tc.Locations = "[]"
	}
	args, argsRef, err := s.spill(tc.Arguments)
	if err != nil {
		return "", "", err
	}
	result, resultRef, err := s.spill(tc.Result)
	if err != nil {
		return "", "", err
	}
	binary := 0
	if tc.ResultBinary != nil {
		key, err := s.PutBlob(tc.ResultBinary)
		if err != nil {
			return "", "", err
		}
		result, resultRef, binary = nil, &key, 1
	}
	syncState := s.initialSync(ctx) // before the transaction: one connection only
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", "", err
	}
	defer func() { _ = tx.Rollback() }()
	var id, prev string
	err = tx.QueryRowContext(ctx, `SELECT id, status FROM tool_call WHERE session_id = ? AND native_call_id = ?`,
		tc.SessionID, tc.NativeID).Scan(&id, &prev)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id = NewID("tc-")
		seq, err := nextSeq(ctx, tx)
		if err != nil {
			return "", "", err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO tool_call (id, session_id, seq, turn_seq, native_call_id, tool_name, title,
		        kind, status, locations, arguments, arguments_ref, result, result_ref, result_binary, created_at, sync_state)
		    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			id, tc.SessionID, seq, tc.TurnSeq, tc.NativeID, tc.ToolName, tc.Title, tc.Kind, tc.Status, tc.Locations,
			args, argsRef, result, resultRef, binary, timefmt.RFC3339(now), syncState); err != nil {
			return "", "", err
		}
	case err != nil:
		return "", "", err
	default:
		if _, err := tx.ExecContext(ctx, `UPDATE tool_call SET tool_name = ?, title = COALESCE(?, title), kind = COALESCE(?, kind),
		        status = ?, locations = CASE WHEN ? = '[]' THEN locations ELSE ? END,
		        arguments = COALESCE(?, arguments), arguments_ref = COALESCE(?, arguments_ref),
		        result = COALESCE(?, result), result_ref = COALESCE(?, result_ref),
		        result_binary = CASE WHEN ? = 1 THEN 1 ELSE result_binary END, `+resync+` WHERE id = ?`,
			tc.ToolName, tc.Title, tc.Kind, tc.Status, tc.Locations, tc.Locations, args, argsRef, result, resultRef, binary, id); err != nil {
			return "", "", err
		}
	}
	if err := s.bumpActivity(ctx, tx, tc.SessionID, timefmt.RFC3339(now)); err != nil {
		return "", "", err
	}
	return id, prev, tx.Commit()
}

// FileTouch is one written path, sampled when the call completed.
type FileTouch struct {
	ToolCallID    string
	SessionID     string
	TurnSeq       int64
	Path          string // project-relative "/" path, or absolute when OutOfRepo
	SHA256After   *string
	SketchAfter   *string
	ExistedBefore bool
	Deleted       bool
	OutOfRepo     bool
}

// AddFileTouch writes one file_touch row.
func (s *Store) AddFileTouch(ctx context.Context, ft FileTouch, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	seq, err := nextSeq(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO file_touch (id, tool_call_id, session_id, turn_seq, seq, path, sha256_after,
	        existed_before, deleted, out_of_repo, created_at, sketch_after) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		NewID("ft-"), ft.ToolCallID, ft.SessionID, ft.TurnSeq, seq, ft.Path, ft.SHA256After, ft.ExistedBefore,
		ft.Deleted, ft.OutOfRepo, timefmt.RFC3339(now), ft.SketchAfter); err != nil {
		return err
	}
	return tx.Commit()
}

// AddAgentEdit writes the call's edit patch (already redacted), spilled when large.
func (s *Store) AddAgentEdit(ctx context.Context, toolCallID, sessionID string, turnSeq int64, path, patch string, now time.Time) error {
	inline, ref, err := s.spill(&patch)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO agent_edit (id, tool_call_id, session_id, turn_seq, path, patch, patch_ref, created_at)
	    VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, NewID("ae-"), toolCallID, sessionID, turnSeq, path, inline, ref, timefmt.RFC3339(now))
	return err
}

// ── Usage ────────────────────────────────────────────────────────────────────

// TokenTotals are cumulative token counts. The context gauges replace, never add.
type TokenTotals struct {
	Input, Output, CacheCreation, CacheRead, Reasoning int64
	ContextUsed, ContextSize                           *int64
}

// MarshalJSON writes the desktop's key order; gauges only when set.
func (t TokenTotals) MarshalJSON() ([]byte, error) {
	s := fmt.Sprintf(`{"input_tokens":%d,"output_tokens":%d,"cache_creation_tokens":%d,"cache_read_tokens":%d,"reasoning_tokens":%d`,
		t.Input, t.Output, t.CacheCreation, t.CacheRead, t.Reasoning)
	if t.ContextUsed != nil {
		s += fmt.Sprintf(`,"context_used":%d`, *t.ContextUsed)
	}
	if t.ContextSize != nil {
		s += fmt.Sprintf(`,"context_size":%d`, *t.ContextSize)
	}
	return []byte(s + "}"), nil
}

// UnmarshalJSON reads token_totals; missing fields are zero.
func (t *TokenTotals) UnmarshalJSON(b []byte) error {
	var v struct {
		Input         int64  `json:"input_tokens"`
		Output        int64  `json:"output_tokens"`
		CacheCreation int64  `json:"cache_creation_tokens"`
		CacheRead     int64  `json:"cache_read_tokens"`
		Reasoning     int64  `json:"reasoning_tokens"`
		ContextUsed   *int64 `json:"context_used"`
		ContextSize   *int64 `json:"context_size"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*t = TokenTotals{v.Input, v.Output, v.CacheCreation, v.CacheRead, v.Reasoning, v.ContextUsed, v.ContextSize}
	return nil
}

// UsageCursor reads the last cumulative report (zero when none).
func (s *Store) UsageCursor(ctx context.Context, sessionID string) (TokenTotals, error) {
	var t TokenTotals
	err := s.db.QueryRowContext(ctx, `SELECT input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, reasoning_tokens
	    FROM usage_cursor WHERE session_id = ?`, sessionID).Scan(&t.Input, &t.Output, &t.CacheCreation, &t.CacheRead, &t.Reasoning)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenTotals{}, nil
	}
	return t, err
}

// delta is one field's increase: 0 means not reported, a value at or above
// the cursor adds the difference, below it means the counter restarted.
func delta(reported, cursor int64) int64 {
	switch {
	case reported == 0:
		return 0
	case reported >= cursor:
		return reported - cursor
	default:
		return reported
	}
}

// RecordUsage applies a cumulative report (store.rs record_usage_delta,
// ADR-0005): per-field deltas against usage_cursor, added into usage_delta
// for the turn, the cursor replaced, token_totals grown, sent → pending.
func (s *Store) RecordUsage(ctx context.Context, sessionID string, turnSeq int64, model *string, rep TokenTotals, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var cur TokenTotals
	err = tx.QueryRowContext(ctx, `SELECT input_tokens, output_tokens, cache_creation_tokens, cache_read_tokens, reasoning_tokens
	    FROM usage_cursor WHERE session_id = ?`, sessionID).Scan(&cur.Input, &cur.Output, &cur.CacheCreation, &cur.CacheRead, &cur.Reasoning)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	d := TokenTotals{
		Input: delta(rep.Input, cur.Input), Output: delta(rep.Output, cur.Output),
		CacheCreation: delta(rep.CacheCreation, cur.CacheCreation), CacheRead: delta(rep.CacheRead, cur.CacheRead),
		Reasoning: delta(rep.Reasoning, cur.Reasoning),
	}
	ts := timefmt.RFC3339(now)
	if d.Input+d.Output+d.CacheCreation+d.CacheRead+d.Reasoning > 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_delta (session_id, turn_seq, model, recorded_at, input_tokens, output_tokens,
		        cache_creation_tokens, cache_read_tokens, reasoning_tokens)
		    VALUES (?, ?, COALESCE(?, (SELECT model FROM agent_session WHERE id = ?)), ?, ?, ?, ?, ?, ?)
		    ON CONFLICT (session_id, turn_seq) DO UPDATE SET input_tokens = input_tokens + excluded.input_tokens,
		        output_tokens = output_tokens + excluded.output_tokens,
		        cache_creation_tokens = cache_creation_tokens + excluded.cache_creation_tokens,
		        cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
		        reasoning_tokens = reasoning_tokens + excluded.reasoning_tokens,
		        model = COALESCE(excluded.model, model)`,
			sessionID, turnSeq, model, sessionID, ts, d.Input, d.Output, d.CacheCreation, d.CacheRead, d.Reasoning); err != nil {
			return err
		}
		next := TokenTotals{
			Input: keep(rep.Input, cur.Input), Output: keep(rep.Output, cur.Output),
			CacheCreation: keep(rep.CacheCreation, cur.CacheCreation), CacheRead: keep(rep.CacheRead, cur.CacheRead),
			Reasoning: keep(rep.Reasoning, cur.Reasoning),
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_cursor (session_id, input_tokens, output_tokens, cache_creation_tokens,
		        cache_read_tokens, reasoning_tokens) VALUES (?, ?, ?, ?, ?, ?)
		    ON CONFLICT (session_id) DO UPDATE SET input_tokens = excluded.input_tokens, output_tokens = excluded.output_tokens,
		        cache_creation_tokens = excluded.cache_creation_tokens, cache_read_tokens = excluded.cache_read_tokens,
		        reasoning_tokens = excluded.reasoning_tokens`,
			sessionID, next.Input, next.Output, next.CacheCreation, next.CacheRead, next.Reasoning); err != nil {
			return err
		}
	}
	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT token_totals FROM agent_session WHERE id = ?`, sessionID).Scan(&raw); err != nil {
		return err
	}
	var tot TokenTotals
	_ = json.Unmarshal([]byte(raw), &tot)
	tot.Input += d.Input
	tot.Output += d.Output
	tot.CacheCreation += d.CacheCreation
	tot.CacheRead += d.CacheRead
	tot.Reasoning += d.Reasoning
	if rep.ContextUsed != nil {
		tot.ContextUsed = rep.ContextUsed
	}
	if rep.ContextSize != nil {
		tot.ContextSize = rep.ContextSize
	}
	b, _ := tot.MarshalJSON()
	if _, err := tx.ExecContext(ctx, `UPDATE agent_session SET token_totals = ?, updated_at = ?, `+resync+` WHERE id = ?`,
		string(b), ts, sessionID); err != nil {
		return err
	}
	return tx.Commit()
}

// keep is the cursor's next value for a field: the report when it reported.
func keep(reported, cursor int64) int64 {
	if reported == 0 {
		return cursor
	}
	return reported
}
