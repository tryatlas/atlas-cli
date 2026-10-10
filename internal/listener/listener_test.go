package listener

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
)

func TestHookPromptIsTheRecordedPrompt(t *testing.T) {
	tmp, _ := os.MkdirTemp("/tmp", "atlr") // short path: the socket name must fit
	defer os.RemoveAll(tmp)
	t.Setenv("XDG_RUNTIME_DIR", tmp)
	root := filepath.Join(tmp, "w")
	_ = os.MkdirAll(root, 0o755)
	_ = exec.Command("git", "-C", root, "init", "-q").Run()
	root, _ = filepath.EvalSymlinks(root)
	st, _ := store.Open(root, true)
	defer st.Close()
	_ = st.PutBinding(context.Background(), store.Binding{WorkspaceID: root, Root: root, Mode: store.ModeLocal, Enabled: true}, time.Now())

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	l, err := Start(root, "test", Options{Recorder: record.New(st, root), AnthropicUpstream: u})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Stop() }()
	if rf := ReadRunFile(root); rf == nil || rf.Port != l.Port || rf.Socket != l.Socket {
		t.Fatalf("run file %+v", rf)
	}

	sock := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", l.Socket)
	}}}
	sid := "4b7a3c2e-1f0d-4e5a-9b8c-7d6e5f4a3b2c"
	for _, ev := range []HookEvent{
		{Agent: "claude", Event: "session-start", SessionID: sid, Cwd: root},
		{Agent: "claude", Event: "user-prompt-submit", SessionID: sid, Cwd: root, Prompt: "Exactly what I typed"},
	} {
		b, _ := json.Marshal(ev)
		resp, err := sock.Post("http://atlas/hook", "application/json", bytes.NewReader(b))
		if err != nil || resp.StatusCode != http.StatusNoContent {
			t.Fatalf("hook: %v %v", resp, err)
		}
		_ = resp.Body.Close()
	}
	body := `{"model":"m","tools":[{"name":"Read"}],"metadata":{"user_id":"user_x_session_` + sid + `"},` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>r</system-reminder>Exactly what I typed"}]}]}`
	resp, err := http.Post("http://127.0.0.1:"+itoa(l.Port)+"/proxy/anthropic/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var got string
	for i := 0; i < 50; i++ {
		var s sql.NullString
		_ = st.DB().QueryRow(`SELECT body FROM agent_message WHERE role = 'user'`).Scan(&s)
		if got = s.String; got != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got != "Exactly what I typed" {
		t.Fatalf("prompt %q", got)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
