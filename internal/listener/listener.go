// Package listener runs a Project's listener for atlas run and atlas serve:
// the shared-memory MCP server on a loopback HTTP port, reached by agents
// through the per-session bearer handed out on session/new (ADR-0009), and an
// owner-only Unix socket that carries the sync control endpoint for
// atlas capture sync and retry. Both are published in .atlas/cli-run.json.
package listener

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tryatlas/atlas-cli/internal/memory/server"
	"github.com/tryatlas/atlas-cli/internal/timefmt"
)

// RunFile is <Project>/.atlas/cli-run.json while a listener runs.
type RunFile struct {
	PID       int    `json:"pid"`
	Port      int    `json:"port"`
	StartedAt string `json:"startedAt"`
	Socket    string `json:"socket"`
	Daemon    bool   `json:"daemon,omitempty"` // started by atlas serve --daemon
}

// RunFilePath is where a Project's run file lives.
func RunFilePath(root string) string { return filepath.Join(root, ".atlas", "cli-run.json") }

// ReadRunFile returns the running listener's file, or nil when none runs.
func ReadRunFile(root string) *RunFile {
	b, err := os.ReadFile(RunFilePath(root))
	if err != nil {
		return nil
	}
	var rf RunFile
	if json.Unmarshal(b, &rf) != nil || rf.Socket == "" {
		return nil
	}
	return &rf
}

// Syncer is the background drain, driven from POST /sync by atlas capture
// sync and atlas capture retry while the listener holds the store.
type Syncer interface {
	Force()
	RetryFailed(ctx context.Context) (int64, error)
}

// SyncRequest is the body of POST /sync.
type SyncRequest struct {
	Retry bool `json:"retry"`
}

// SyncAnswer is its answer.
type SyncAnswer struct {
	Requeued int64 `json:"requeued"`
}

// Options configure the listener.
type Options struct {
	Sync   Syncer // nil: this listener does not drain
	Daemon bool   // recorded in the run file for atlas serve --stop
}

// Listener is a running listener.
type Listener struct {
	Root   string
	Socket string
	Port   int // the loopback port serving /mcp
	Memory *server.Server
	Tokens *server.Tokens

	control *http.Server
	tcp     *http.Server
	done    chan struct{}
}

// Start binds the control socket and the loopback port and starts serving.
func Start(root, version string, opt Options) (*Listener, error) {
	sock := server.SocketPath(root)
	ul, err := server.ListenUnix(sock)
	if err != nil {
		return nil, err
	}
	tl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = ul.Close()
		return nil, err
	}
	mem := server.New(version)
	tokens := server.NewTokens()
	ln := &Listener{Root: root, Socket: sock, Port: tl.Addr().(*net.TCPAddr).Port, Memory: mem, Tokens: tokens,
		done: make(chan struct{})}

	ctl := http.NewServeMux()
	ctl.HandleFunc("/sync", syncHandler(opt.Sync))
	ln.control = &http.Server{Handler: ctl, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		_ = ln.control.Serve(ul)
		close(ln.done)
	}()

	mux := http.NewServeMux()
	mux.Handle("/mcp", server.NewHTTPHandler(mem, tokens.Auth))
	ln.tcp = &http.Server{Handler: mux, ReadHeaderTimeout: 30 * time.Second}
	go func() { _ = ln.tcp.Serve(tl) }()

	rf := RunFile{PID: os.Getpid(), Port: ln.Port, StartedAt: timefmt.RFC3339(time.Now()), Socket: sock, Daemon: opt.Daemon}
	b, _ := json.Marshal(rf)
	if err := os.MkdirAll(filepath.Join(root, ".atlas"), 0o755); err != nil {
		_ = ln.Stop()
		return nil, err
	}
	if err := os.WriteFile(RunFilePath(root), b, 0o644); err != nil { //nolint:gosec // G306: holds no secret
		_ = ln.Stop()
		return nil, err
	}
	return ln, nil
}

// MCPURL is the memory server's URL for mcpServers entries.
func (l *Listener) MCPURL() string { return "http://127.0.0.1:" + strconv.Itoa(l.Port) + "/mcp" }

// Stop shuts the listener down and removes the run file and the socket.
func (l *Listener) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = l.tcp.Shutdown(ctx)
	err := l.control.Shutdown(ctx)
	<-l.done
	l.Memory.Close()
	if rf := ReadRunFile(l.Root); rf != nil && rf.PID == os.Getpid() {
		_ = os.Remove(RunFilePath(l.Root))
	}
	_ = os.Remove(l.Socket)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func syncHandler(s Syncer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if s == nil {
			http.Error(w, "this listener does not sync", http.StatusConflict)
			return
		}
		var req SyncRequest
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<10)).Decode(&req)
		var ans SyncAnswer
		if req.Retry {
			n, err := s.RetryFailed(r.Context())
			if err != nil {
				http.Error(w, "retry failed", http.StatusInternalServerError)
				return
			}
			ans.Requeued = n
		}
		s.Force()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ans)
	}
}
