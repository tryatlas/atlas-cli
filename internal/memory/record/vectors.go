package record

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"unicode"

	"github.com/tryatlas/atlas-cli/internal/embed"
)

// VectorSource embeds text; the record caches what it returns.
type VectorSource interface {
	Embed(text string) []float32
}

// Vectors returns the embedder, or nil when the model is not on this
// machine: then search is keyword-only and dedup is key-or-hash only, as
// in the app. Tests replace it.
var Vectors = func() VectorSource {
	e, err := embed.Default()
	if err != nil {
		return nil
	}
	return e
}

// VectorModel is the name vectors are cached under.
const VectorModel = embed.ModelName

// NearDuplicate is the cosine at which a new durable memory merges into a
// stored one of its kind (record.rs NEAR_DUPLICATE).
const NearDuplicate = 0.92

// Search legs (record.rs LEG_DEPTH, MIN_SIMILARITY).
const (
	legDepth      = 100
	minSimilarity = 0.35
)

type vecDB interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// cachedVector reads text's vector from embed_cache.
func cachedVector(ctx context.Context, q vecDB, text string) []float32 {
	var vec []byte
	if q.QueryRowContext(ctx, `SELECT vec FROM embed_cache WHERE key = ?`, embed.CacheKey(VectorModel, text)).Scan(&vec) != nil {
		return nil
	}
	return embed.Decode(vec)
}

// putVector caches text's vector, as the app's put_cache.
func putVector(ctx context.Context, q vecDB, text string, v []float32) error {
	_, err := q.ExecContext(ctx, `INSERT OR REPLACE INTO embed_cache (key, model, dims, vec) VALUES (?, ?, ?, ?)`,
		embed.CacheKey(VectorModel, text), VectorModel, len(v), embed.Encode(v))
	return err
}

// vectorFor is text's vector: cached, else embedded (nil without a model,
// or for a text with nothing to embed).
func vectorFor(ctx context.Context, q vecDB, text string) []float32 {
	if v := cachedVector(ctx, q, text); v != nil {
		return v
	}
	src := Vectors()
	if src == nil {
		return nil
	}
	v := src.Embed(text)
	for _, x := range v {
		if x != 0 {
			return v
		}
	}
	return nil
}

// nearDuplicate is the stored entry of w's kind most like v at or above
// NearDuplicate; keyed entries only merge with keyless ones, and a text
// that contradicts (another number, one side negated) never merges.
func nearDuplicate(ctx context.Context, tx *sql.Tx, w Write, v []float32) (*Entry, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, key, content FROM entries WHERE kind = ? AND state <> 'archived'`, w.Kind)
	if err != nil {
		return nil, err
	}
	type cand struct {
		id           int64
		key, content string
	}
	var cs []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.id, &c.key, &c.content); err != nil {
			_ = rows.Close()
			return nil, err
		}
		cs = append(cs, c)
	}
	_ = rows.Close()
	var best int64
	bestSim := float32(0)
	for _, c := range cs {
		if w.Key != "" && c.key != "" || ContradictsText(w.Content, c.content) {
			continue
		}
		stored := cachedVector(ctx, tx, c.content)
		if stored == nil {
			continue
		}
		if sim := embed.Cosine(v, stored); sim >= NearDuplicate && sim > bestSim {
			best, bestSim = c.id, sim
		}
	}
	if best == 0 {
		return nil, nil
	}
	return scanOne(tx.QueryRowContext(ctx, `SELECT `+entryColumns+` FROM entries WHERE id = ?`, best))
}

var negations = []string{"not", "never", "no longer", "don't", "doesn't", "isn't", "instead of"}

// ContradictsText is the app's conflict signal (consolidate.rs
// contradicts_text): the texts differ in their numbers or one is negated.
func ContradictsText(a, b string) bool {
	na, nb := numbers(a), numbers(b)
	if strings.Join(na, "\x00") != strings.Join(nb, "\x00") {
		return true
	}
	return negated(a) != negated(b)
}

func numbers(s string) []string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	var out []string
	for _, p := range parts {
		if strings.ContainsAny(p, "0123456789") {
			out = append(out, strings.Trim(p, "."))
		}
	}
	sort.Strings(out)
	return out
}

func negated(s string) bool {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r == '\'' || r == '‘' || r == '’':
			b.WriteRune('\'')
		case isAlnum(r):
			b.WriteRune(r)
		default:
			b.WriteByte(' ')
		}
	}
	padded := " " + strings.Join(strings.Fields(b.String()), " ") + " "
	for _, n := range negations {
		if strings.Contains(padded, " "+n+" ") {
			return true
		}
	}
	return false
}

// denseIDs are the entries whose cached vector is within minSimilarity of
// v, best first, at most legDepth (the app searches an HNSW built from the
// same cache; this reads it exactly).
func (s *Store) denseIDs(ctx context.Context, v []float32) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, content FROM entries WHERE state <> 'archived'`)
	if err != nil {
		return nil, err
	}
	type idText struct {
		id   int64
		text string
	}
	var all []idText
	for rows.Next() {
		var x idText
		if err := rows.Scan(&x.id, &x.text); err != nil {
			_ = rows.Close()
			return nil, err
		}
		all = append(all, x)
	}
	_ = rows.Close()
	type scored struct {
		id  int64
		sim float32
	}
	var hits []scored
	for _, x := range all {
		stored := cachedVector(ctx, s.db, x.text)
		if stored == nil {
			continue
		}
		if sim := embed.Cosine(v, stored); sim >= minSimilarity {
			hits = append(hits, scored{x.id, sim})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].sim != hits[j].sim {
			return hits[i].sim > hits[j].sim
		}
		return hits[i].id < hits[j].id
	})
	if len(hits) > legDepth {
		hits = hits[:legDepth]
	}
	ids := make([]int64, len(hits))
	for i, h := range hits {
		ids[i] = h.id
	}
	return ids, nil
}

// SyncVectors embeds every durable entry without a cached vector (the app's
// sync_vectors backfill) and drops cached vectors no entry uses any more
// (garbage_keys). It returns how many it embedded; ErrNoVectors without a
// model.
func (s *Store) SyncVectors(ctx context.Context) (int, error) {
	src := Vectors()
	if src == nil {
		return 0, ErrNoVectors
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, content FROM entries`)
	if err != nil {
		return 0, err
	}
	live := map[string]bool{}
	var todo []string
	for rows.Next() {
		var kind, content string
		if err := rows.Scan(&kind, &content); err != nil {
			_ = rows.Close()
			return 0, err
		}
		key := string(embed.CacheKey(VectorModel, content))
		if live[key] {
			continue
		}
		live[key] = true
		if IsDurable(kind) {
			todo = append(todo, content)
		}
	}
	_ = rows.Close()
	n := 0
	for _, text := range todo {
		if cachedVector(ctx, s.db, text) != nil {
			continue
		}
		v := src.Embed(strings.TrimSpace(text))
		if v == nil {
			continue
		}
		if err := putVector(ctx, s.db, text, v); err != nil {
			return n, err
		}
		n++
	}
	// Garbage: cache keys of this model that no entry's text produces.
	krows, err := s.db.QueryContext(ctx, `SELECT key FROM embed_cache WHERE model = ?`, VectorModel)
	if err != nil {
		return n, err
	}
	var dead [][]byte
	for krows.Next() {
		var k []byte
		if err := krows.Scan(&k); err != nil {
			_ = krows.Close()
			return n, err
		}
		if len(k) != 32 || !live[string(k)] {
			dead = append(dead, k)
		}
	}
	_ = krows.Close()
	for _, k := range dead {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM embed_cache WHERE key = ?`, k); err != nil {
			return n, err
		}
	}
	return n, nil
}

// ErrNoVectors says there is no model to embed with.
var ErrNoVectors = errors.New("no embedding model on this machine")

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

// DurableForCorpus is one kind's active memories with a log position, the
// newest n, oldest first (the desktop's durable_entries for the corpus).
func (s *Store) DurableForCorpus(ctx context.Context, kind string, n int) ([]*Entry, error) {
	es, err := s.queryEntries(ctx, `WHERE kind = ? AND seq IS NOT NULL ORDER BY seq DESC LIMIT ?`, kind, n)
	if err != nil {
		return nil, err
	}
	var out []*Entry
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].State == StateActive {
			out = append(out, es[i])
		}
	}
	return out, nil
}
