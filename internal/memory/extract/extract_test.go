package extract

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	memrecord "github.com/tryatlas/atlas-cli/internal/memory/record"
)

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "atlas-state-")
	_ = os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// gateway answers /models and /chat/completions like the Atlas gateway.
func gateway(t *testing.T, seen *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("atlas-org") != "org_1" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"big","default":false},{"id":"small","default":true}]}`)
		case "/v1/chat/completions":
			var req struct {
				Model    string                     `json:"model"`
				Messages []struct{ Content string } `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			*seen = req.Model + "\n" + req.Messages[0].Content
			fmt.Fprint(w, `{"choices":[{"message":{"content":"`+"```json\\n"+`{\"entries\":[{\"kind\":\"decision\",\"content\":\"Use the streaming parser for large files.\",\"confidence\":0.8},{\"kind\":\"bogus\",\"content\":\"x\"}]}`+"\\n```"+`"}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestExtractGatesAndWrites(t *testing.T) {
	ctx := context.Background()
	root, _ := os.MkdirTemp("", "atlas-x-")
	defer os.RemoveAll(root)
	st, err := store.Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.PutBinding(ctx, store.Binding{WorkspaceID: root, Root: root, Mode: store.ModeLocal, Enabled: true}, time.Now())
	rec := record.New(st, root)
	sess := record.Session{Agent: "claude-acp", NativeID: "s-x", Cwd: root}
	for i := 0; i < 3; i++ {
		args, _ := json.Marshal(map[string]string{"command": "go test"})
		if _, err := rec.RecordTurn(ctx, sess, record.CapturedTurn{Prompt: fmt.Sprintf("step %d", i),
			Items: []record.CapturedItem{{Mode: "text", Body: "ran the tests <system-reminder>hidden</system-reminder>"},
				{Tool: &record.Tool{NativeID: fmt.Sprintf("t%d", i), Name: "Bash", Arguments: args, Status: "completed"}}}}); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Close()
	var seen string
	gw := gateway(t, &seen)
	defer gw.Close()
	o := Options{Root: root, Agent: "claude-acp", Session: "s-x", Gateway: gw.URL + "/v1",
		Auth: func(context.Context) (string, string, error) { return "tok", "org_1", nil }}

	// 3 turns: under the mid-session gate.
	if _, err := Run(ctx, o); !errors.Is(err, ErrSkipped) {
		t.Fatalf("mid-session pass = %v, want skipped", err)
	}
	// Session end: extract whatever is new.
	o.Final = true
	n, err := Run(ctx, o)
	if err != nil || n != 1 {
		t.Fatalf("final pass = %d %v", n, err)
	}
	if !strings.HasPrefix(seen, "small\n") || !strings.Contains(seen, "User: step 2") || !strings.Contains(seen, "[tool Bash]") || strings.Contains(seen, "hidden") {
		t.Fatalf("request = %q", seen)
	}
	mem, err := memrecord.Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	es, _ := mem.Active(ctx, "decision")
	if len(es) != 1 || es[0].Source != "extractor" || es[0].Agent != "claude-acp" || es[0].Session != "s-x" {
		t.Fatalf("entries = %+v", es)
	}
	// Nothing new since: skipped.
	if _, err := Run(ctx, o); !errors.Is(err, ErrSkipped) {
		t.Fatalf("repeat = %v", err)
	}
}

func TestLocalSummariserSendsNothing(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(root+"/.atlas", 0o755)
	_ = os.WriteFile(root+"/.atlas/memory-summarizer.json", []byte(`{"mode":"local"}`), 0o644)
	called := false
	o := Options{Root: root, Session: "s", Final: true, Gateway: "http://127.0.0.1:1",
		Auth: func(context.Context) (string, string, error) { called = true; return "", "", nil }}
	if _, err := Run(context.Background(), o); !errors.Is(err, ErrSkipped) || called {
		t.Fatalf("local mode: %v called=%v", err, called)
	}
}
