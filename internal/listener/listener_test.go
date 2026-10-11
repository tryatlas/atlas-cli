package listener

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/memory/server"
)

type syncer struct{ forced, retried int }

func (s *syncer) Force() { s.forced++ }
func (s *syncer) RetryFailed(context.Context) (int64, error) {
	s.retried++
	return 3, nil
}

func start(t *testing.T, opt Options) *Listener {
	t.Helper()
	tmp, _ := os.MkdirTemp("/tmp", "atlr") // short path: the socket name must fit
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	t.Setenv("XDG_RUNTIME_DIR", tmp)
	root := filepath.Join(tmp, "w")
	_ = os.MkdirAll(root, 0o755)
	_ = exec.Command("git", "-C", root, "init", "-q").Run()
	root, _ = filepath.EvalSymlinks(root)
	l, err := Start(root, "test", opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Stop() })
	return l
}

func TestRunFileAndBearerOnlyMCP(t *testing.T) {
	l := start(t, Options{})
	if rf := ReadRunFile(l.Root); rf == nil || rf.Port != l.Port || rf.Socket != l.Socket || rf.Daemon {
		t.Fatalf("run file %+v", rf)
	}
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	resp, err := http.Post(l.MCPURL(), "application/json", strings.NewReader(init))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no bearer: %d", resp.StatusCode)
	}
	tok := l.Tokens.Mint(server.Grant{SessionID: "s", Agent: "test-agent", Cwd: l.Root})
	req, _ := http.NewRequest(http.MethodPost, l.MCPURL(), strings.NewReader(init))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Contains(b, []byte(`"serverInfo"`)) {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	if err := l.Stop(); err != nil {
		t.Fatal(err)
	}
	if ReadRunFile(l.Root) != nil {
		t.Fatal("run file left behind")
	}
}

func TestSyncControlOverTheSocket(t *testing.T) {
	s := &syncer{}
	l := start(t, Options{Sync: s, Daemon: true})
	if rf := ReadRunFile(l.Root); !rf.Daemon {
		t.Fatal("daemon flag")
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", l.Socket)
	}}}
	resp, err := c.Post("http://atlas/sync", "application/json", strings.NewReader(`{"retry":true}`))
	if err != nil {
		t.Fatal(err)
	}
	var ans SyncAnswer
	_ = json.NewDecoder(resp.Body).Decode(&ans)
	_ = resp.Body.Close()
	if ans.Requeued != 3 || s.retried != 1 || s.forced != 1 {
		t.Fatalf("%+v %+v", ans, s)
	}
	resp, err = c.Get("http://atlas/hook")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("socket serves only /sync: %d", resp.StatusCode)
	}
}

func TestStaleRunFileIsIgnored(t *testing.T) {
	root := t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, ".atlas"), 0o755)
	// A pid that cannot be running.
	_ = os.WriteFile(RunFilePath(root), []byte(`{"pid":999999,"port":1,"socket":"/tmp/x.sock"}`), 0o644)
	if ReadRunFile(root) != nil {
		t.Fatal("a dead listener's run file was trusted")
	}
}
