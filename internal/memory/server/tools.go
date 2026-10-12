package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tryatlas/cli/internal/memory/corpus"
	"github.com/tryatlas/cli/internal/memory/record"
	"github.com/tryatlas/cli/internal/project"
)

// call is one tool call's context.
type call struct {
	s   *Server
	st  *record.Store
	g   Grant
	ctx context.Context
}

// Briefing and line caps (SPEC §5.7).
const (
	planChars       = 2000
	prefCount       = 30
	prefTotalChars  = 2000
	prefLineChars   = 300
	filesCount      = 50
	summaryChars    = 160
	indexEntries    = 200
	indexChars      = 8000
	indexLineChars  = 160
	handoffBytes    = 4096
	searchDefault   = 10
	searchMax       = 50
	listMax         = 200
	whyMemories     = 10
	halfLifeMS      = 14 * 24 * 3600 * 1000
	sourcesInIndex  = 2
	unknownKindText = "unknown kind `%s`; one of plan, file_changed, decision, fact, failure, architecture, preference"
)

var indexKinds = []string{record.KindDecision, record.KindFact, record.KindFailure, record.KindArchitecture}

// cut shortens s to n characters with an ellipsis.
func cut(s string, n int) (string, bool) {
	r := []rune(s)
	if len(r) <= n {
		return s, false
	}
	return string(r[:n-1]) + "…", true
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// evidenceState checks an entry's citations now.
type evidenceState struct {
	validity  string
	citations []map[string]any
}

func (t *call) evidence(e *record.Entry) *evidenceState {
	if len(e.Evidence) == 0 {
		return nil
	}
	es := &evidenceState{}
	var vs []string
	for _, c := range e.Evidence {
		v, now := record.Check(t.st.ScopeRoot, c)
		vs = append(vs, v)
		m := map[string]any{"path": now.Path, "lines": now.Lines()}
		if now.Symbol != nil {
			m["symbol"] = *now.Symbol
		}
		es.citations = append(es.citations, m)
	}
	es.validity = record.Overall(vs)
	return es
}

// entryJSON is the shape every tool shares (SPEC §5.6).
func (t *call) entryJSON(e *record.Entry, sources []string, ev *evidenceState) map[string]any {
	m := map[string]any{
		"id": e.ID, "kind": e.Kind, "key": e.Key, "content": e.Content,
		"by": e.By(), "source": e.Source, "confidence": e.Confidence, "updatedAt": e.UpdatedAt,
		"uses": e.Uses, "revision": e.Rev, "state": e.State, "added": record.Date(e.CreatedAt),
		"sources": nonNil(sources),
	}
	if e.Kind == record.KindPlan && e.Status != "" {
		m["status"] = e.Status
	}
	if e.State == record.StateCandidate {
		m["candidate"] = true
	}
	if c, _ := t.st.Conflicts(t.ctx, e.ID); len(c) > 0 {
		m["conflicts"] = c
	}
	if ev != nil {
		m["validity"] = ev.validity
		m["citations"] = ev.citations
	}
	return m
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func lastN(s []string, n int) []string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return nonNil(s)
}

func (t *call) sourcesOf(es []*record.Entry) map[int64][]string {
	var ids []int64
	for _, e := range es {
		ids = append(ids, e.ID)
	}
	m, _ := t.st.Sources(t.ctx, ids)
	return m
}

// ── memory_briefing ──────────────────────────────────────────────────────────

func score(e *record.Entry, now int64) float64 {
	last := e.UpdatedAt
	if e.LastUsedAt != nil && *e.LastUsedAt > last {
		last = *e.LastUsedAt
	}
	age := float64(max(now-last, 0))
	return math.Pow(0.5, age/halfLifeMS) + math.Log(1+float64(e.Uses)) + e.Confidence
}

func (t *call) briefing() (any, error) {
	now := t.s.now()
	out := map[string]any{"plan": nil}
	synced := int64(0)
	seen := func(e *record.Entry) {
		if e.UpdatedAt > synced {
			synced = e.UpdatedAt
		}
	}

	if plans, err := t.st.Active(t.ctx, record.KindPlan); err != nil {
		return nil, err
	} else if len(plans) > 0 {
		p := plans[0]
		seen(p)
		content, trunc := cut(strings.TrimSpace(p.Content), planChars)
		pm := map[string]any{"id": p.ID, "content": content, "status": p.Status, "by": p.By(), "updatedAt": p.UpdatedAt}
		if trunc {
			pm["truncated"] = true
		}
		out["plan"] = pm
	}

	prefs, err := t.st.Active(t.ctx, record.KindPreference)
	if err != nil {
		return nil, err
	}
	prefSources := t.sourcesOf(prefs)
	prefLines := []any{}
	total := 0
	for _, e := range prefs {
		if len(prefLines) >= prefCount {
			break
		}
		ev := t.evidence(e)
		if ev != nil && ev.validity == record.Stale {
			continue
		}
		line := t.indexLine(e, prefSources[e.ID], ev, prefLineChars)
		total += len([]rune(line["content"].(string)))
		if total > prefTotalChars {
			break
		}
		seen(e)
		prefLines = append(prefLines, line)
	}
	out["preferences"] = prefLines

	files, err := t.st.Active(t.ctx, record.KindFileChanged)
	if err != nil {
		return nil, err
	}
	fl := []any{}
	for i, e := range files {
		if i >= filesCount {
			break
		}
		seen(e)
		summary, _ := cut(oneLine(e.Content), summaryChars)
		fl = append(fl, map[string]any{"id": e.ID, "path": e.Key, "summary": summary, "by": e.By(), "updatedAt": e.UpdatedAt})
	}
	out["filesChanged"] = fl

	// The ranked index: stale entries out first, each kind to its cap, then
	// best-first across kinds within 200 entries and 8 000 characters.
	durable, err := t.st.Active(t.ctx, indexKinds...)
	if err != nil {
		return nil, err
	}
	type cand struct {
		e     *record.Entry
		ev    *evidenceState
		score float64
	}
	staleHidden := 0
	perKind := map[string][]cand{}
	for _, e := range durable {
		ev := t.evidence(e)
		if ev != nil && ev.validity == record.Stale {
			staleHidden++
			continue
		}
		perKind[e.Kind] = append(perKind[e.Kind], cand{e, ev, score(e, now)})
	}
	var pool []cand
	for _, k := range indexKinds {
		c := perKind[k]
		sort.SliceStable(c, func(i, j int) bool { return c[i].score > c[j].score })
		if len(c) > record.DisplayCap[k] {
			c = c[:record.DisplayCap[k]]
		}
		pool = append(pool, c...)
	}
	sort.SliceStable(pool, func(i, j int) bool { return pool[i].score > pool[j].score })
	var chosen []*record.Entry
	chars := 0
	for _, c := range pool {
		line, _ := cut(oneLine(c.e.Content), indexLineChars)
		if len(chosen) >= indexEntries || chars+len([]rune(line)) > indexChars {
			break
		}
		chars += len([]rune(line))
		chosen = append(chosen, c.e)
	}
	srcs := t.sourcesOf(chosen)
	index := map[string]any{}
	for _, c := range pool {
		for _, e := range chosen {
			if e.ID == c.e.ID {
				seen(e)
				lines, _ := index[e.Kind].([]any)
				index[e.Kind] = append(lines, t.indexLine(e, srcs[e.ID], c.ev, indexLineChars))
			}
		}
	}
	out["index"] = index
	out["syncedTo"] = synced
	if staleHidden > 0 {
		out["staleHidden"] = staleHidden
	}
	if h, err := t.st.LatestHandoff(t.ctx, t.g.SessionID); err == nil && h != nil && len(h) <= handoffBytes {
		out["handoff"] = h
	}
	t.s.mu.Lock()
	t.s.clocks[t.g.SessionID] = synced
	t.s.mu.Unlock()
	return out, nil
}

// indexLine is one capped line: id, kind, one-line content, writer, sources.
func (t *call) indexLine(e *record.Entry, sources []string, ev *evidenceState, limit int) map[string]any {
	content, trunc := cut(oneLine(e.Content), limit)
	m := map[string]any{"id": e.ID, "kind": e.Kind, "content": content, "by": e.By(), "confidence": e.Confidence,
		"updatedAt": e.UpdatedAt, "added": record.Date(e.CreatedAt), "sources": lastN(sources, sourcesInIndex)}
	if trunc {
		m["truncated"] = true
	}
	if ev != nil {
		m["validity"] = ev.validity
		m["citations"] = ev.citations
	}
	return m
}

// ── memory_changes ───────────────────────────────────────────────────────────

func (t *call) changes() (any, error) {
	t.s.mu.Lock()
	since := t.s.clocks[t.g.SessionID]
	t.s.mu.Unlock()
	c, err := t.st.ChangesSince(t.ctx, since, t.g.SessionID)
	if err != nil {
		return nil, err
	}
	t.s.mu.Lock()
	t.s.clocks[t.g.SessionID] = c.SyncedTo
	t.s.mu.Unlock()
	srcs := t.sourcesOf(c.Entries)
	entries := []any{}
	for _, e := range c.Entries {
		entries = append(entries, t.entryJSON(e, srcs[e.ID], t.evidence(e)))
	}
	forgotten := c.Forgotten
	if forgotten == nil {
		forgotten = []int64{}
	}
	return map[string]any{"since": c.Since, "syncedTo": c.SyncedTo, "more": c.More, "forgotten": forgotten, "entries": entries}, nil
}

// ── memory_search ────────────────────────────────────────────────────────────

func (t *call) search(args json.RawMessage) (any, error) {
	var a struct {
		Query string   `json:"query"`
		Kinds []string `json:"kinds"`
		Limit *int     `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, errorf("memory unavailable: %v", err)
	}
	for _, k := range a.Kinds {
		if !record.IsKind(k) {
			return nil, errorf(unknownKindText, k)
		}
	}
	kinds := a.Kinds
	if len(kinds) == 0 {
		kinds = record.DurableKinds
	}
	limit := searchDefault
	if a.Limit != nil {
		limit = min(max(*a.Limit, 1), searchMax)
	}
	hits, err := t.st.Search(t.ctx, a.Query, kinds, 100)
	if err != nil {
		return nil, err
	}
	type scored struct {
		h     record.Hit
		ev    *evidenceState
		stale bool
	}
	var list []scored
	for _, h := range hits {
		ev := t.evidence(h.Entry)
		list = append(list, scored{h, ev, ev != nil && ev.validity == record.Stale})
	}
	sort.SliceStable(list, func(i, j int) bool { return !list[i].stale && list[j].stale })
	if len(list) > limit {
		list = list[:limit]
	}
	var es []*record.Entry
	var ids []int64
	for _, x := range list {
		es = append(es, x.h.Entry)
		ids = append(ids, x.h.Entry.ID)
	}
	_ = t.st.TouchUsed(t.ctx, ids, t.s.now())
	srcs := t.sourcesOf(es)
	entries := []any{}
	for _, x := range list {
		m := t.entryJSON(x.h.Entry, srcs[x.h.Entry.ID], x.ev)
		why := map[string]any{}
		if x.h.BM25 > 0 {
			why["bm25"] = x.h.BM25
		}
		if x.h.Dense > 0 {
			why["dense"] = x.h.Dense
		}
		if len(why) > 0 {
			m["why"] = why
		}
		entries = append(entries, m)
	}
	documents := []any{}
	if len(a.Kinds) == 0 {
		docLimit := indexDefault
		if a.Limit != nil {
			docLimit = min(max(*a.Limit, 1), indexMax)
		}
		for _, d := range t.documents(a.Query, docLimit) {
			documents = append(documents, map[string]any{"title": d.Title, "source": d.Source, "text": d.Text})
		}
	}
	return map[string]any{"entries": entries, "documents": documents}, nil
}

// How many indexed documents memory_search adds (tools.rs INDEX_*_LIMIT).
const (
	indexDefault = 6
	indexMax     = 20
)

// documents is the index leg of memory_search: the project's corpus, best
// effort (no index, no model: BM25 only or nothing). A shared memory's
// document is returned only while its entry still exists.
func (t *call) documents(query string, limit int) []corpus.Result {
	root := t.g.Cwd
	if p, err := project.Discover(t.ctx, t.g.Cwd); err == nil {
		root = p.Root
	}
	ix, err := corpus.Open(root, false)
	if err != nil {
		return nil
	}
	defer ix.Close()
	var qvec []float32
	if src := record.Vectors(); src != nil {
		qvec = src.Embed(query) // untrimmed, as the app's retrieve
	}
	live := func(id string) bool {
		rest, ok := strings.CutPrefix(id, "shared:")
		if !ok {
			return true
		}
		_, num, _ := strings.Cut(rest, ":")
		n, err := strconv.ParseInt(num, 10, 64)
		if err != nil {
			return false
		}
		e, err := t.st.Get(t.ctx, n)
		return err == nil && e != nil
	}
	ctx, cancel := context.WithTimeout(t.ctx, 6*time.Second)
	defer cancel()
	res, _ := ix.Retrieve(ctx, query, qvec, limit, live)
	return res
}

// ── memory_get, memory_list, memory_history ──────────────────────────────────

type idArg struct {
	ID *int64 `json:"id"`
}

func parseID(args json.RawMessage) (int64, error) {
	var a idArg
	if err := json.Unmarshal(args, &a); err != nil || a.ID == nil {
		return 0, errorf("give the id of a memory entry")
	}
	return *a.ID, nil
}

func (t *call) get(args json.RawMessage) (any, error) {
	id, err := parseID(args)
	if err != nil {
		return nil, err
	}
	e, err := t.st.Get(t.ctx, id)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, errorf("no memory entry %d", id)
	}
	_ = t.st.TouchUsed(t.ctx, []int64{id}, t.s.now())
	m := t.entryJSON(e, t.sourcesOf([]*record.Entry{e})[e.ID], t.evidence(e))
	prov, err := t.provenance(id)
	if err != nil {
		return nil, err
	}
	m["provenance"] = prov
	return map[string]any{"entry": m}, nil
}

func (t *call) list(args json.RawMessage) (any, error) {
	var a struct {
		Kind  string `json:"kind"`
		Limit *int   `json:"limit"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, errorf("memory unavailable: %v", err)
	}
	if a.Kind != "" && !record.IsKind(a.Kind) {
		return nil, errorf(unknownKindText, a.Kind)
	}
	var es []*record.Entry
	kinds := record.Kinds
	if a.Kind != "" {
		kinds = []string{a.Kind}
	}
	for _, k := range kinds {
		n := record.DisplayCap[k]
		if a.Limit != nil {
			n = min(max(*a.Limit, 1), listMax)
		}
		got, err := t.st.List(t.ctx, k, n)
		if err != nil {
			return nil, err
		}
		es = append(es, got...)
	}
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].UpdatedAt != es[j].UpdatedAt {
			return es[i].UpdatedAt > es[j].UpdatedAt
		}
		return es[i].ID > es[j].ID
	})
	if a.Limit != nil && len(es) > min(max(*a.Limit, 1), listMax) {
		es = es[:min(max(*a.Limit, 1), listMax)]
	}
	srcs := t.sourcesOf(es)
	entries := []any{}
	for _, e := range es {
		entries = append(entries, t.entryJSON(e, srcs[e.ID], t.evidence(e)))
	}
	return map[string]any{"entries": entries}, nil
}

func (t *call) history(args json.RawMessage) (any, error) {
	id, err := parseID(args)
	if err != nil {
		return nil, err
	}
	revs, err := t.st.History(t.ctx, id)
	if err != nil {
		return nil, err
	}
	if len(revs) == 0 {
		return nil, errorf("no memory entry %d", id)
	}
	list := []any{}
	for _, r := range revs {
		list = append(list, map[string]any{"revision": r.Rev, "op": r.Op, "content": r.Content, "by": r.By(),
			"state": r.State, "confidence": r.Confidence, "at": r.At, "note": r.Note})
	}
	prov, err := t.provenance(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "revisions": list, "provenance": prov}, nil
}

// provenance lists each distinct writer of an entry: who, when first, and
// what capture knows about the session.
func (t *call) provenance(id int64) ([]any, error) {
	revs, err := t.st.History(t.ctx, id)
	if err != nil {
		return nil, err
	}
	capt := openCapture(t.ctx, t.g.Cwd)
	defer capt.close()
	out := []any{}
	seen := map[string]bool{}
	for _, r := range revs {
		if r.State == record.StateTombstoned {
			continue
		}
		u := record.SourceURI(r.Source, r.Agent, r.Session)
		if seen[u] {
			continue
		}
		seen[u] = true
		p := map[string]any{"source": u, "agent": r.Agent, "added": record.Date(r.At), "title": nil, "commits": []any{}}
		if info := capt.session(r.Session); info != nil {
			p["title"] = info.title
			p["commits"] = info.commitList()
		}
		out = append(out, p)
	}
	return out, nil
}

// ── memory_why ───────────────────────────────────────────────────────────────

var shaRE = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func (t *call) why(args json.RawMessage) (any, error) {
	var a struct {
		Path   string `json:"path"`
		Commit string `json:"commit"`
	}
	_ = json.Unmarshal(args, &a)
	if (a.Path == "") == (a.Commit == "") {
		return nil, errorf("give exactly one of path or commit")
	}
	capt := openCapture(t.ctx, t.g.Cwd)
	defer capt.close()
	var target map[string]any
	var sessions []*capSession
	var memories []*record.Entry
	if a.Commit != "" {
		sha := strings.ToLower(strings.TrimSpace(a.Commit))
		if !shaRE.MatchString(sha) {
			return nil, errorf("`%s` is not a commit sha (7 to 40 hex characters)", a.Commit)
		}
		target = map[string]any{"commit": sha}
		sessions = capt.byCommit(sha)
	} else {
		abs := a.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(t.g.Cwd, a.Path)
		}
		abs = filepath.Clean(abs)
		rel, err := filepath.Rel(t.st.ScopeRoot, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil, errorf("`%s` is not a path inside the repository", a.Path)
		}
		target = map[string]any{"path": filepath.ToSlash(rel)}
		sessions = capt.byPath(abs)
		cited, err := t.st.EntriesCiting(t.ctx, filepath.ToSlash(rel), whyMemories)
		if err != nil {
			return nil, err
		}
		memories = cited
	}
	var native []string
	list := []any{}
	for _, s := range sessions {
		native = append(native, s.native)
		m := map[string]any{"session": record.SourceURI("", s.agent, s.native), "title": s.title, "agent": s.agent,
			"date": s.date, "commits": s.commitList()}
		list = append(list, m)
	}
	if written, err := t.st.EntriesBySessions(t.ctx, native, whyMemories); err == nil {
		memories = mergeEntries(memories, written, whyMemories)
	}
	srcs := t.sourcesOf(memories)
	mems := []any{}
	for _, e := range memories {
		mems = append(mems, t.entryJSON(e, srcs[e.ID], t.evidence(e)))
	}
	out := map[string]any{"target": target, "sessions": list, "memories": mems}
	switch {
	case !capt.on && a.Path != "":
		out["note"] = "Session capture is off here; these are the memories that cite this path."
	case !capt.on:
		out["note"] = "Session capture is off here, so no recorded session can be named."
	case len(sessions) == 0:
		out["note"] = "No recorded session matches this."
	}
	return out, nil
}

func mergeEntries(a, b []*record.Entry, n int) []*record.Entry {
	seen := map[int64]bool{}
	var out []*record.Entry
	for _, e := range append(a, b...) {
		if !seen[e.ID] && len(out) < n {
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	return out
}

// ── memory_remember, memory_feedback, memory_forget ──────────────────────────

func (t *call) remember(args json.RawMessage) (any, error) {
	var a struct {
		Kind             string `json:"kind"`
		Content          string `json:"content"`
		Key              string `json:"key"`
		ExpectedRevision *int64 `json:"expected_revision"`
		Evidence         []struct {
			Path   string  `json:"path"`
			Lines  string  `json:"lines"`
			Symbol *string `json:"symbol"`
		} `json:"evidence"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return nil, errorf("not remembered: %v", err)
	}
	switch {
	case a.Kind == record.KindPlan || a.Kind == record.KindFileChanged:
		return nil, errorf("not remembered: `%s` is working memory, captured from the session automatically; remember records only decision, fact, failure, architecture or preference", a.Kind)
	case !record.IsDurable(a.Kind):
		return nil, errorf("not remembered: "+unknownKindText, a.Kind)
	case strings.TrimSpace(a.Content) == "":
		return nil, errorf("not remembered: nothing to remember: content is empty")
	case len(a.Evidence) > record.MaxCitations:
		return nil, errorf("not remembered: at most 8 citations per memory")
	}
	roots := append([]string{t.st.ScopeRoot}, project.Worktrees(t.ctx, t.st.ScopeRoot)...)
	var cites []record.Citation
	for _, ev := range a.Evidence {
		c, err := record.Cite(roots, t.g.Cwd, ev.Path, ev.Lines, ev.Symbol)
		if err != nil {
			return nil, errorf("not remembered: %s", err.Error())
		}
		cites = append(cites, c)
	}
	outcome, e, err := t.st.Remember(t.ctx, record.Write{
		Kind: a.Kind, Key: a.Key, Content: a.Content, Source: t.g.Agent, Agent: t.g.Agent, Session: t.g.SessionID,
		Confidence: 1.0, Evidence: cites, At: t.s.now(), Guarded: true, ExpectedRevision: a.ExpectedRevision, LogEvent: true,
	})
	var conflict *record.Conflict
	if errors.As(err, &conflict) {
		return nil, errorf("not remembered: %s", conflict.Error())
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"outcome": outcome, "entry": t.entryJSON(e, t.sourcesOf([]*record.Entry{e})[e.ID], t.evidence(e))}, nil
}

func (t *call) feedback(args json.RawMessage) (any, error) {
	var a struct {
		ID      *int64 `json:"id"`
		Verdict string `json:"verdict"`
		Note    string `json:"note"`
	}
	if err := json.Unmarshal(args, &a); err != nil || a.ID == nil {
		return nil, errorf("not remembered: give the id of a memory entry")
	}
	if a.Verdict != record.VerdictUseful && a.Verdict != record.VerdictWrong && a.Verdict != record.VerdictStale {
		return nil, errorf("unknown verdict `%s`; one of useful, wrong, stale", a.Verdict)
	}
	e, err := t.st.Feedback(t.ctx, *a.ID, a.Verdict, a.Note, t.g.Agent, t.g.SessionID, t.s.now())
	if err != nil {
		return nil, err
	}
	if e == nil {
		return map[string]any{"forgotten": false, "id": *a.ID}, nil
	}
	return map[string]any{"entry": t.entryJSON(e, t.sourcesOf([]*record.Entry{e})[e.ID], t.evidence(e))}, nil
}

func (t *call) forget(args json.RawMessage) (any, error) {
	id, err := parseID(args)
	if err != nil {
		return nil, err
	}
	before, _ := t.st.Get(t.ctx, id)
	ok, err := t.st.Forget(t.ctx, id, t.g.SessionID, t.s.now())
	if err != nil {
		return nil, err
	}
	if ok && before != nil {
		// Its document leaves the index now, not at the next pass.
		root := t.g.Cwd
		if p, err := project.Discover(t.ctx, t.g.Cwd); err == nil {
			root = p.Root
		}
		if ix, err := corpus.Open(root, false); err == nil {
			_ = ix.Delete(t.ctx, fmt.Sprintf("shared:%s:%d", before.Kind, id))
			_ = ix.Close()
		}
	}
	return map[string]any{"forgotten": ok, "id": id}, nil
}
