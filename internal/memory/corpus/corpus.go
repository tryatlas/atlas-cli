// Package corpus is the memory retrieval index the desktop keeps in
// <project>/.atlas/memory/corpus.sqlite (crates/atlas-memory/src/corpus.rs):
// the project's documents (its CLAUDE.md and AGENTS.md, knowledge notes,
// durable shared memories, captured sessions), each with a BM25 row and a
// MiniLM vector, searched by memory_search as "documents". The schema, ids,
// keys and vectors are the app's, so either side keeps the index the other
// built. The app's usearch file is a projection it rebuilds from
// embed_cache; the CLI flags it dirty after a change and searches the cache
// itself.
package corpus

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/zeebo/blake3"

	"github.com/tryatlas/atlas-cli/internal/embed"
	memrecord "github.com/tryatlas/atlas-cli/internal/memory/record"
)

// File is the index's database inside .atlas/memory/.
const File = "corpus.sqlite"

const schema = `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS docs (id TEXT PRIMARY KEY, vkey INTEGER NOT NULL, content_hash TEXT NOT NULL,
  corpus TEXT NOT NULL, title TEXT NOT NULL, source TEXT NOT NULL, text TEXT NOT NULL, scope TEXT NOT NULL DEFAULT 'repo');
CREATE INDEX IF NOT EXISTS docs_vkey ON docs(vkey);
CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(title, text, content='', contentless_delete=1,
  tokenize='porter unicode61 remove_diacritics 2');
CREATE TABLE IF NOT EXISTS embed_cache (key BLOB PRIMARY KEY, model TEXT NOT NULL, dims INTEGER NOT NULL, vec BLOB NOT NULL);
`

// Index is an open corpus.sqlite.
type Index struct {
	db   *sql.DB
	Path string
}

// Path is the project's corpus file.
func Path(root string) string { return filepath.Join(root, ".atlas", "memory", File) }

// Open opens (with create, creates) the project's index.
func Open(root string, create bool) (*Index, error) {
	p := Path(root)
	if _, err := os.Stat(p); err != nil {
		if !create {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(p)
	db, err := sql.Open("sqlite", "file:"+esc+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("corpus schema: %w", err)
	}
	return &Index{db: db, Path: p}, nil
}

// Close closes it.
func (x *Index) Close() error { return x.db.Close() }

// Doc is one document as a source yields it.
type Doc struct {
	ID, Title, Text string
	Source          string // the corpus tag: shared, claude, codex, note, or an agent id
}

// embedText is what is embedded and hashed: the title, then the text
// (memory_indexer.rs embed_text).
func (d Doc) embedText() string {
	if strings.TrimSpace(d.Text) == "" {
		return d.Title
	}
	return d.Title + "\n\n" + d.Text
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// cacheKey is blake3(model ‖ 0 ‖ content hash hex) (corpus.rs doc_cache_key).
func cacheKey(model, contentHash string) []byte {
	h := blake3.New()
	_, _ = h.Write([]byte(model))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(contentHash))
	return h.Sum(nil)
}

// splitEmbedded is the stored title and body: cut at the first blank line,
// both trimmed (docstore.rs split_embedded).
func splitEmbedded(s string) (string, string) {
	title, body, _ := strings.Cut(s, "\n\n")
	return strings.TrimSpace(title), strings.TrimSpace(body)
}

// Stats counts what a pass did.
type Stats struct {
	Added, Updated, Deleted, Unchanged, Embedded int
}

// Update brings the index in line with docs: unchanged documents (same hash,
// vector cached) are left alone, changed or new ones written (and embedded
// when emb is not nil and the vector is not cached), and stored documents
// owned reports as ours but that are no longer in docs deleted. Documents
// another writer owns (the app's readers of agent folders) are never
// touched.
func (x *Index) Update(ctx context.Context, docs []Doc, owned func(id string) bool, emb memrecord.VectorSource) (Stats, error) {
	var st Stats
	model := embed.ModelName
	stored := map[string]string{} // id → content_hash
	rows, err := x.db.QueryContext(ctx, `SELECT id, content_hash FROM docs`)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			_ = rows.Close()
			return st, err
		}
		stored[id] = h
	}
	_ = rows.Close()
	hasVec := func(h string) bool {
		var one int
		return x.db.QueryRowContext(ctx, `SELECT 1 FROM embed_cache WHERE key = ?`, cacheKey(model, h)).Scan(&one) == nil
	}
	type plan struct {
		doc  Doc
		text string
		hash string
		vec  []float32
	}
	var upserts []plan
	current := map[string]bool{}
	for _, d := range docs {
		if current[d.ID] {
			continue
		}
		current[d.ID] = true
		text := d.embedText()
		h := hashText(text)
		cached := hasVec(h)
		if old, ok := stored[d.ID]; ok && old == h && (cached || emb == nil) {
			st.Unchanged++
			continue
		}
		p := plan{doc: d, text: text, hash: h}
		if !cached && emb != nil {
			p.vec = emb.Embed(text)
			st.Embedded++
		}
		upserts = append(upserts, p)
	}
	var deletes []string
	for id := range stored {
		if !current[id] && owned(id) {
			deletes = append(deletes, id)
		}
	}
	sort.Strings(deletes)
	if len(upserts) == 0 && len(deletes) == 0 {
		return st, nil
	}
	tx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		return st, err
	}
	defer func() { _ = tx.Rollback() }()
	changedVectors := false
	for _, p := range upserts {
		var rowid int64
		switch err := tx.QueryRowContext(ctx, `SELECT rowid FROM docs WHERE id = ?`, p.doc.ID).Scan(&rowid); {
		case err == nil:
			if _, err := tx.ExecContext(ctx, `DELETE FROM docs_fts WHERE rowid = ?`, rowid); err != nil {
				return st, err
			}
			st.Updated++
		case errors.Is(err, sql.ErrNoRows):
			st.Added++
		default:
			return st, err
		}
		title, body := splitEmbedded(p.text)
		scope := "repo"
		if strings.HasSuffix(p.doc.ID, "@global") {
			scope = "user"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO docs (id, vkey, content_hash, corpus, title, source, text, scope)
		    VALUES (?1, ?2, ?3, ?4, ?5, ?4, ?6, ?7)
		    ON CONFLICT(id) DO UPDATE SET content_hash = excluded.content_hash, corpus = excluded.corpus,
		      title = excluded.title, source = excluded.source, text = excluded.text, scope = excluded.scope`,
			p.doc.ID, embed.VKey(p.doc.ID), p.hash, p.doc.Source, title, body, scope); err != nil {
			return st, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO docs_fts (rowid, title, text) SELECT rowid, title, text FROM docs WHERE id = ?`, p.doc.ID); err != nil {
			return st, err
		}
		if p.vec != nil {
			if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO embed_cache (key, model, dims, vec) VALUES (?, ?, ?, ?)`,
				cacheKey(model, p.hash), model, len(p.vec), embed.Encode(p.vec)); err != nil {
				return st, err
			}
		}
		changedVectors = true
	}
	for _, id := range deletes {
		var rowid int64
		if tx.QueryRowContext(ctx, `SELECT rowid FROM docs WHERE id = ?`, id).Scan(&rowid) == nil {
			if _, err := tx.ExecContext(ctx, `DELETE FROM docs_fts WHERE rowid = ?`, rowid); err != nil {
				return st, err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM docs WHERE id = ?`, id); err != nil {
			return st, err
		}
		st.Deleted++
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (k, v) VALUES ('model', ?), ('dims', ?)`, model, fmt.Sprint(embed.Dim)); err != nil {
		return st, err
	}
	if changedVectors || len(deletes) > 0 {
		// The app's usearch file no longer matches: it rebuilds it from
		// embed_cache on its next open (corpus.rs heal).
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (k, v) VALUES (?, '1')`, "vectors_dirty:"+model); err != nil {
			return st, err
		}
	}
	return st, tx.Commit()
}

// Retrieval constants (retrieve.rs).
const (
	minCosine    = 0.30
	rrfK         = 60
	jaccardDedup = 0.8
	minQueryLen  = 4
)

// Result is one document memory_search returns.
type Result struct {
	ID, Title, Source, Text string
}

// Retrieve is the documents leg of memory_search: dense (cosine over the
// cached vectors, at least 0.30) and BM25 over docs_fts, pool max(4·limit,
// 20) each, fused by reciprocal rank (k 60), near-duplicates (Jaccard ≥ 0.8
// over words) dropped, at most limit. keep filters ids (live shared
// memories). qvec is the query's vector, nil without a model (then BM25
// only).
func (x *Index) Retrieve(ctx context.Context, query string, qvec []float32, limit int, keep func(id string) bool) ([]Result, error) {
	if len(strings.TrimSpace(query)) < minQueryLen || limit <= 0 {
		return nil, nil
	}
	pool := max(limit*4, 20)
	var dense []string
	if qvec != nil {
		type scored struct {
			id  string
			sim float32
		}
		var hits []scored
		drows, err := x.db.QueryContext(ctx, `SELECT id, content_hash FROM docs`)
		if err != nil {
			return nil, err
		}
		type idHash struct{ id, hash string }
		var all []idHash
		for drows.Next() {
			var ih idHash
			if err := drows.Scan(&ih.id, &ih.hash); err != nil {
				_ = drows.Close()
				return nil, err
			}
			all = append(all, ih)
		}
		_ = drows.Close()
		for _, ih := range all {
			var vec []byte
			if x.db.QueryRowContext(ctx, `SELECT vec FROM embed_cache WHERE key = ?`, cacheKey(embed.ModelName, ih.hash)).Scan(&vec) != nil {
				continue
			}
			if sim := embed.Cosine(qvec, embed.Decode(vec)); sim >= minCosine {
				hits = append(hits, scored{ih.id, sim})
			}
		}
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].sim > hits[j].sim })
		for i, h := range hits {
			if i >= pool {
				break
			}
			dense = append(dense, h.id)
		}
	}
	var bm []string
	if fq := memrecord.FTSQuery(query); fq != "" {
		rows, err := x.db.QueryContext(ctx, `SELECT d.id FROM docs_fts f JOIN docs d ON d.rowid = f.rowid
		    WHERE docs_fts MATCH ? ORDER BY f.rank LIMIT ?`, fq, pool)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			bm = append(bm, id)
		}
		_ = rows.Close()
	}
	score := map[string]float64{}
	var order []string
	add := func(list []string) {
		for r, id := range list {
			if _, ok := score[id]; !ok {
				order = append(order, id)
			}
			score[id] += 1.0 / float64(rrfK+r+1)
		}
	}
	add(dense)
	add(bm)
	sort.SliceStable(order, func(i, j int) bool { return score[order[i]] > score[order[j]] })
	var out []Result
	var kept [][]string
	for _, id := range order {
		if keep != nil && !keep(id) {
			continue
		}
		var r Result
		if x.db.QueryRowContext(ctx, `SELECT id, title, source, text FROM docs WHERE id = ?`, id).Scan(&r.ID, &r.Title, &r.Source, &r.Text) != nil {
			continue
		}
		words := tokens(r.Title + " " + r.Text)
		dup := false
		for _, k := range kept {
			if jaccard(words, k) >= jaccardDedup {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		kept = append(kept, words)
		r.Text = strings.TrimSpace(r.Text)
		out = append(out, r)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func tokens(s string) []string {
	var out []string
	for _, w := range strings.Fields(s) {
		w = strings.ToLower(strings.TrimFunc(w, func(r rune) bool { return !isAlnum(r) }))
		if len(w) >= 2 {
			out = append(out, w)
		}
	}
	return out
}

func jaccard(a, b []string) float64 {
	sa, sb := map[string]bool{}, map[string]bool{}
	for _, w := range a {
		sa[w] = true
	}
	for _, w := range b {
		sb[w] = true
	}
	inter := 0
	for w := range sa {
		if sb[w] {
			inter++
		}
	}
	union := len(sa) + len(sb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }

// Delete drops one document now (memory_forget evicts its shared doc).
func (x *Index) Delete(ctx context.Context, id string) error {
	tx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var rowid int64
	if tx.QueryRowContext(ctx, `SELECT rowid FROM docs WHERE id = ?`, id).Scan(&rowid) != nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM docs_fts WHERE rowid = ?`, rowid); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM docs WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO meta (k, v) VALUES (?, '1')`, "vectors_dirty:"+embed.ModelName); err != nil {
		return err
	}
	return tx.Commit()
}
