package record

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/tryatlas/atlas-cli/internal/embed"
)

// Revision ops.
const (
	OpInsert   = "insert"
	OpReplace  = "replace"
	OpMerge    = "merge"
	OpFold     = "fold"
	OpImport   = "import"
	OpForget   = "forget"
	OpArchive  = "archive"
	OpPromote  = "promote"
	OpFeedback = "feedback"
)

// Outcomes of a direct write.
const (
	Inserted = "inserted"
	Replaced = "replaced"
	Merged   = "merged"
)

// retryWindow: an active entry restated by the same source and session
// within this window, with no new evidence, is a retry.
const retryWindow = 5 * 60 * 1000

// Conflict is the refusal of a keyed replace by another writer.
type Conflict struct {
	ID      int64
	Rev     int64
	By      string
	Content string
}

func (c *Conflict) Error() string {
	return fmt.Sprintf("memory %d is at revision %d (by %s): %q. Read both versions, write one that keeps what is still true, and pass expected_revision=%d to replace it.",
		c.ID, c.Rev, c.By, c.Content, c.Rev)
}

// Write is one direct write (memory_remember, imports).
type Write struct {
	Kind       string
	Key        string
	Content    string
	Source     string
	Agent      string
	Session    string
	Confidence float64
	Evidence   []Citation
	At         int64 // ms since epoch

	Guarded          bool   // agent path: a keyed replace needs expected_revision or same source+session
	ExpectedRevision *int64 //
	LogEvent         bool   // tool path: an insert or replace is also an event of the entry's kind
	Op               string // override the revision op (OpImport); default insert/replace/merge
	Note             string // revision note, outside the chain
	OnlyNew          bool   // imports: a matching entry is left alone (no merge)
}

// Remember applies a direct write and returns the outcome and the entry.
// Every text is cleaned and redacted first.
func (s *Store) Remember(ctx context.Context, w Write) (string, *Entry, error) {
	var err error
	if w.Content, err = RedactText(w.Content); err != nil {
		return "", nil, err
	}
	if w.Key, err = RedactText(w.Key); err != nil {
		return "", nil, err
	}
	if w.Note != "" {
		if w.Note, err = RedactText(w.Note); err != nil {
			return "", nil, err
		}
	}
	if strings.TrimSpace(w.Content) == "" {
		return "", nil, errors.New("nothing to remember: content is empty")
	}
	hash := ContentHash(w.Content)
	// A durable write is embedded before the transaction (as the app does,
	// outside its lock); without a model it is stored all the same.
	var vec []float32
	if IsDurable(w.Kind) {
		vec = vectorFor(ctx, s.db, strings.TrimSpace(w.Content))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var cur *Entry
	if w.Key != "" {
		cur, err = scanOne(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE kind = ? AND key = ? ORDER BY id LIMIT 1`, w.Kind, w.Key))
		if err == nil && cur == nil {
			// No entry has this key: the same text written without a key is
			// the same memory, and the keyless survivor adopts the key.
			cur, err = scanOne(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE kind = ? AND key = '' AND content_hash = ? ORDER BY id LIMIT 1`, w.Kind, hash))
		}
	} else {
		cur, err = scanOne(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE kind = ? AND content_hash = ? ORDER BY id LIMIT 1`, w.Kind, hash))
	}
	if err != nil {
		return "", nil, err
	}
	nearDup := false
	if cur == nil && vec != nil && !w.OnlyNew {
		// No key or text match: a stored memory of the kind that says the
		// same thing takes the write as a merge.
		if cur, err = nearDuplicate(ctx, tx, w, vec); err != nil {
			return "", nil, err
		}
		nearDup = cur != nil
	}

	var outcome string
	var id int64
	switch {
	case cur == nil:
		outcome = Inserted
		res, err := tx.ExecContext(ctx, `INSERT INTO entries (kind, key, content, status, source, agent, session,
		        confidence, created_at, updated_at, uses, content_hash, seq, rev, state, scope, evidence, changed_by)
		    VALUES (?, ?, ?, '', ?, ?, ?, ?, ?, ?, 0, ?, NULL, 0, 'active', 'repo', ?, ?)`,
			w.Kind, w.Key, w.Content, w.Source, w.Agent, w.Session, w.Confidence, w.At, w.At, hash,
			evidenceJSON(w.Evidence), w.Session)
		if err != nil {
			return "", nil, err
		}
		id, _ = res.LastInsertId()

	case w.OnlyNew:
		return "", cur, nil

	case cur.ContentHash == hash || nearDup:
		if isRetry(cur, w) {
			return Merged, cur, nil
		}
		outcome, id = Merged, cur.ID
		key := cur.Key
		if key == "" {
			key = w.Key // a keyless survivor adopts the key
		}
		ev := unionEvidence(cur.Evidence, w.Evidence)
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET key = ?, uses = uses + 1, confidence = MAX(confidence, ?),
		        updated_at = MAX(updated_at, ?), evidence = ? WHERE id = ?`,
			key, w.Confidence, w.At, evidenceJSON(ev), id); err != nil {
			return "", nil, err
		}

	default:
		if w.Guarded && w.Key != "" {
			allowed := (w.ExpectedRevision != nil && *w.ExpectedRevision == cur.Rev) ||
				(w.ExpectedRevision == nil && cur.Source == w.Source && cur.Session == w.Session)
			if !allowed {
				return "", nil, &Conflict{ID: cur.ID, Rev: cur.Rev, By: cur.By(), Content: cur.Content}
			}
		}
		outcome, id = Replaced, cur.ID
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET content = ?, source = ?, agent = ?, session = ?,
		        confidence = ?, updated_at = ?, content_hash = ?, evidence = ? WHERE id = ?`,
			w.Content, w.Source, w.Agent, w.Session, w.Confidence, w.At, hash, evidenceJSON(w.Evidence), id); err != nil {
			return "", nil, err
		}
	}

	if w.LogEvent && outcome != Merged {
		seq, err := appendEvent(ctx, tx, w.At, w.Kind, w.Key, w.Agent, w.Session, map[string]any{"text": w.Content})
		if err != nil {
			return "", nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET seq = ? WHERE id = ?`, seq, id); err != nil {
			return "", nil, err
		}
	}
	if vec != nil && (outcome == Inserted || outcome == Replaced) {
		if err := putVector(ctx, tx, w.Content, vec); err != nil {
			return "", nil, err
		}
	}
	op := w.Op
	if op == "" {
		op = map[string]string{Inserted: OpInsert, Replaced: OpReplace, Merged: OpMerge}[outcome]
	}
	e, err := afterWrite(ctx, tx, id, op, writer{w.Source, w.Agent, w.Session}, w.Session, w.At, w.Note, "")
	if err != nil {
		return "", nil, err
	}
	return outcome, e, tx.Commit()
}

// isRetry: an active entry restated by the same source and session within
// five minutes with no new evidence. A candidate's restatement is never a retry.
func isRetry(cur *Entry, w Write) bool {
	if cur.State != StateActive || cur.Source != w.Source || cur.Session != w.Session || w.At-cur.UpdatedAt > retryWindow {
		return false
	}
	have := map[string]bool{}
	for _, c := range cur.Evidence {
		have[c.Path+"\x00"+c.Hash] = true
	}
	for _, c := range w.Evidence {
		if !have[c.Path+"\x00"+c.Hash] {
			return false
		}
	}
	return true
}

func unionEvidence(a, b []Citation) []Citation {
	seen := map[string]bool{}
	var out []Citation
	for _, c := range append(append([]Citation(nil), a...), b...) {
		k := c.Path + "\x00" + c.Hash
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, c)
	}
	if len(out) > MaxCitations {
		out = out[:MaxCitations]
	}
	return out
}

func evidenceJSON(ev []Citation) string {
	if len(ev) == 0 {
		return "[]"
	}
	b, _ := json.Marshal(ev)
	return string(b)
}

func scanOne(r *sql.Row) (*Entry, error) {
	e, err := scanEntry(r)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return e, err
}

// writer names who made a revision.
type writer struct{ source, agent, session string }

// afterWrite settles the entry's state, sets changed_by, appends a sealed
// revision holding the entry as it now stands, points entries.rev at it and
// rewrites the FTS row — all in the caller's transaction (ADR-0017).
// forceState, when set, overrides the settled state (archived).
func afterWrite(ctx context.Context, tx *sql.Tx, id int64, op string, by writer, changedBy string, at int64, note, forceState string) (*Entry, error) {
	if forceState != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET state = ?, changed_by = ? WHERE id = ?`, forceState, changedBy, id); err != nil {
			return nil, err
		}
	} else if _, err := tx.ExecContext(ctx, `UPDATE entries SET state = CASE
	        WHEN state = 'archived' THEN 'archived' WHEN confidence < ? THEN 'candidate' ELSE 'active' END,
	        changed_by = ? WHERE id = ?`, Trusted, changedBy, id); err != nil {
		return nil, err
	}
	e, err := scanEntry(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}
	rev, err := appendRevision(ctx, tx, e, op, by, at, note, e.State)
	if err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE entries SET rev = ? WHERE id = ?`, rev, id); err != nil {
		return nil, err
	}
	e.Rev = rev
	if err := writeFTS(ctx, tx, e); err != nil {
		return nil, err
	}
	return e, nil
}

// appendRevision inserts a revision row and seals it into the chain.
func appendRevision(ctx context.Context, tx *sql.Tx, e *Entry, op string, by writer, at int64, note, state string) (int64, error) {
	ev := evidenceJSON(e.Evidence)
	res, err := tx.ExecContext(ctx, `INSERT INTO revisions (entry_id, op, kind, key, content, content_hash, status,
	        source, agent, session, confidence, state, scope, evidence, at, chain, note)
	    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, x'', ?)`,
		e.ID, op, e.Kind, e.Key, e.Content, e.ContentHash, e.Status, by.source, by.agent, by.session,
		e.Confidence, state, e.Scope, ev, at, note)
	if err != nil {
		return 0, err
	}
	rev, _ := res.LastInsertId()
	var prev []byte
	err = tx.QueryRowContext(ctx, `SELECT chain FROM revisions WHERE rev < ? ORDER BY rev DESC LIMIT 1`, rev).Scan(&prev)
	if errors.Is(err, sql.ErrNoRows) {
		prev = genesis
	} else if err != nil {
		return 0, err
	}
	row, err := rowBytes([]any{rev, e.ID, op, e.Kind, e.Key, e.Content, e.ContentHash, e.Status,
		by.source, by.agent, by.session, e.Confidence, state, e.Scope, ev, at})
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE revisions SET chain = ? WHERE rev = ?`, link(prev, row), rev); err != nil {
		return 0, err
	}
	return rev, nil
}

// writeFTS rewrites an entry's search row (contentless table: delete, insert).
func writeFTS(ctx context.Context, tx *sql.Tx, e *Entry) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM entries_fts WHERE rowid = ?`, e.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO entries_fts (rowid, key, content) VALUES (?, ?, ?)`, e.ID, e.Key, e.Content)
	return err
}

// appendEvent writes an event row; seq is MAX(seq)+1 inside the transaction.
// Every string in the payload is redacted.
func appendEvent(ctx context.Context, tx *sql.Tx, at int64, kind, key, agent, session string, payload map[string]any) (int64, error) {
	for k, v := range payload {
		if s, ok := v.(string); ok {
			r, err := RedactText(s)
			if err != nil {
				return 0, err
			}
			payload[k] = r
		}
	}
	rk, err := RedactText(key)
	if err != nil {
		return 0, err
	}
	body, err := compactJSON(payload)
	if err != nil {
		return 0, err
	}
	var seq int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM events`).Scan(&seq); err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO events (seq, ts, kind, key, agent, session, payload) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		seq, at, kind, rk, agent, session, body)
	return seq, err
}

// compactJSON writes payloads the way serde_json does: compact, keys in the
// order the desktop writes them ("path" before "summary", "text" before
// "status"), no HTML escaping.
func compactJSON(m map[string]any) (string, error) {
	order := []string{"text", "status", "path", "summary"}
	var b strings.Builder
	b.WriteByte('{')
	n := 0
	write := func(k string, v any) error {
		if n > 0 {
			b.WriteByte(',')
		}
		n++
		kj, _ := json.Marshal(k)
		b.Write(kj)
		b.WriteByte(':')
		vj, err := marshalNoEscape(v)
		if err != nil {
			return err
		}
		b.Write(vj)
		return nil
	}
	done := map[string]bool{}
	for _, k := range order {
		if v, ok := m[k]; ok {
			if err := write(k, v); err != nil {
				return "", err
			}
			done[k] = true
		}
	}
	for k, v := range m {
		if !done[k] {
			if err := write(k, v); err != nil {
				return "", err
			}
		}
	}
	b.WriteByte('}')
	return b.String(), nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(buf.String(), "\n")), nil
}

// ── Forget, feedback, archive ────────────────────────────────────────────────

// Forget writes a tombstone revision, deletes the entry, retracts the events
// that carried it and records it in forgotten. false when there is no entry.
func (s *Store) Forget(ctx context.Context, id int64, session string, at int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	e, err := scanOne(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE id = ?`, id))
	if err != nil || e == nil {
		return false, err
	}
	if _, err := appendRevision(ctx, tx, e, OpForget, writer{e.Source, e.Agent, session}, at, "", StateTombstoned); err != nil {
		return false, err
	}
	seqs, err := carryingEvents(ctx, tx, e)
	if err != nil {
		return false, err
	}
	for _, q := range seqs {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO retracted_events (seq) VALUES (?)`, q); err != nil {
			return false, err
		}
	}
	sj, _ := json.Marshal(seqs)
	if seqs == nil {
		sj = []byte("[]")
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO forgotten (id, kind, session, at, seqs) VALUES (?, ?, ?, ?, ?)`,
		e.ID, e.Kind, session, at, string(sj)); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entries_fts WHERE rowid = ?`, e.ID); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE id = ?`, e.ID); err != nil {
		return false, err
	}
	// Its vectors go too, for every text it ever had (record.rs purge).
	if err := dropVectors(ctx, tx, e); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// dropVectors deletes the cached vectors of an entry's current and earlier
// texts, under every model the cache holds.
func dropVectors(ctx context.Context, tx *sql.Tx, e *Entry) error {
	texts := map[string]bool{e.Content: true}
	rows, err := tx.QueryContext(ctx, `SELECT content FROM revisions WHERE entry_id = ?`, e.ID)
	if err == nil {
		for rows.Next() {
			var c string
			if rows.Scan(&c) == nil {
				texts[c] = true
			}
		}
		_ = rows.Close()
	}
	mrows, err := tx.QueryContext(ctx, `SELECT DISTINCT model FROM embed_cache`)
	if err != nil {
		return err
	}
	var models []string
	for mrows.Next() {
		var m string
		if mrows.Scan(&m) == nil {
			models = append(models, m)
		}
	}
	_ = mrows.Close()
	for _, m := range models {
		for t := range texts {
			if _, err := tx.ExecContext(ctx, `DELETE FROM embed_cache WHERE key = ?`, embed.CacheKey(m, t)); err != nil {
				return err
			}
		}
	}
	return nil
}

// carryingEvents are the entry's own event and the events of its kind with
// its key (keyed) or its content hash (keyless).
func carryingEvents(ctx context.Context, tx *sql.Tx, e *Entry) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT seq, key, payload FROM events WHERE kind = ? ORDER BY seq`, eventKind(e.Kind))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var seq int64
		var key, payload string
		if err := rows.Scan(&seq, &key, &payload); err != nil {
			return nil, err
		}
		var p struct {
			Text    string `json:"text"`
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal([]byte(payload), &p)
		text := p.Text
		if e.Kind == KindFileChanged {
			text = p.Summary
		}
		if (e.Seq != nil && *e.Seq == seq) || (e.Key != "" && key == e.Key) || (e.Key == "" && ContentHash(text) == e.ContentHash) {
			out = append(out, seq)
		}
	}
	return out, rows.Err()
}

func eventKind(kind string) string {
	if kind == KindPlan {
		return "plan_set"
	}
	return kind
}

// Verdicts.
const (
	VerdictUseful = "useful"
	VerdictWrong  = "wrong"
	VerdictStale  = "stale"
)

// Feedback records a verdict on an entry (SPEC §5.3). nil entry when the id
// is unknown.
func (s *Store) Feedback(ctx context.Context, id int64, verdict, note, by, session string, at int64) (*Entry, error) {
	if verdict != VerdictUseful && verdict != VerdictWrong && verdict != VerdictStale {
		return nil, fmt.Errorf("unknown verdict `%s`; one of useful, wrong, stale", verdict)
	}
	note, err := RedactText(note)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	e, err := scanOne(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE id = ?`, id))
	if err != nil || e == nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO feedback (entry_id, verdict, note, by, session, at) VALUES (?, ?, ?, ?, ?, ?)`,
		id, verdict, note, by, session, at); err != nil {
		return nil, err
	}
	w := writer{source: by, agent: by, session: session}
	switch verdict {
	case VerdictUseful:
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET uses = uses + 1, last_used_at = ? WHERE id = ?`, at, id); err != nil {
			return nil, err
		}
		if e.State == StateCandidate && e.Session != session {
			if _, err := tx.ExecContext(ctx, `UPDATE entries SET confidence = MAX(confidence, ?), updated_at = ? WHERE id = ?`, Promoted, at, id); err != nil {
				return nil, err
			}
			if e, err = afterWrite(ctx, tx, id, OpPromote, w, session, at, "", ""); err != nil {
				return nil, err
			}
		} else if e, err = scanOne(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE id = ?`, id)); err != nil {
			return nil, err
		}
	case VerdictWrong:
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET confidence = MIN(confidence, ?), updated_at = ? WHERE id = ?`, Candidate, at, id); err != nil {
			return nil, err
		}
		if e, err = afterWrite(ctx, tx, id, OpFeedback, w, session, at, "", StateArchived); err != nil {
			return nil, err
		}
	case VerdictStale:
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET confidence = MIN(confidence, ?), updated_at = ? WHERE id = ?`, Candidate, at, id); err != nil {
			return nil, err
		}
		if e, err = afterWrite(ctx, tx, id, OpFeedback, w, session, at, "", ""); err != nil {
			return nil, err
		}
	}
	return e, tx.Commit()
}

// Archive takes an entry out of briefings and default search. changed_by is
// cleared so every session hears it.
func (s *Store) Archive(ctx context.Context, id int64, by, session string, at int64) (*Entry, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE entries SET updated_at = ? WHERE id = ?`, at, id); err != nil {
		return nil, err
	}
	e, err := afterWrite(ctx, tx, id, OpArchive, writer{by, by, session}, "", at, "", StateArchived)
	if err != nil {
		return nil, err
	}
	return e, tx.Commit()
}

// ── Sessions ─────────────────────────────────────────────────────────────────

// SessionStart logs a session_start event and a sessions row, so memory_why
// and memory_changes can name the session.
func (s *Store) SessionStart(ctx context.Context, session, agent string, at int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	_ = tx.QueryRowContext(ctx, `SELECT count(*) FROM sessions WHERE session_id = ?`, session).Scan(&exists)
	if exists > 0 {
		return nil
	}
	if _, err := appendEvent(ctx, tx, at, "session_start", "", agent, session, map[string]any{}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (session_id, agent, started_at, ended_at) VALUES (?, ?, ?, NULL)`,
		session, agent, at); err != nil {
		return err
	}
	return tx.Commit()
}

// SessionEnd logs session_end and stamps ended_at.
func (s *Store) SessionEnd(ctx context.Context, session, agent string, at int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := appendEvent(ctx, tx, at, "session_end", "", agent, session, map[string]any{}); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET ended_at = ? WHERE session_id = ?`, at, session); err != nil {
		return err
	}
	return tx.Commit()
}

// ── Folds (capture and lifecycle) ────────────────────────────────────────────

// Event is a captured event folded into working or durable memory.
type Event struct {
	Kind    string // plan_set, file_changed, decision, fact, failure, architecture, preference
	Key     string // file_changed: the path
	Text    string // plan text, file summary, or the durable text
	Status  string // plan_set: active | done | abandoned
	Agent   string
	Session string
	At      int64
}

// Fold appends the event and folds it into entries (SPEC §5.3 "Event folds"):
// no near-duplicate matching; several matches collapse onto the oldest id;
// folded rows get confidence 1.0 and op fold.
func (s *Store) Fold(ctx context.Context, ev Event) (*Entry, error) {
	kind := ev.Kind
	key := ev.Key
	payload := map[string]any{"text": ev.Text}
	switch ev.Kind {
	case "plan_set":
		kind, key = KindPlan, "plan"
		if ev.Status == "" {
			ev.Status = "active"
		}
		payload["status"] = ev.Status
	case KindFileChanged:
		payload = map[string]any{"path": ev.Key, "summary": ev.Text}
	case KindDecision, KindFact, KindFailure, KindArchitecture, KindPreference:
	default:
		return nil, fmt.Errorf("cannot fold an event of kind %q", ev.Kind)
	}
	text, err := RedactText(ev.Text)
	if err != nil {
		return nil, err
	}
	if key, err = RedactText(key); err != nil {
		return nil, err
	}
	hash := ContentHash(text)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	seq, err := appendEvent(ctx, tx, ev.At, eventKind(kind), key, ev.Agent, ev.Session, payload)
	if err != nil {
		return nil, err
	}

	var where string
	var args []any
	switch kind {
	case KindPlan:
		where, args = `kind = 'plan'`, nil
	case KindDecision:
		where, args = `kind = 'decision' AND ((? <> '' AND key = ?) OR content_hash = ?)`, []any{key, key, hash}
	case KindFileChanged:
		where, args = `kind = 'file_changed' AND key = ?`, []any{key}
	case KindFact, KindPreference:
		where, args = `kind = ? AND content_hash = ?`, []any{kind, hash}
	default: // failure, architecture: hash, or content equal to the key (a carried-over quirk)
		where, args = `kind = ? AND (content_hash = ? OR (? <> '' AND content = ?))`, []any{kind, hash, key, key}
	}
	matches, err := queryTx(ctx, tx, `WHERE `+where+` ORDER BY id`, args...)
	if err != nil {
		return nil, err
	}
	by := writer{ev.Agent, ev.Agent, ev.Session}

	if kind == KindPlan && (ev.Status == "done" || ev.Status == "abandoned") {
		for _, m := range matches {
			if err := tombstone(ctx, tx, m, OpFold, by, ev.At); err != nil {
				return nil, err
			}
		}
		return nil, tx.Commit()
	}

	status := ""
	if kind == KindPlan {
		status = ev.Status
	}
	var id int64
	if len(matches) == 0 {
		res, err := tx.ExecContext(ctx, `INSERT INTO entries (kind, key, content, status, source, agent, session,
		        confidence, created_at, updated_at, uses, content_hash, seq, rev, state, scope, evidence, changed_by)
		    VALUES (?, ?, ?, ?, ?, ?, ?, 1.0, ?, ?, 0, ?, ?, 0, 'active', 'repo', '[]', ?)`,
			kind, key, text, status, ev.Agent, ev.Agent, ev.Session, ev.At, ev.At, hash, seq, ev.Session)
		if err != nil {
			return nil, err
		}
		id, _ = res.LastInsertId()
	} else {
		id = matches[0].ID
		for _, m := range matches[1:] {
			if err := tombstone(ctx, tx, m, OpFold, by, ev.At); err != nil {
				return nil, err
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE entries SET key = CASE WHEN ? <> '' THEN ? ELSE key END, content = ?,
		        status = ?, source = ?, agent = ?, session = ?, confidence = 1.0, updated_at = ?, content_hash = ?, seq = ?
		    WHERE id = ?`, key, key, text, status, ev.Agent, ev.Agent, ev.Session, ev.At, hash, seq, id); err != nil {
			return nil, err
		}
	}
	e, err := afterWrite(ctx, tx, id, OpFold, by, ev.Session, ev.At, "", "")
	if err != nil {
		return nil, err
	}
	return e, tx.Commit()
}

// tombstone removes an entry from the view with a tombstone revision.
func tombstone(ctx context.Context, tx *sql.Tx, e *Entry, op string, by writer, at int64) error {
	if _, err := appendRevision(ctx, tx, e, op, by, at, "", StateTombstoned); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entries_fts WHERE rowid = ?`, e.ID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM entries WHERE id = ?`, e.ID)
	return err
}

func queryTx(ctx context.Context, tx *sql.Tx, q string, args ...any) ([]*Entry, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+entryColumns+` FROM entries `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TouchUsed stamps last_used_at (memory_get and every search hit). Not revisioned.
func (s *Store) TouchUsed(ctx context.Context, ids []int64, at int64) error {
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `UPDATE entries SET last_used_at = ? WHERE id = ?`, at, id); err != nil {
			return err
		}
	}
	return nil
}
