package record

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode"
)

// List returns the newest entries of one kind or of every kind, by
// updated_at desc, id desc. Archived entries are included only when asked.
func (s *Store) List(ctx context.Context, kind string, limit int) ([]*Entry, error) {
	if kind != "" {
		return s.queryEntries(ctx, `WHERE kind = ? AND state <> 'archived' ORDER BY updated_at DESC, id DESC LIMIT ?`, kind, limit)
	}
	return s.queryEntries(ctx, `WHERE state <> 'archived' ORDER BY updated_at DESC, id DESC LIMIT ?`, limit)
}

// Active returns every active entry of the given kinds, newest first.
func (s *Store) Active(ctx context.Context, kinds ...string) ([]*Entry, error) {
	q, args := kindFilter(kinds)
	return s.queryEntries(ctx, `WHERE state = 'active'`+q+` ORDER BY updated_at DESC, id DESC`, args...)
}

func kindFilter(kinds []string) (string, []any) {
	if len(kinds) == 0 {
		return "", nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
	args := make([]any, len(kinds))
	for i, k := range kinds {
		args[i] = k
	}
	return ` AND kind IN (` + ph + `)`, args
}

// Revision is one row of an entry's history.
type Revision struct {
	Rev        int64
	EntryID    int64
	Op         string
	Kind       string
	Key        string
	Content    string
	Source     string
	Agent      string
	Session    string
	Confidence float64
	State      string
	At         int64
	Note       string
}

// By names the writer of a revision.
func (r Revision) By() string {
	if r.Agent != "" {
		return r.Agent
	}
	return r.Source
}

// SourceURI names a writer on the wire: atlas-user for the user, the bare
// agent id when there is no session, else atlas-session:<agent>/<session>.
func SourceURI(source, agent, session string) string {
	switch {
	case source == "user":
		return "atlas-user"
	case agent == "" && session == "":
		return source
	case session == "":
		return agent
	case agent == "":
		return "atlas-session:" + source + "/" + session
	}
	return "atlas-session:" + agent + "/" + session
}

// History returns every revision of an entry, oldest first (including a
// tombstone if it was forgotten).
func (s *Store) History(ctx context.Context, id int64) ([]Revision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rev, entry_id, op, kind, key, content, source, agent, session,
	        confidence, state, at, note FROM revisions WHERE entry_id = ? ORDER BY rev`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Revision
	for rows.Next() {
		var r Revision
		if err := rows.Scan(&r.Rev, &r.EntryID, &r.Op, &r.Kind, &r.Key, &r.Content, &r.Source, &r.Agent,
			&r.Session, &r.Confidence, &r.State, &r.At, &r.Note); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Sources returns each entry's distinct writer URIs, oldest first.
func (s *Store) Sources(ctx context.Context, ids []int64) (map[int64][]string, error) {
	out := map[int64][]string{}
	if len(ids) == 0 {
		return out, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT entry_id, source, agent, session FROM revisions
	    WHERE entry_id IN (`+ph+`) AND state <> 'tombstoned' ORDER BY rev`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[int64]map[string]bool{}
	for rows.Next() {
		var id int64
		var src, agent, session string
		if err := rows.Scan(&id, &src, &agent, &session); err != nil {
			return nil, err
		}
		u := SourceURI(src, agent, session)
		if seen[id] == nil {
			seen[id] = map[string]bool{}
		}
		if !seen[id][u] {
			seen[id][u] = true
			out[id] = append(out[id], u)
		}
	}
	return out, rows.Err()
}

// Conflicts returns the ids each entry is linked to by contradicts.
func (s *Store) Conflicts(ctx context.Context, id int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT CASE WHEN a = ? THEN b ELSE a END FROM links
	    WHERE rel = 'contradicts' AND (a = ? OR b = ?) ORDER BY 1`, id, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var x int64
		if err := rows.Scan(&x); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ── Search ───────────────────────────────────────────────────────────────────

// FTSQuery turns free text into an FTS5 query: each alphanumeric word
// lower-cased, quoted and OR-ed, dropping words under 3 characters when
// longer ones exist.
func FTSQuery(q string) string {
	words := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	long := false
	for _, w := range words {
		if len([]rune(w)) >= 3 {
			long = true
			break
		}
	}
	var kept []string
	seen := map[string]bool{}
	for _, w := range words {
		if (long && len([]rune(w)) < 3) || seen[w] {
			continue
		}
		seen[w] = true
		kept = append(kept, `"`+w+`"`)
	}
	return strings.Join(kept, " OR ")
}

// Hit is a search result with its BM25 rank (1-based; 0 when not from BM25).
type Hit struct {
	Entry *Entry
	BM25  int // 1-based rank in the keyword leg; 0 when it did not find it
	Dense int // 1-based rank in the meaning leg
}

// rrfK is the reciprocal-rank-fusion constant.
const rrfK = 60

// Search is the app's hybrid search (record.rs search_explained): the
// keyword leg (FTS5 BM25, 100 deep) and the meaning leg (cosine over cached
// vectors, 100 deep, at least 0.35) fused by reciprocal rank (k 60, weight 1
// each), then priors that only reorder what was found: trust (active before
// candidate, then confidence, then id; weight 0.5) and recency (the later of
// last use and update; weight 0.3). Archived entries are excluded. Without a
// model the meaning leg is empty. An empty query returns the newest entries.
func (s *Store) Search(ctx context.Context, query string, kinds []string, limit int) ([]Hit, error) {
	kq, kargs := kindFilter(kinds)
	fq := FTSQuery(query)
	if strings.TrimSpace(query) == "" {
		es, err := s.queryEntries(ctx, `WHERE state <> 'archived'`+kq+` ORDER BY updated_at DESC, id DESC LIMIT ?`, append(kargs, limit)...)
		hits := make([]Hit, len(es))
		for i, e := range es {
			hits[i] = Hit{Entry: e}
		}
		return hits, err
	}
	var bmIDs []int64
	if fq != "" {
		rows, err := s.db.QueryContext(ctx, `SELECT rowid FROM entries_fts WHERE entries_fts MATCH ? ORDER BY rank LIMIT ?`, fq, legDepth)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			bmIDs = append(bmIDs, id)
		}
		_ = rows.Close()
	}
	var denseIDs []int64
	if v := vectorFor(ctx, s.db, strings.TrimSpace(query)); v != nil {
		var err error
		if denseIDs, err = s.denseIDs(ctx, v); err != nil {
			return nil, err
		}
	}
	bm, dn := map[int64]int{}, map[int64]int{}
	var ids []int64
	for i, id := range bmIDs {
		bm[id] = i + 1
		ids = append(ids, id)
	}
	for i, id := range denseIDs {
		dn[id] = i + 1
		if bm[id] == 0 {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+len(kargs))
	for _, id := range ids {
		args = append(args, id)
	}
	found, err := s.queryEntries(ctx, `WHERE id IN (`+ph+`) AND state <> 'archived'`+kq, append(args, kargs...)...)
	if err != nil {
		return nil, err
	}
	score := map[int64]float64{}
	for _, e := range found {
		if r := bm[e.ID]; r > 0 {
			score[e.ID] += 1.0 / float64(rrfK+r)
		}
		if r := dn[e.ID]; r > 0 {
			score[e.ID] += 1.0 / float64(rrfK+r)
		}
	}
	trust := append([]*Entry(nil), found...)
	sort.SliceStable(trust, func(i, j int) bool {
		a, b := trust[i], trust[j]
		if (a.State == StateCandidate) != (b.State == StateCandidate) {
			return b.State == StateCandidate
		}
		if a.Confidence != b.Confidence {
			return a.Confidence > b.Confidence
		}
		return a.ID < b.ID
	})
	for i, e := range trust {
		score[e.ID] += 0.5 / float64(rrfK+i+1)
	}
	lastTouch := func(e *Entry) int64 {
		t := e.UpdatedAt
		if e.LastUsedAt != nil && *e.LastUsedAt > t {
			t = *e.LastUsedAt
		}
		return t
	}
	recent := append([]*Entry(nil), found...)
	sort.SliceStable(recent, func(i, j int) bool {
		if lastTouch(recent[i]) != lastTouch(recent[j]) {
			return lastTouch(recent[i]) > lastTouch(recent[j])
		}
		return recent[i].ID < recent[j].ID
	})
	for i, e := range recent {
		score[e.ID] += 0.3 / float64(rrfK+i+1)
	}
	sort.SliceStable(found, func(i, j int) bool {
		if score[found[i].ID] != score[found[j].ID] {
			return score[found[i].ID] > score[found[j].ID]
		}
		return found[i].ID < found[j].ID
	})
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}
	hits := make([]Hit, len(found))
	for i, e := range found {
		hits[i] = Hit{Entry: e, BM25: bm[e.ID], Dense: dn[e.ID]}
	}
	return hits, nil
}

// ── Changes ──────────────────────────────────────────────────────────────────

// ChangesPerKind caps one page of memory_changes.
const ChangesPerKind = 8

// Changes are entries other sessions wrote after since, at most 8 per kind
// per page. When a kind had more, More is set and SyncedTo stops just before
// the first entry left out, so the next page starts there.
type Changes struct {
	Since     int64
	SyncedTo  int64
	More      bool
	Forgotten []int64
	Entries   []*Entry
}

// ChangesSince pages through other sessions' changes, oldest first per kind
// and newest first in the result.
func (s *Store) ChangesSince(ctx context.Context, since int64, ownSession string) (*Changes, error) {
	all, err := s.queryEntries(ctx, `WHERE updated_at > ? AND changed_by <> ? ORDER BY updated_at ASC, id ASC`, since, ownSession)
	if err != nil {
		return nil, err
	}
	c := &Changes{Since: since, SyncedTo: since}
	perKind := map[string]int{}
	stop := int64(-1)
	for _, e := range all {
		if perKind[e.Kind] >= ChangesPerKind {
			c.More = true
			if stop < 0 || e.UpdatedAt-1 < stop {
				stop = e.UpdatedAt - 1
			}
			continue
		}
		perKind[e.Kind]++
		c.Entries = append(c.Entries, e)
		if e.UpdatedAt > c.SyncedTo {
			c.SyncedTo = e.UpdatedAt
		}
	}
	if stop >= 0 && stop < c.SyncedTo {
		c.SyncedTo = stop
	}
	sort.SliceStable(c.Entries, func(i, j int) bool { return c.Entries[i].UpdatedAt > c.Entries[j].UpdatedAt })
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM forgotten WHERE at > ? AND session <> ? ORDER BY id`, since, ownSession)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		c.Forgotten = append(c.Forgotten, id)
	}
	return c, rows.Err()
}

// ── Episodes (handoff notes) ─────────────────────────────────────────────────

// LatestHandoff returns the newest handoff note left by another session.
func (s *Store) LatestHandoff(ctx context.Context, ownSession string) (json.RawMessage, error) {
	var note string
	err := s.db.QueryRowContext(ctx, `SELECT note FROM episodes WHERE session <> ? ORDER BY ended_at DESC LIMIT 1`, ownSession).Scan(&note)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid([]byte(note)) {
		return nil, nil
	}
	return json.RawMessage(note), nil
}

// EntriesBySessions returns entries whose revisions were written by one of the sessions.
func (s *Store) EntriesBySessions(ctx context.Context, sessions []string, limit int) ([]*Entry, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(sessions)), ",")
	args := make([]any, 0, len(sessions)+1)
	for _, x := range sessions {
		args = append(args, x)
	}
	return s.queryEntries(ctx, `WHERE id IN (SELECT entry_id FROM revisions WHERE session IN (`+ph+`))
	    AND state <> 'archived' ORDER BY updated_at DESC LIMIT ?`, append(args, limit)...)
}

// EntriesCiting returns entries whose evidence cites a repository-relative path.
func (s *Store) EntriesCiting(ctx context.Context, rel string, limit int) ([]*Entry, error) {
	es, err := s.queryEntries(ctx, `WHERE evidence <> '[]' AND state <> 'archived' ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	var out []*Entry
	for _, e := range es {
		for _, c := range e.Evidence {
			if c.Path == rel {
				out = append(out, e)
				break
			}
		}
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// Date is the UTC calendar date of a ms timestamp.
func Date(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02") }
