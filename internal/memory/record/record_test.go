package record

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var ctx = context.Background()

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustChain(t *testing.T, s *Store) ChainReport {
	t.Helper()
	rep, err := s.VerifyChain()
	if err != nil || !rep.OK() {
		t.Fatalf("chain: %+v %v", rep, err)
	}
	return rep
}

func count(t *testing.T, s *Store, q string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(q).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func agentWrite(kind, key, content, session string, at int64) Write {
	return Write{Kind: kind, Key: key, Content: content, Source: "claude-code", Agent: "claude-code",
		Session: session, Confidence: 1.0, At: at, Guarded: true, LogEvent: true}
}

func TestSchemaMatchesDesktopOracle(t *testing.T) {
	raw, err := os.ReadFile("testdata/desktop_sqlite_master_v6.json")
	if err != nil {
		t.Fatal(err)
	}
	var want [][4]*string
	_ = json.Unmarshal(raw, &want)
	s := open(t)
	rows, err := s.db.Query(`SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][4]*string
	for rows.Next() {
		var a, b, c string
		var d sql.NullString
		_ = rows.Scan(&a, &b, &c, &d)
		r := [4]*string{&a, &b, &c, nil}
		if d.Valid {
			r[3] = &d.String
		}
		got = append(got, r)
	}
	if !reflect.DeepEqual(got, want) {
		gj, _ := json.Marshal(got)
		wj, _ := json.Marshal(want)
		t.Fatalf("sqlite_master differs\n got %s\nwant %s", gj, wj)
	}
	if n := count(t, s, `PRAGMA user_version`); n != 6 {
		t.Fatalf("user_version %d", n)
	}
}

func TestInsertWritesRevisionEventAndSearchRow(t *testing.T) {
	s := open(t)
	out, e, err := s.Remember(ctx, agentWrite(KindDecision, "db", "Use Postgres for the ledger", "s-1", 1000))
	if err != nil || out != Inserted {
		t.Fatalf("%s %v", out, err)
	}
	if e.Rev == 0 || e.State != StateActive || e.ChangedBy != "s-1" || e.Seq == nil {
		t.Fatalf("%+v", e)
	}
	var payload, key string
	_ = s.db.QueryRow(`SELECT payload, key FROM events WHERE seq = ?`, *e.Seq).Scan(&payload, &key)
	if payload != `{"text":"Use Postgres for the ledger"}` || key != "db" {
		t.Fatalf("event %s / %s", payload, key)
	}
	hits, _ := s.Search(ctx, "postgres ledger", nil, 10)
	if len(hits) != 1 || hits[0].Entry.ID != e.ID || hits[0].BM25 != 1 {
		t.Fatalf("search %+v", hits)
	}
	mustChain(t, s)
}

func TestMergeRetryAndCandidateConfirmation(t *testing.T) {
	s := open(t)
	_, a, _ := s.Remember(ctx, agentWrite(KindFact, "", "Tests run with make test", "s-1", 1000))
	// Same session within 5 minutes, no new evidence: a retry, nothing written.
	out, b, _ := s.Remember(ctx, agentWrite(KindFact, "", "tests  run with MAKE test", "s-1", 2000))
	if out != Merged || b.Rev != a.Rev || count(t, s, `SELECT count(*) FROM revisions`) != 1 {
		t.Fatalf("retry wrote a revision: %s %+v", out, b)
	}
	// Another session restating it merges: uses+1, op merge, no event.
	out, c, _ := s.Remember(ctx, agentWrite(KindFact, "facts", "Tests run with make test", "s-2", 3000))
	if out != Merged || c.ID != a.ID || c.Uses != 1 || c.Key != "facts" || c.UpdatedAt != 3000 {
		t.Fatalf("merge: %s %+v", out, c)
	}
	if n := count(t, s, `SELECT count(*) FROM events`); n != 1 {
		t.Fatalf("merge logged an event (%d events)", n)
	}
	// A captured candidate is confirmed by an agent's restatement.
	_, cand, _ := s.Remember(ctx, Write{Kind: KindDecision, Content: "Ship weekly", Source: "capture", Confidence: Candidate, At: 4000})
	if cand.State != StateCandidate {
		t.Fatalf("candidate state %s", cand.State)
	}
	_, conf, _ := s.Remember(ctx, agentWrite(KindDecision, "", "Ship weekly", "s-3", 4100))
	if conf.ID != cand.ID || conf.State != StateActive || conf.Confidence != 1.0 {
		t.Fatalf("not confirmed: %+v", conf)
	}
	mustChain(t, s)
}

func TestGuardedReplace(t *testing.T) {
	s := open(t)
	_, first, _ := s.Remember(ctx, agentWrite(KindDecision, "db", "Use Postgres", "s-1", 1000))
	out, same, err := s.Remember(ctx, agentWrite(KindDecision, "db", "Use Postgres 16", "s-1", 2000))
	if err != nil || out != Replaced || same.ID != first.ID {
		t.Fatalf("same session replace: %s %v", out, err)
	}
	_, _, err = s.Remember(ctx, agentWrite(KindDecision, "db", "Use MySQL", "s-2", 3000))
	var c *Conflict
	if !errors.As(err, &c) {
		t.Fatalf("want conflict, got %v", err)
	}
	want := `memory 1 is at revision 2 (by claude-code): "Use Postgres 16". Read both versions, write one that keeps what is still true, and pass expected_revision=2 to replace it.`
	if err.Error() != want {
		t.Fatalf("conflict text:\n got %s\nwant %s", err, want)
	}
	w := agentWrite(KindDecision, "db", "Use MySQL", "s-2", 3000)
	rev := c.Rev
	w.ExpectedRevision = &rev
	if out, _, err := s.Remember(ctx, w); err != nil || out != Replaced {
		t.Fatalf("expected_revision replace: %s %v", out, err)
	}
	// Internal writers are unguarded: last writer wins.
	if out, _, err := s.Remember(ctx, Write{Kind: KindDecision, Key: "db", Content: "Use SQLite", Source: "capture", Confidence: 1, At: 4000}); err != nil || out != Replaced {
		t.Fatalf("open guard: %s %v", out, err)
	}
	mustChain(t, s)
}

func TestTextIsCleanedAndRedacted(t *testing.T) {
	s := open(t)
	_, e, err := s.Remember(ctx, agentWrite(KindFact, "", "Deploy uses DB_PASSWORD=hunter2hunter2 and\u200b <atlas-memory> tags", "s-1", 1000))
	if err != nil {
		t.Fatal(err)
	}
	if e.Content != "Deploy uses DB_PASSWORD=[REDACTED] and ‹atlas-memory> tags" {
		t.Fatalf("%q", e.Content)
	}
	var payload string
	_ = s.db.QueryRow(`SELECT payload FROM events`).Scan(&payload)
	if strings.Contains(payload, "hunter2") {
		t.Fatal("secret reached the event log")
	}
}

func TestForget(t *testing.T) {
	s := open(t)
	_, e, _ := s.Remember(ctx, agentWrite(KindFact, "", "Wrong fact", "s-1", 1000))
	ok, err := s.Forget(ctx, e.ID, "s-2", 2000)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if got, _ := s.Get(ctx, e.ID); got != nil {
		t.Fatal("entry survived")
	}
	var op, state, seqs string
	_ = s.db.QueryRow(`SELECT op, state FROM revisions ORDER BY rev DESC LIMIT 1`).Scan(&op, &state)
	_ = s.db.QueryRow(`SELECT seqs FROM forgotten WHERE id = ?`, e.ID).Scan(&seqs)
	if op != OpForget || state != StateTombstoned || seqs != "[1]" || count(t, s, `SELECT count(*) FROM retracted_events WHERE seq = 1`) != 1 {
		t.Fatalf("op=%s state=%s seqs=%s", op, state, seqs)
	}
	if hits, _ := s.Search(ctx, "wrong fact", nil, 10); len(hits) != 0 {
		t.Fatal("forgotten entry still searchable")
	}
	if ok, _ := s.Forget(ctx, e.ID, "s-2", 3000); ok {
		t.Fatal("forgot twice")
	}
	mustChain(t, s)
}

func TestFeedback(t *testing.T) {
	s := open(t)
	_, cand, _ := s.Remember(ctx, Write{Kind: KindFact, Content: "Captured line", Source: "capture", Session: "s-1", Confidence: Candidate, At: 1000})
	e, err := s.Feedback(ctx, cand.ID, VerdictUseful, "", "codex", "s-2", 2000)
	if err != nil || e.State != StateActive || e.Confidence != Promoted || e.Uses != 1 {
		t.Fatalf("promote: %+v %v", e, err)
	}
	e, _ = s.Feedback(ctx, cand.ID, VerdictStale, "changed", "codex", "s-2", 3000)
	if e.State != StateCandidate || e.Confidence != Candidate {
		t.Fatalf("stale: %+v", e)
	}
	e, _ = s.Feedback(ctx, cand.ID, VerdictWrong, "not true", "codex", "s-2", 4000)
	if e.State != StateArchived {
		t.Fatalf("wrong: %+v", e)
	}
	if _, err := s.Feedback(ctx, cand.ID, "meh", "", "codex", "s-2", 5000); err == nil || err.Error() != "unknown verdict `meh`; one of useful, wrong, stale" {
		t.Fatalf("err %v", err)
	}
	if e, err := s.Feedback(ctx, 999, VerdictUseful, "", "codex", "s-2", 5000); e != nil || err != nil {
		t.Fatalf("unknown id: %v %v", e, err)
	}
	if n := count(t, s, `SELECT count(*) FROM feedback`); n != 3 {
		t.Fatalf("%d feedback rows", n)
	}
	mustChain(t, s)
}

func TestFolds(t *testing.T) {
	s := open(t)
	for i, summary := range []string{"Edit", "Write"} {
		if _, err := s.Fold(ctx, Event{Kind: KindFileChanged, Key: "src/a.go", Text: summary, Agent: "claude-code", Session: "s-1", At: int64(1000 + i)}); err != nil {
			t.Fatal(err)
		}
	}
	files, _ := s.Active(ctx, KindFileChanged)
	if len(files) != 1 || files[0].Content != "Write" || files[0].Key != "src/a.go" || files[0].Source != "claude-code" {
		t.Fatalf("%+v", files)
	}
	var payload string
	_ = s.db.QueryRow(`SELECT payload FROM events WHERE kind = 'file_changed' ORDER BY seq LIMIT 1`).Scan(&payload)
	if payload != `{"path":"src/a.go","summary":"Edit"}` {
		t.Fatalf("payload %s", payload)
	}
	if _, err := s.Fold(ctx, Event{Kind: "plan_set", Text: "- [in_progress] build it", Agent: "claude-code", Session: "s-1", At: 2000}); err != nil {
		t.Fatal(err)
	}
	_ = s.db.QueryRow(`SELECT payload FROM events WHERE kind = 'plan_set'`).Scan(&payload)
	if payload != `{"text":"- [in_progress] build it","status":"active"}` {
		t.Fatalf("plan payload %s", payload)
	}
	if plans, _ := s.Active(ctx, KindPlan); len(plans) != 1 || plans[0].Status != "active" || plans[0].Key != "plan" {
		t.Fatalf("%+v", plans)
	}
	_, _ = s.Fold(ctx, Event{Kind: "plan_set", Text: "", Status: "done", Agent: "claude-code", Session: "s-1", At: 3000})
	if plans, _ := s.Active(ctx, KindPlan); len(plans) != 0 {
		t.Fatal("done plan not cleared")
	}
	var ops []string
	rows, _ := s.db.Query(`SELECT DISTINCT op FROM revisions`)
	for rows.Next() {
		var o string
		_ = rows.Scan(&o)
		ops = append(ops, o)
	}
	rows.Close()
	if !reflect.DeepEqual(ops, []string{"fold"}) {
		t.Fatalf("ops %v", ops)
	}
	mustChain(t, s)
}

func TestSessionLifecycle(t *testing.T) {
	s := open(t)
	for i := 0; i < 2; i++ {
		if err := s.SessionStart(ctx, "s-1", "codex", 1000); err != nil {
			t.Fatal(err)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM events WHERE kind = 'session_start' AND payload = '{}'`); n != 1 {
		t.Fatalf("%d session_start events", n)
	}
	_ = s.SessionEnd(ctx, "s-1", "codex", 2000)
	if n := count(t, s, `SELECT count(*) FROM sessions WHERE session_id = 's-1' AND started_at = 1000 AND ended_at = 2000`); n != 1 {
		t.Fatal("sessions row")
	}
}

func TestFTSQuery(t *testing.T) {
	for in, want := range map[string]string{
		"Postgres ledger":    `"postgres" OR "ledger"`,
		"a DB of the ledger": `"the" OR "ledger"`,
		"db io":              `"db" OR "io"`,
		"why? Café-crème!!":  `"why" OR "café" OR "crème"`,
		"   ":                ``,
		"dup dup DUP":        `"dup"`,
	} {
		if got := FTSQuery(in); got != want {
			t.Errorf("FTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSearchFiltersAndOrders(t *testing.T) {
	s := open(t)
	_, a, _ := s.Remember(ctx, agentWrite(KindDecision, "", "Cache tokens in redis", "s-1", 1000))
	_, b, _ := s.Remember(ctx, Write{Kind: KindDecision, Content: "Cache sessions in redis", Source: "capture", Confidence: Candidate, At: 2000})
	_, c, _ := s.Remember(ctx, agentWrite(KindFact, "", "Redis runs on port 6380", "s-1", 3000))
	hits, _ := s.Search(ctx, "redis", []string{KindDecision}, 10)
	if len(hits) != 2 || hits[0].Entry.ID != a.ID || hits[1].Entry.ID != b.ID {
		t.Fatalf("active before candidate: %+v", ids(hits))
	}
	_, _ = s.Feedback(ctx, a.ID, VerdictWrong, "", "x", "s-9", 4000)
	hits, _ = s.Search(ctx, "redis", nil, 10)
	for _, h := range hits {
		if h.Entry.ID == a.ID {
			t.Fatal("archived entry in search")
		}
	}
	newest, _ := s.Search(ctx, "", nil, 1)
	if len(newest) != 1 || newest[0].Entry.ID != a.ID { // the archive bumped updated_at; empty query lists newest non-archived
		if len(newest) != 1 || newest[0].Entry.ID != c.ID {
			t.Fatalf("empty query: %+v", ids(newest))
		}
	}
}

func ids(h []Hit) []int64 {
	var out []int64
	for _, x := range h {
		out = append(out, x.Entry.ID)
	}
	return out
}

func TestChangesPaging(t *testing.T) {
	s := open(t)
	for i := 0; i < 10; i++ {
		_, _, _ = s.Remember(ctx, agentWrite(KindFact, "", "fact number "+string(rune('a'+i)), "other", int64(1000+i)))
	}
	_, _, _ = s.Remember(ctx, agentWrite(KindFact, "", "my own fact", "me", 5000))
	c, err := s.ChangesSince(ctx, 0, "me")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Entries) != 8 || !c.More || c.SyncedTo != 1007 || c.Entries[0].UpdatedAt != 1007 {
		t.Fatalf("page 1: n=%d more=%v synced=%d", len(c.Entries), c.More, c.SyncedTo)
	}
	c, _ = s.ChangesSince(ctx, c.SyncedTo, "me")
	if len(c.Entries) != 2 || c.More || c.SyncedTo != 1009 {
		t.Fatalf("page 2: n=%d more=%v synced=%d", len(c.Entries), c.More, c.SyncedTo)
	}
}

func TestChainDetectsAHandEdit(t *testing.T) {
	s := open(t)
	for i := 0; i < 3; i++ {
		_, _, _ = s.Remember(ctx, agentWrite(KindFact, "", "fact "+string(rune('a'+i)), "s", int64(1000+i)))
	}
	mustChain(t, s)
	_, _ = s.db.Exec(`UPDATE revisions SET content = 'tampered' WHERE rev = 2`)
	rep, _ := s.VerifyChain()
	if rep.OK() || rep.FirstBad != 2 || rep.Revisions != 3 {
		t.Fatalf("%+v", rep)
	}
}

func TestRowBytesEncoding(t *testing.T) {
	b, err := rowBytes([]any{int64(1), nil, 0.5, "hé", []byte{9}})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		1, 8, 0, 0, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, // int 1
		0, 0, 0, 0, 0, 0, 0, 0, 0, // null
		2, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xe0, 0x3f, // 0.5
		3, 3, 0, 0, 0, 0, 0, 0, 0, 'h', 0xc3, 0xa9, // "hé"
		4, 1, 0, 0, 0, 0, 0, 0, 0, 9, // blob
	}
	if !reflect.DeepEqual(b, want) {
		t.Fatalf("got %v", b)
	}
}

func TestCitations(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	_ = os.MkdirAll(src, 0o755)
	body := "package a\n\nfunc TTL() int {\n\treturn 15\n}\n"
	_ = os.WriteFile(filepath.Join(src, "a.go"), []byte(body), 0o644)
	_ = os.WriteFile(filepath.Join(src, "bin"), []byte{0, 1, 2}, 0o644)
	roots := []string{mustReal(t, root)}
	sym := "TTL"
	c, err := Cite(roots, src, "a.go", "3-5", &sym)
	if err != nil || c.Path != "src/a.go" || c.Lines() != "3-5" || len(c.Hash) != 64 {
		t.Fatalf("%+v %v", c, err)
	}
	if v, _ := Check(roots[0], c); v != Valid {
		t.Fatalf("valid: %s", v)
	}
	_ = os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n\n// moved\n"+body[11:]), 0o644)
	v, moved := Check(roots[0], c)
	if v != Moved || moved.Lines() != "4-6" {
		t.Fatalf("moved: %s %s", v, moved.Lines())
	}
	_ = os.WriteFile(filepath.Join(src, "a.go"), []byte("package a\n"), 0o644)
	if v, _ := Check(roots[0], c); v != Stale {
		t.Fatalf("stale: %s", v)
	}
	_ = os.Remove(filepath.Join(src, "a.go"))
	if v, _ := Check(roots[0], c); v != Stale {
		t.Fatalf("missing: %s", v)
	}
	errs := map[string][2]string{
		"no lines":  {"a.go", ""},
		"bad lines": {"a.go", "x"},
		"not range": {"a.go", "5-3"},
		"too long":  {"a.go", "1-201"},
		"missing":   {"nope.go", "1"},
		"outside":   {"/etc/hosts", "1"},
		"binary":    {"bin", "1"},
		"past end":  {"short.go", "2-3"},
	}
	_ = os.WriteFile(filepath.Join(src, "short.go"), []byte("one\n"), 0o644)
	want := map[string]string{
		"no lines":  `give lines ("12-30"); the symbol is used to find them again later`,
		"bad lines": "bad lines `x`: give \"12-30\" or \"12\"",
		"not range": "lines 5-3 are not a range",
		"too long":  "a citation spans at most 200 lines",
		"missing":   "`nope.go` does not exist",
		"outside":   "`/etc/hosts` is outside the repository",
		"binary":    "`bin` can't be read as text",
		"past end":  "`short.go` has 1 lines",
	}
	for name, a := range errs {
		if _, err := Cite(roots, src, a[0], a[1], nil); err == nil || err.Error() != want[name] {
			t.Errorf("%s: %v", name, err)
		}
	}
	if Overall([]string{Valid, Moved}) != Moved || Overall([]string{Unverifiable, Unverifiable}) != Unverifiable ||
		Overall([]string{Valid, Unverifiable}) != Valid || Overall([]string{Moved, Stale}) != Stale {
		t.Fatal("overall")
	}
}

func mustReal(t *testing.T, p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestExportImportRoundTrip(t *testing.T) {
	src := open(t)
	_, _, _ = src.Remember(ctx, agentWrite(KindDecision, "db", "Use Postgres; not [MySQL]", "s-1", 1000))
	_, _, _ = src.Remember(ctx, agentWrite(KindPreference, "", "Answer in British English", "s-1", 1001))
	_, _, _ = src.Remember(ctx, agentWrite(KindFailure, "", "Vendoring node_modules broke CI", "s-1", 1002))
	_, _ = src.Fold(ctx, Event{Kind: KindFileChanged, Key: "a.go", Text: "Edit", At: 1003}) // working memory: not exported
	dir := filepath.Join(t.TempDir(), "amr")
	n, err := src.Export(ctx, dir)
	if err != nil || n != 3 {
		t.Fatalf("export %d %v", n, err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "decisions.md"))
	want := "# Decisions and why\n\n- Use Postgres; not [MySQL] [source: atlas-session:claude-code/s-1; added: 1970-01-01; atlas-id: 1; revision: 1]\n"
	if string(b) != want {
		t.Fatalf("decisions.md:\n%q\nwant\n%q", b, want)
	}
	idx, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if !strings.Contains(string(idx), "- [[decisions]] Decisions and why (1)") || !strings.Contains(string(idx), "## Preferences\n\n- Answer in British English") {
		t.Fatalf("MEMORY.md:\n%s", idx)
	}

	dst := open(t)
	res, err := dst.Import(ctx, dir, 5000)
	if err != nil || res.Imported != 3 || res.Skipped != 0 {
		t.Fatalf("import %+v %v", res, err)
	}
	all, _ := dst.List(ctx, "", 50)
	kinds := map[string]string{}
	for _, e := range all {
		kinds[e.Content] = e.Kind
		if e.State != StateCandidate || e.Source != "import:amr" || e.Agent != "" || e.Session != "" || e.Confidence != Candidate {
			t.Fatalf("not a candidate import: %+v", e)
		}
	}
	if kinds["Use Postgres; not [MySQL]"] != KindDecision || kinds["Answer in British English"] != KindPreference || kinds["Vendoring node_modules broke CI"] != KindFailure {
		t.Fatalf("kinds %v", kinds)
	}
	var op, note string
	_ = dst.db.QueryRow(`SELECT op, note FROM revisions WHERE rev = 1`).Scan(&op, &note)
	if op != OpImport || !strings.Contains(note, "imported from ") || !strings.Contains(note, "atlas-id: ") {
		t.Fatalf("op %s note %q", op, note)
	}
	if n := count(t, dst, `SELECT count(*) FROM events`); n != 0 {
		t.Fatalf("imports logged %d events", n)
	}
	again, _ := dst.Import(ctx, dir, 6000)
	if again.Imported != 0 || again.Skipped != 3 {
		t.Fatalf("re-import %+v", again)
	}
	mustChain(t, dst)
}
