package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/tryatlas/atlas-cli/internal/timefmt"
)

// The outbox: rows waiting to drain to the ingest server (SPEC §6.7,
// evidence-capture.md §7.3). Only agent_session, agent_message, tool_call and
// checkpoint rows are ever uploaded.

// Sync states (SPEC §6.3).
const (
	SyncLocal   = "local"
	SyncPending = "pending"
	SyncSent    = "sent"
	SyncFailed  = "failed"
)

// MaxAttempts retires a row to failed at the start of a drain.
const MaxAttempts = 5

// Kind names an outbox table.
type Kind string

// Outbox kinds, in drain order.
const (
	KindSession    Kind = "agent_session"
	KindMessage    Kind = "agent_message"
	KindToolCall   Kind = "tool_call"
	KindCheckpoint Kind = "checkpoint"
)

// OutSession is a pending agent_session.
type OutSession struct {
	ID, Source, NativeID, StartedAt, TokenTotals string
	Title, Agent, Model                          *string
}

// OutMessage is a pending agent_message.
type OutMessage struct {
	ID, SessionID, Role, Mode, Preview, ContentHash, CreatedAt string
	Seq, TurnSeq                                               int64
	Body, BodyRef                                              *string
}

// OutToolCall is a pending tool_call.
type OutToolCall struct {
	ID, SessionID, ToolName, Status, Locations, CreatedAt   string
	Seq, TurnSeq                                            int64
	Title, Kind, Arguments, ArgumentsRef, Result, ResultRef *string
	ResultBinary                                            bool
}

// OutCheckpoint is a pending checkpoint.
type OutCheckpoint struct {
	ID, SessionID, CommitSHA, LinkState, FilesTouched, CreatedAt string
	PatchID, Branch, AuthorName, AuthorEmail                     *string
	Insertions, Deletions                                        int64
}

// Outbox is one pass's worth of pending rows, in drain order.
type Outbox struct {
	Sessions    []OutSession
	Messages    []OutMessage
	ToolCalls   []OutToolCall
	Checkpoints []OutCheckpoint
}

// Len counts the rows.
func (o Outbox) Len() int {
	return len(o.Sessions) + len(o.Messages) + len(o.ToolCalls) + len(o.Checkpoints)
}

// notAborted keeps rows of aborted turns out of the outbox.
const notAborted = ` AND NOT EXISTS (SELECT 1 FROM turn t WHERE t.session_id = x.session_id
    AND t.turn_seq = x.turn_seq AND t.state = 'aborted')`

// RetireFailed moves pending rows that used up their attempts to failed.
func (s *Store) RetireFailed(ctx context.Context) (int64, error) {
	var n int64
	for _, t := range syncTables {
		res, err := s.db.ExecContext(ctx, `UPDATE `+t+` SET sync_state = 'failed' WHERE sync_state = 'pending' AND sync_attempts >= ?`, MaxAttempts) //nolint:gosec // G202: fixed table list
		if err != nil {
			return n, err
		}
		c, _ := res.RowsAffected()
		n += c
	}
	return n, nil
}

// Pending reads up to limit pending rows of each kind, in drain order:
// sessions by started_at, messages and tool calls by seq, checkpoints by
// created_at.
func (s *Store) Pending(ctx context.Context, limit int) (Outbox, error) {
	var o Outbox
	rows, err := s.db.QueryContext(ctx, `SELECT id, source, native_session_id, started_at, token_totals, title, agent, model
	    FROM agent_session WHERE sync_state = 'pending' ORDER BY started_at, id LIMIT ?`, limit)
	if err != nil {
		return o, err
	}
	for rows.Next() {
		var r OutSession
		if err := rows.Scan(&r.ID, &r.Source, &r.NativeID, &r.StartedAt, &r.TokenTotals, &r.Title, &r.Agent, &r.Model); err != nil {
			_ = rows.Close()
			return o, err
		}
		o.Sessions = append(o.Sessions, r)
	}
	_ = rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT id, session_id, role, mode, preview, content_hash, created_at, seq, turn_seq, body, body_ref
	    FROM agent_message x WHERE sync_state = 'pending'`+notAborted+` ORDER BY seq LIMIT ?`, limit)
	if err != nil {
		return o, err
	}
	for rows.Next() {
		var r OutMessage
		if err := rows.Scan(&r.ID, &r.SessionID, &r.Role, &r.Mode, &r.Preview, &r.ContentHash, &r.CreatedAt, &r.Seq, &r.TurnSeq, &r.Body, &r.BodyRef); err != nil {
			_ = rows.Close()
			return o, err
		}
		o.Messages = append(o.Messages, r)
	}
	_ = rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT id, session_id, tool_name, status, locations, created_at, seq, turn_seq,
	        title, kind, arguments, arguments_ref, result, result_ref, result_binary
	    FROM tool_call x WHERE sync_state = 'pending'`+notAborted+` ORDER BY seq LIMIT ?`, limit)
	if err != nil {
		return o, err
	}
	for rows.Next() {
		var r OutToolCall
		if err := rows.Scan(&r.ID, &r.SessionID, &r.ToolName, &r.Status, &r.Locations, &r.CreatedAt, &r.Seq, &r.TurnSeq,
			&r.Title, &r.Kind, &r.Arguments, &r.ArgumentsRef, &r.Result, &r.ResultRef, &r.ResultBinary); err != nil {
			_ = rows.Close()
			return o, err
		}
		o.ToolCalls = append(o.ToolCalls, r)
	}
	_ = rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT id, session_id, commit_sha, link_state, files_touched, created_at,
	        patch_id, branch, git_author_name, git_author_email, insertions, deletions
	    FROM checkpoint WHERE sync_state = 'pending' ORDER BY created_at, id LIMIT ?`, limit)
	if err != nil {
		return o, err
	}
	defer rows.Close()
	for rows.Next() {
		var r OutCheckpoint
		if err := rows.Scan(&r.ID, &r.SessionID, &r.CommitSHA, &r.LinkState, &r.FilesTouched, &r.CreatedAt,
			&r.PatchID, &r.Branch, &r.AuthorName, &r.AuthorEmail, &r.Insertions, &r.Deletions); err != nil {
			return o, err
		}
		o.Checkpoints = append(o.Checkpoints, r)
	}
	return o, rows.Err()
}

// Guard is the content a row was sent with. MarkSent only flips a row whose
// content still matches, so a change made during the upload sends again.
type Guard struct {
	Kind Kind
	ID   string
	cols string
	args []any
}

// SessionGuard guards a session by the columns its contentHash covers.
func SessionGuard(r OutSession) Guard {
	return Guard{KindSession, r.ID, `title IS ? AND agent IS ? AND model IS ? AND token_totals = ?`,
		[]any{r.Title, r.Agent, r.Model, r.TokenTotals}}
}

// MessageGuard guards a message by its content hash.
func MessageGuard(r OutMessage) Guard {
	return Guard{KindMessage, r.ID, `content_hash = ?`, []any{r.ContentHash}}
}

// ToolCallGuard guards a tool call by status and result.
func ToolCallGuard(r OutToolCall) Guard {
	return Guard{KindToolCall, r.ID, `status = ? AND result IS ? AND result_ref IS ?`, []any{r.Status, r.Result, r.ResultRef}}
}

// CheckpointGuard guards a checkpoint by commit and link state.
func CheckpointGuard(r OutCheckpoint) Guard {
	return Guard{KindCheckpoint, r.ID, `commit_sha = ? AND link_state = ?`, []any{r.CommitSHA, r.LinkState}}
}

// MarkSent flips accepted rows to sent.
func (s *Store) MarkSent(ctx context.Context, gs []Guard) error {
	for _, g := range gs {
		args := append([]any{g.ID}, g.args...)
		if _, err := s.db.ExecContext(ctx, `UPDATE `+string(g.Kind)+` SET sync_state = 'sent', sync_attempts = 0
		    WHERE id = ? AND sync_state = 'pending' AND `+g.cols, args...); err != nil { //nolint:gosec // G202: fixed kinds and columns
			return err
		}
	}
	return nil
}

// BumpAttempts counts one more failed attempt for each row.
func (s *Store) BumpAttempts(ctx context.Context, kind Kind, ids ...string) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE `+string(kind)+` SET sync_attempts = sync_attempts + 1
		    WHERE id = ? AND sync_state = 'pending'`, id); err != nil { //nolint:gosec // G202: fixed kinds
			return err
		}
	}
	return nil
}

// MarkFailed retires a row now: a poison row, a missing or oversized blob.
func (s *Store) MarkFailed(ctx context.Context, kind Kind, id string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE `+string(kind)+` SET sync_state = 'failed' WHERE id = ? AND sync_state = 'pending'`, id) //nolint:gosec // G202: fixed kinds
	return err
}

// RetryFailed requeues every failed row (atlas-cli capture retry).
func (s *Store) RetryFailed(ctx context.Context) (int64, error) {
	var n int64
	for _, t := range syncTables {
		res, err := s.db.ExecContext(ctx, `UPDATE `+t+` SET sync_state = 'pending', sync_attempts = 0 WHERE sync_state = 'failed'`) //nolint:gosec // G202: fixed table list
		if err != nil {
			return n, err
		}
		c, _ := res.RowsAffected()
		n += c
	}
	return n, nil
}

// SetDrainState records ok or not_authorized on the binding.
func (s *Store) SetDrainState(ctx context.Context, state string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE binding SET drain_state = ?, updated_at = ? WHERE id = 1`, state, timefmt.RFC3339(now))
	return err
}

// OutboxCounts counts rows by sync_state across the uploaded tables.
func (s *Store) OutboxCounts(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range syncTables {
		rows, err := s.db.QueryContext(ctx, `SELECT sync_state, COUNT(*) FROM `+t+` GROUP BY sync_state`) //nolint:gosec // G202: fixed table list
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var st string
			var n int64
			if err := rows.Scan(&st, &n); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[st] += n
		}
		_ = rows.Close()
	}
	return out, nil
}

// ReadBlob returns a stored blob; os.ErrNotExist when it is missing.
func (s *Store) ReadBlob(key string) ([]byte, error) {
	if len(key) < 2 {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(s.Root, DirName, "blobs", key[:2], filepath.Base(key)))
	if err != nil {
		return nil, err
	}
	if SHA256Hex(b) != key {
		return nil, os.ErrNotExist // a damaged blob is as good as missing
	}
	return b, nil
}

// RawJSON returns stored JSON text as a raw message, falling back to def.
func RawJSON(text, def string) json.RawMessage {
	if json.Valid([]byte(text)) {
		return json.RawMessage(text)
	}
	return json.RawMessage(def)
}
