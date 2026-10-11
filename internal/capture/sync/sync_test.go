package sync

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func ptr(s string) *string { return &s }

type tokens struct {
	gosync.Mutex
	invalidated int
}

func (t *tokens) Token(context.Context) (string, error) {
	t.Lock()
	defer t.Unlock()
	return "jwt-" + string(rune('a'+t.invalidated)), nil
}

func (t *tokens) Invalidate() { t.Lock(); t.invalidated++; t.Unlock() }

// cloudStore is a cloud-bound store with one session, a message whose body
// spilled to a blob, a tool call and a checkpoint.
func cloudStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.BindCloud(ctx, store.Binding{WorkspaceID: st.Root, Root: st.Root, Enabled: true,
		OrgID: ptr("org-1"), Slug: ptr("demo"), RemoteWorkspaceID: ptr("ws-1")}, t0); err != nil {
		t.Fatal(err)
	}
	sid, err := st.UpsertSession(ctx, store.SessionKey{WorkspaceID: st.Root, Source: "acp", NativeID: "n-1"},
		ptr("claude-code"), ptr("claude-opus"), ptr(st.Root), nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetTitleIfAbsent(ctx, sid, "Fix the build")
	_ = st.BeginTurn(ctx, sid, 1, t0)
	if _, err := st.RecordMessage(ctx, store.Message{SessionID: sid, TurnSeq: 1, NativeID: "p1", Role: "user", Mode: "text",
		Body: strings.Repeat("x", store.SpillBytes+1), CreatedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.UpsertToolCall(ctx, store.ToolCall{SessionID: sid, TurnSeq: 1, NativeID: "tu1", ToolName: "Bash",
		Title: ptr("ls"), Kind: ptr("execute"), Status: "completed", Locations: `[{"path":"a.go"}]`,
		Arguments: ptr(`{"command":"ls"}`), Result: ptr("a.go")}, t0); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertCheckpoint(ctx, store.Checkpoint{SessionID: sid, CommitSHA: "c0ffee", PatchID: ptr("p"),
		Branch: ptr("main"), AuthorName: ptr("A"), AuthorEmail: ptr("a@x"), FilesTouched: []string{"a.go"},
		Insertions: 2, Deletions: 1}, t0); err != nil {
		t.Fatal(err)
	}
	return st, sid
}

type recorded struct {
	method, path, auth string
	body               []byte
}

type fake struct {
	gosync.Mutex
	calls  []recorded
	answer func(r recorded, n int) (int, http.Header, string)
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	f.Lock()
	rec := recorded{r.Method, r.URL.Path, r.Header.Get("Authorization"), b}
	f.calls = append(f.calls, rec)
	n := len(f.calls)
	f.Unlock()
	code, hdr, body := 200, http.Header{}, ""
	if f.answer != nil {
		code, hdr, body = f.answer(rec, n)
	}
	for k, v := range hdr {
		w.Header()[k] = v
	}
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

func drain(t *testing.T, st *store.Store, f *fake, tk *tokens) Report {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	rep, err := Drain(context.Background(), NewClient(srv.URL, tk), st, time.Minute, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func counts(t *testing.T, st *store.Store) map[string]int64 {
	t.Helper()
	c, err := st.OutboxCounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestArtifactsExactShape(t *testing.T) {
	st, sid := cloudStore(t)
	o, err := st.Pending(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	items := Target{OrgID: "org-1", WorkspaceID: "ws-1"}.items(o)
	if len(items) != 4 {
		t.Fatalf("%d items", len(items))
	}
	s := o.Sessions[0]
	wantSession := `{"type":"agent_session","rowId":"` + sid + `","orgId":"org-1","workspaceId":"ws-1","seq":0,` +
		`"contentHash":"` + hashOf(sid+":Fix the build:claude-code:claude-opus:"+s.TokenTotals) + `","createdAt":"` + s.StartedAt + `",` +
		`"sessionId":"` + sid + `","source":"acp","nativeSessionId":"n-1","title":"Fix the build","agent":"claude-code",` +
		`"model":"claude-opus","tokenTotals":{"input_tokens":0,"output_tokens":0,"cache_creation_tokens":0,"cache_read_tokens":0,"reasoning_tokens":0},` +
		`"startedAt":"` + s.StartedAt + `"}`
	if s.TokenTotals != "{}" && string(items[0].artifact) != wantSession {
		t.Fatalf("session artifact\n got %s\nwant %s", items[0].artifact, wantSession)
	}
	if s.TokenTotals == "{}" && !strings.Contains(string(items[0].artifact), `"tokenTotals":{}`) {
		t.Fatalf("session artifact %s", items[0].artifact)
	}
	m := o.Messages[0]
	if m.Body != nil || m.BodyRef == nil || len(items[1].blobs) != 1 || items[1].blobs[0] != *m.BodyRef {
		t.Fatalf("spilled body: %+v", items[1])
	}
	wantMsg := `{"type":"agent_message","rowId":"` + m.ID + `","orgId":"org-1","workspaceId":"ws-1","seq":` + itoa(m.Seq) +
		`,"contentHash":"` + m.ContentHash + `","createdAt":"` + m.CreatedAt + `","sessionId":"` + sid +
		`","turnSeq":1,"role":"user","mode":"text","preview":"` + m.Preview + `","body":null,"bodyRef":"` + *m.BodyRef + `"}`
	if string(items[1].artifact) != wantMsg {
		t.Fatalf("message artifact\n got %.300s", items[1].artifact)
	}
	tc := o.ToolCalls[0]
	wantTC := `{"type":"tool_call","rowId":"` + tc.ID + `","orgId":"org-1","workspaceId":"ws-1","seq":` + itoa(tc.Seq) +
		`,"contentHash":"` + hashOf(tc.ID+":completed::a.go") + `","createdAt":"` + tc.CreatedAt + `","sessionId":"` + sid +
		`","turnSeq":1,"toolName":"Bash","title":"ls","kind":"execute","status":"completed","locations":[{"path":"a.go"}],` +
		`"arguments":"{\"command\":\"ls\"}","argumentsRef":null,"result":"a.go","resultRef":null,"resultBinary":false}`
	if string(items[2].artifact) != wantTC {
		t.Fatalf("tool call artifact\n got %s\nwant %s", items[2].artifact, wantTC)
	}
	cp := o.Checkpoints[0]
	wantCP := `{"type":"checkpoint","rowId":"` + cp.ID + `","orgId":"org-1","workspaceId":"ws-1","seq":0,` +
		`"contentHash":"` + hashOf(sid+":c0ffee:linked") + `","createdAt":"` + cp.CreatedAt + `","sessionId":"` + sid +
		`","commitSha":"c0ffee","patchId":"p","linkState":"linked","branch":"main","gitAuthorName":"A","gitAuthorEmail":"a@x",` +
		`"filesTouched":["a.go"],"insertions":2,"deletions":1}`
	if string(items[3].artifact) != wantCP {
		t.Fatalf("checkpoint artifact\n got %s\nwant %s", items[3].artifact, wantCP)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestHashFormulasNullsAreEmpty(t *testing.T) {
	r := store.OutSession{ID: "as-1", TokenTotals: "{}"}
	if SessionHash(r) != hashOf("as-1::::{}") {
		t.Fatal("session hash with nulls")
	}
	if ToolCallHash(store.OutToolCall{ID: "tc-1", Status: "running"}) != hashOf("tc-1:running::") {
		t.Fatal("tool call hash with nulls")
	}
}

func TestBlobsBeforeIngestAndAllSent(t *testing.T) {
	st, _ := cloudStore(t)
	f := &fake{}
	tk := &tokens{}
	rep := drain(t, st, f, tk)
	if rep.Outcome != Done || rep.Sent != 4 {
		t.Fatalf("%+v", rep)
	}
	if len(f.calls) != 2 || f.calls[0].method != http.MethodPut || !strings.HasPrefix(f.calls[0].path, "/blobs/") ||
		f.calls[1].path != "/ingest" {
		t.Fatalf("calls %+v", f.calls)
	}
	if f.calls[0].auth != "Bearer jwt-a" {
		t.Fatal("bearer")
	}
	var body struct {
		Artifacts []map[string]any `json:"artifacts"`
	}
	if err := json.Unmarshal(f.calls[1].body, &body); err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, a := range body.Artifacts {
		types = append(types, a["type"].(string))
	}
	if strings.Join(types, ",") != "agent_session,agent_message,tool_call,checkpoint" {
		t.Fatalf("order %v", types)
	}
	if c := counts(t, st); c["sent"] != 4 || c["pending"] != 0 {
		t.Fatalf("counts %v", c)
	}
	// A changed row goes back to pending and is sent again.
	o, _ := st.Pending(context.Background(), 10)
	if o.Len() != 0 {
		t.Fatal("nothing pending after a drain")
	}
}

func TestBatchesOfAtMost100(t *testing.T) {
	st, sid := cloudStore(t)
	ctx := context.Background()
	for i := range 230 {
		_, _ = st.RecordMessage(ctx, store.Message{SessionID: sid, TurnSeq: 1, NativeID: "m" + itoa(int64(i)), Role: "assistant",
			Mode: "text", Body: "hi", CreatedAt: t0})
	}
	f := &fake{}
	rep := drain(t, st, f, &tokens{})
	var sizes []int
	for _, c := range f.calls {
		if c.path == "/ingest" {
			var b struct{ Artifacts []json.RawMessage }
			_ = json.Unmarshal(c.body, &b)
			sizes = append(sizes, len(b.Artifacts))
			if len(c.body) > MaxBatch {
				t.Fatal("batch over 4 MiB")
			}
		}
	}
	if rep.Sent != 234 || len(sizes) != 3 || sizes[0] != 100 || sizes[1] != 100 || sizes[2] != 34 {
		t.Fatalf("sent %d sizes %v", rep.Sent, sizes)
	}
}

func TestRefreshOnceOn401(t *testing.T) {
	st, _ := cloudStore(t)
	tk := &tokens{}
	f := &fake{answer: func(r recorded, _ int) (int, http.Header, string) {
		if r.auth == "Bearer jwt-a" {
			return 401, nil, ""
		}
		return 200, nil, ""
	}}
	if rep := drain(t, st, f, tk); rep.Outcome != Done || tk.invalidated != 1 {
		t.Fatalf("%+v invalidated %d", rep, tk.invalidated)
	}
	st2, _ := cloudStore(t)
	tk2 := &tokens{}
	always := &fake{answer: func(recorded, int) (int, http.Header, string) { return 401, nil, "" }}
	if rep := drain(t, st2, always, tk2); rep.Outcome != SignedOut || tk2.invalidated != 1 {
		t.Fatalf("%+v invalidated %d", rep, tk2.invalidated)
	}
}

func TestForbiddenStopsDraining(t *testing.T) {
	st, _ := cloudStore(t)
	f := &fake{answer: func(recorded, int) (int, http.Header, string) { return 403, nil, "" }}
	if rep := drain(t, st, f, &tokens{}); rep.Outcome != NotAuthorized {
		t.Fatalf("%+v", rep)
	}
	b, _ := st.Binding(context.Background())
	if b.DrainState != store.DrainNotAuthorized {
		t.Fatal("drain_state")
	}
	n := len(f.calls)
	if rep := drain(t, st, f, &tokens{}); rep.Outcome != NotAuthorized || len(f.calls) != n {
		t.Fatal("a not_authorized binding must not call the server")
	}
}

func TestRateLimitAndBackoff(t *testing.T) {
	st, _ := cloudStore(t)
	f := &fake{answer: func(recorded, int) (int, http.Header, string) {
		return 429, http.Header{"Retry-After": {"120"}}, ""
	}}
	rep := drain(t, st, f, &tokens{})
	if rep.Outcome != RateLimited || rep.RetryAfter != 2*time.Minute {
		t.Fatalf("%+v", rep)
	}
	var b Backoff
	if d := b.Next(rep.RetryAfter); d != 2*time.Minute {
		t.Fatalf("Retry-After lengthens: %v", d)
	}
	if d := b.Next(0); d != time.Minute {
		t.Fatalf("doubled %v", d)
	}
	if d := b.Next(5 * time.Second); d != 2*time.Minute {
		t.Fatalf("Retry-After never shortens: %v", d)
	}
	for range 10 {
		b.Next(0)
	}
	if d := b.Next(0); d != MaxBackoff {
		t.Fatalf("cap %v", d)
	}
	b.Reset()
	if d := b.Next(0); d != MinBackoff {
		t.Fatal("reset")
	}
}

func TestServerErrorIsOffline(t *testing.T) {
	st, _ := cloudStore(t)
	f := &fake{answer: func(recorded, int) (int, http.Header, string) { return 503, nil, "" }}
	if rep := drain(t, st, f, &tokens{}); rep.Outcome != Offline {
		t.Fatalf("%+v", rep)
	}
	if c := counts(t, st); c["pending"] != 4 {
		t.Fatalf("rows stay pending: %v", c)
	}
}

func TestRejectedBatchBisectsToThePoisonRow(t *testing.T) {
	st, _ := cloudStore(t)
	f := &fake{answer: func(r recorded, _ int) (int, http.Header, string) {
		if r.path == "/ingest" && strings.Contains(string(r.body), `"type":"tool_call"`) {
			return 422, nil, `{"error":{"code":"bad","message":"no"}}`
		}
		return 200, nil, ""
	}}
	rep := drain(t, st, f, &tokens{})
	if rep.Sent != 3 || rep.Failed != 1 {
		t.Fatalf("%+v", rep)
	}
	if c := counts(t, st); c["failed"] != 1 || c["sent"] != 3 {
		t.Fatalf("%v", c)
	}
	n, _ := st.RetryFailed(context.Background())
	if n != 1 {
		t.Fatal("retry requeues the failed row")
	}
}

func TestNotAcceptedCountsAnAttemptAndRetiresAtFive(t *testing.T) {
	st, sid := cloudStore(t)
	f := &fake{answer: func(r recorded, _ int) (int, http.Header, string) {
		if r.path != "/ingest" {
			return 200, nil, ""
		}
		return 200, nil, `{"results":[{"rowId":"` + sid + `","accepted":false,"reason":"x"}]}`
	}}
	for range 5 {
		drain(t, st, f, &tokens{})
	}
	if c := counts(t, st); c["pending"] != 1 {
		t.Fatalf("after 5 attempts the session is still pending until the next drain: %v", c)
	}
	rep := drain(t, st, f, &tokens{})
	if rep.Retired != 1 {
		t.Fatalf("%+v", rep)
	}
	if c := counts(t, st); c["failed"] != 1 {
		t.Fatalf("%v", c)
	}
}

func TestChangedDuringUploadStaysPending(t *testing.T) {
	st, sid := cloudStore(t)
	ctx := context.Background()
	f := &fake{answer: func(r recorded, _ int) (int, http.Header, string) {
		if r.path == "/ingest" {
			_ = st.RecordUsage(ctx, sid, 1, nil, store.TokenTotals{Input: 5}, t0)
		}
		return 200, nil, ""
	}}
	drain(t, st, f, &tokens{})
	o, _ := st.Pending(ctx, 10)
	if len(o.Sessions) != 1 {
		t.Fatal("a session whose totals changed mid-upload must send again")
	}
}

func TestMissingBlobFailsTheRow(t *testing.T) {
	st, _ := cloudStore(t)
	o, _ := st.Pending(context.Background(), 10)
	key := *o.Messages[0].BodyRef
	_ = removeBlob(st, key)
	rep := drain(t, st, &fake{}, &tokens{})
	if rep.Failed != 1 || rep.Sent != 3 {
		t.Fatalf("%+v", rep)
	}
}

func TestBlob413FailsTheRow(t *testing.T) {
	st, _ := cloudStore(t)
	f := &fake{answer: func(r recorded, _ int) (int, http.Header, string) {
		if r.method == http.MethodPut {
			return 413, nil, ""
		}
		return 200, nil, ""
	}}
	if rep := drain(t, st, f, &tokens{}); rep.Failed != 1 || rep.Sent != 3 {
		t.Fatalf("%+v", rep)
	}
}

func TestLocalBindingIsIdle(t *testing.T) {
	st, err := store.Open(t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.PutBinding(context.Background(), store.Binding{WorkspaceID: st.Root, Root: st.Root, Mode: store.ModeLocal, Enabled: true}, t0)
	f := &fake{}
	if rep := drain(t, st, f, &tokens{}); rep.Outcome != Idle || len(f.calls) != 0 {
		t.Fatal("local mode never touches the network")
	}
}

func removeBlob(st *store.Store, key string) error {
	return os.Remove(filepath.Join(st.Root, store.DirName, "blobs", key[:2], key))
}
