// Package git links captured sessions to commits.
package git

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

type fixture struct {
	past bool // date commits an hour ago (before the session)
	t    *testing.T
	root string
	st   *store.Store
	rec  *record.Recorder
	sess record.Session
	n    int
}

func (f *fixture) git(args ...string) string {
	f.t.Helper()
	c := exec.Command("git", append([]string{"-C", f.root, "-c", "user.name=Dev", "-c", "user.email=dev@e.test", "-c", "commit.gpgsign=false"}, args...)...)
	when := time.Now().Add(time.Hour)
	if f.past {
		when = time.Now().Add(-time.Hour)
	}
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_COMMITTER_DATE="+when.Format(time.RFC3339), "GIT_AUTHOR_DATE="+when.Format(time.RFC3339))
	out, err := c.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *fixture) write(p, s string) {
	_ = os.MkdirAll(filepath.Dir(filepath.Join(f.root, p)), 0o755)
	if err := os.WriteFile(filepath.Join(f.root, p), []byte(s), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// agentWrites records a completed Write tool call for path with content.
func (f *fixture) agentWrites(p, s string) {
	f.write(p, s)
	f.n++
	id := "toolu_" + string(rune('a'+f.n))
	args, _ := json.Marshal(map[string]string{"file_path": filepath.Join(f.root, p), "content": s})
	ctx := context.Background()
	_ = f.rec.ToolCall(ctx, f.sess, record.Tool{NativeID: id, Name: "Write", Status: "running", Arguments: args})
	_ = f.rec.ToolCall(ctx, f.sess, record.Tool{NativeID: id, Name: "Write", Status: "completed"})
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "w")
	_ = os.MkdirAll(dir, 0o755)
	root, _ := filepath.EvalSymlinks(dir)
	f := &fixture{t: t, root: root, past: true}
	f.git("init", "-q", "-b", "main")
	f.write("existing.go", "package main\n")
	f.git("add", ".")
	f.git("commit", "-q", "-m", "base")
	f.past = false
	st, err := store.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = st.PutBinding(context.Background(), store.Binding{WorkspaceID: root, Root: root, Mode: store.ModeLocal, Enabled: true}, time.Now())
	f.st = st
	f.rec = record.New(st, root)
	f.sess = record.Session{Agent: "claude-code", NativeID: "sess-1", Cwd: root}
	if _, err := f.rec.Prompt(context.Background(), f.sess, "do work"); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) link() Result {
	f.t.Helper()
	r, err := Link(context.Background(), f.st, f.root, time.Now)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func q(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	var s sql.NullString
	if err := db.QueryRow(query).Scan(&s); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return s.String
}

func TestLinkRules(t *testing.T) {
	f := newFixture(t)
	db := f.st.DB()
	f.agentWrites("new.go", "package main\n\nfunc New() {}\n")                // exact hash
	f.agentWrites("existing.go", "package main\n\nfunc Changed() {}\n")       // existed before: path alone
	f.agentWrites("edited.go", "a := 1\nb := 2\nc := 3\nd := 4\n")            // the user edits it before committing
	f.write("edited.go", "a := 1\nb := 2\nc := 3\nd := 4\ne := 5 // human\n") // keeps all four agent lines
	f.write("human.go", "package human\n")                                    // never touched by the agent
	f.git("add", ".")
	f.git("commit", "-q", "-m", "agent work")
	sha := f.git("rev-parse", "HEAD")

	r := f.link()
	if r.Checkpoints != 1 {
		t.Fatalf("%+v", r)
	}
	if got := q(t, db, `SELECT commit_sha || '|' || files_touched || '|' || link_state || '|' || branch || '|' || git_author_name || '|' || (patch_id IS NOT NULL) || '|' || insertions FROM checkpoint`); got !=
		sha+`|["edited.go","existing.go","new.go"]|linked|main|Dev|1|11` { // insertions are the whole commit's
		t.Fatal(got)
	}
	if got := q(t, db, `SELECT count(*) FROM file_touch WHERE consumed_by_commit = '`+sha+`'`); got != "3" {
		t.Fatalf("consumed %s", got)
	}
	if got := q(t, db, `SELECT last_seen_commit FROM workspace_cursor`); got != sha {
		t.Fatalf("cursor %s", got)
	}
	// Walking again adds nothing.
	if r := f.link(); r.Commits != 0 || r.Checkpoints != 0 {
		t.Fatalf("second walk %+v", r)
	}
	// A human-only commit links nothing but moves the cursor.
	f.write("human.go", "package human\n// more\n")
	f.git("commit", "-q", "-am", "human")
	if r := f.link(); r.Commits != 1 || r.Checkpoints != 0 {
		t.Fatalf("human commit %+v", r)
	}
}

func TestStrictRuleRejectsRewrittenFile(t *testing.T) {
	f := newFixture(t)
	f.agentWrites("gen.go", "line one\nline two\nline three\nline four\n")
	f.write("gen.go", "totally\ndifferent\ncontent\n") // nothing of the agent's survives
	f.git("add", ".")
	f.git("commit", "-q", "-m", "rewrite")
	if r := f.link(); r.Checkpoints != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestNoCursorNoCandidatesSetsCursorToHead(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "w")
	_ = os.MkdirAll(dir, 0o755)
	root, _ := filepath.EvalSymlinks(dir)
	f := &fixture{t: t, root: root}
	f.git("init", "-q", "-b", "main")
	f.write("a", "a\n")
	f.git("add", ".")
	f.git("commit", "-q", "-m", "a")
	st, _ := store.Open(root, true)
	defer st.Close()
	f.st = st
	if r := f.link(); r.Commits != 0 {
		t.Fatalf("%+v", r)
	}
	if got := q(t, st.DB(), `SELECT last_seen_commit FROM workspace_cursor`); got != f.git("rev-parse", "HEAD") {
		t.Fatal(got)
	}
}

func TestMergeLinksItsSideCommits(t *testing.T) {
	f := newFixture(t)
	f.git("checkout", "-q", "-b", "feature")
	f.agentWrites("feature.go", "package feature\n\nfunc F() {}\n")
	f.git("add", ".")
	f.git("commit", "-q", "-m", "feature work")
	side := f.git("rev-parse", "HEAD")
	f.git("checkout", "-q", "main")
	f.write("main.go", "package main\n")
	f.git("add", ".")
	f.git("commit", "-q", "-m", "main work")
	f.git("merge", "-q", "--no-ff", "feature", "-m", "merge")
	if r := f.link(); r.Checkpoints != 1 {
		t.Fatalf("%+v", r)
	}
	if got := q(t, f.st.DB(), `SELECT commit_sha FROM checkpoint`); got != side {
		t.Fatalf("linked %s, want the side commit %s", got, side)
	}
}

func TestRetainsWork(t *testing.T) {
	s := func(x string) *string { return &x }
	if !retainsWork(s("a,b,c,d"), s("a,b,x")) || retainsWork(s("a,b,c,d"), s("a,x")) == false && false {
		t.Fatal("half kept should link")
	}
	if retainsWork(s("a,b,c,d"), s("a,x,y")) || retainsWork(nil, s("a")) {
		t.Fatal("under half must not link")
	}
}
