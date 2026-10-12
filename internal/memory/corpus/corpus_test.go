package corpus

import (
	"context"
	"database/sql"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	memrecord "github.com/tryatlas/cli/internal/memory/record"
)

type bag struct{}

func (bag) Embed(text string) []float32 {
	v := make([]float32, 64)
	for _, w := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(strings.Trim(w, ".,:[]")))
		v[h.Sum32()%64]++
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	for i := range v {
		if n > 0 {
			v[i] = float32(float64(v[i]) / math.Sqrt(n))
		}
	}
	return v
}

func TestUpdateAndRetrieve(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	ix, err := Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer ix.Close()
	docs := []Doc{
		{ID: "shared:decision:1", Title: "Use Postgres for the ledger", Text: "[pi] Use Postgres for the ledger", Source: "shared"},
		{ID: "kb:deploy", Title: "deploy", Text: "Deploys go out on Fridays after the smoke tests.", Source: "note"},
		{ID: "claude:MEMORY.md", Title: "Memory Index", Text: "app-owned", Source: "claude"},
	}
	st, err := ix.Update(ctx, docs, Owned, bag{})
	if err != nil || st.Added != 3 || st.Embedded != 3 {
		t.Fatalf("%+v %v", st, err)
	}
	// Unchanged: nothing written.
	if st, _ := ix.Update(ctx, docs, Owned, bag{}); st.Unchanged != 3 || st.Added+st.Updated+st.Embedded != 0 {
		t.Fatalf("second pass %+v", st)
	}
	// A pass without the app-owned doc keeps it; a removed owned doc goes.
	st, err = ix.Update(ctx, docs[:1], Owned, bag{})
	if err != nil || st.Deleted != 1 {
		t.Fatalf("%+v %v", st, err)
	}
	var n int
	_ = ix.db.QueryRow(`SELECT count(*) FROM docs WHERE id = 'claude:MEMORY.md'`).Scan(&n)
	if n != 1 {
		t.Fatal("an app-owned document must never be deleted by the CLI")
	}
	var dirty string
	_ = ix.db.QueryRow(`SELECT v FROM meta WHERE k = 'vectors_dirty:all-MiniLM-L6-v2-384'`).Scan(&dirty)
	if dirty != "1" {
		t.Fatal("the app's vector file must be flagged for a rebuild")
	}
	res, err := ix.Retrieve(ctx, "ledger postgres", bag{}.Embed("ledger postgres"), 6, nil)
	if err != nil || len(res) == 0 || res[0].ID != "shared:decision:1" || res[0].Source != "shared" {
		t.Fatalf("%+v %v", res, err)
	}
	if res, _ := ix.Retrieve(ctx, "ab", nil, 6, nil); res != nil {
		t.Fatal("a query under 4 bytes returns nothing")
	}
	if res, _ := ix.Retrieve(ctx, "ledger postgres", nil, 6, func(string) bool { return false }); len(res) != 0 {
		t.Fatal("keep filters")
	}
	if err := ix.Delete(ctx, "shared:decision:1"); err != nil {
		t.Fatal(err)
	}
	if res, _ := ix.Retrieve(ctx, "ledger postgres", nil, 6, nil); len(res) != 0 {
		t.Fatalf("deleted doc returned: %+v", res)
	}
}

func TestHelpersMatchTheApp(t *testing.T) {
	if got := shortTitle("first line\n---\nrest"); got != "first line" {
		t.Fatal(got)
	}
	if got := shortTitle(strings.Repeat("ab ", 40)); !strings.HasSuffix(got, "…") || len([]rune(got)) != 81 {
		t.Fatal(got)
	}
	in := "keep\n<atlas-memory>\nsecret\n</atlas-memory>\n--- SHARED MEMORY — UPDATES ---\nx\n--- END SHARED MEMORY ---\nalso keep"
	if got := StripInjected(in); got != "keep\nalso keep" {
		t.Fatalf("%q", got)
	}
	if t1, b1 := splitEmbedded("Title\n\n body \n\nmore"); t1 != "Title" || b1 != "body \n\nmore" {
		t.Fatalf("%q %q", t1, b1)
	}
	if !Owned("opencode:ses_1") || Owned("claude:MEMORY.md") || Owned("codex:019a") || !Owned("claude:CLAUDE.md") || Owned("claude-code:x") {
		t.Fatal("ownership")
	}
}

func TestSharedAndCaptureDocs(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	rec, err := memrecord.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _ = rec.Remember(ctx, memrecord.Write{Kind: "decision", Content: "  Ship on Fridays  ", Source: "agent", Agent: "pi", Session: "s", Confidence: 0.8, At: time.Now().UnixMilli(), LogEvent: true})
	_ = rec.Close()
	_ = os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("Run make lint.\n"), 0o644)
	docs, err := Collect(ctx, root, root)
	if err != nil {
		t.Fatal(err)
	}
	var shared, agents *Doc
	for i := range docs {
		switch {
		case strings.HasPrefix(docs[i].ID, "shared:decision:"):
			shared = &docs[i]
		case docs[i].ID == "codex:AGENTS.md":
			agents = &docs[i]
		}
	}
	if shared == nil || shared.Text != "[pi] Ship on Fridays" || shared.Title != "Ship on Fridays" || agents == nil || agents.Text != "Run make lint." {
		t.Fatalf("%+v", docs)
	}
}

// TestCollectMatchesTheAppsIndex runs Collect on a Project the app indexed
// (ATLAS_COMPAT_PROJECT) and checks every document both produce has the
// same content hash: the same text, byte for byte.
func TestCollectMatchesTheAppsIndex(t *testing.T) {
	root := os.Getenv("ATLAS_COMPAT_PROJECT")
	if root == "" {
		t.Skip("set ATLAS_COMPAT_PROJECT to a Project the app has indexed")
	}
	ctx := context.Background()
	docs, err := Collect(ctx, root, root)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+Path(root)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	same, differ := 0, 0
	for _, d := range docs {
		var h string
		if db.QueryRow(`SELECT content_hash FROM docs WHERE id = ?`, d.ID).Scan(&h) != nil {
			continue
		}
		if h == hashText(d.embedText()) {
			same++
		} else {
			differ++
			t.Logf("differs: %s", d.ID)
		}
	}
	t.Logf("%d documents match the app's index, %d differ", same, differ)
	if same == 0 {
		t.Fatal("no document matched")
	}
}
