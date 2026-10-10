// Package listener runs the per-Project listener that atlas shell and
// atlas serve start: the shared-memory MCP server on a Unix socket for the
// stdio bridge (ADR-0003), published in .atlas/cli-run.json. The capture
// proxy joins it in a later phase.
package listener

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/memory/server"
	"github.com/tryatlas/atlas-cli/internal/procs"
	"github.com/tryatlas/atlas-cli/internal/proxy"
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

// Listener is a running listener.
type Listener struct {
	Root   string
	Socket string
	Port   int // the capture proxy's loopback port; 0 when capture is not served
	Memory *server.Server
	Hooks  *proxy.Hooks

	claude *proxy.Anthropic
	http   *http.Server
	tcp    *http.Server
	done   chan struct{}
}

// Options configure what the listener serves besides memory.
type Options struct {
	Recorder          *record.Recorder // nil: no capture proxy
	AnthropicUpstream *url.URL         // where Claude traffic goes after the proxy
	Sync              Syncer           // nil: this listener does not drain
	Daemon            bool             // recorded in the run file for atlas serve --stop
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

// Start binds the socket (memory, hooks) and, with a recorder, the loopback
// proxy port, and starts serving.
func Start(root, version string, opt Options) (*Listener, error) {
	sock := server.SocketPath(root)
	l, err := server.ListenUnix(sock)
	if err != nil {
		return nil, err
	}
	mem := server.New(version)
	ln := &Listener{Root: root, Socket: sock, Memory: mem, Hooks: proxy.NewHooks(), done: make(chan struct{})}
	mux := http.NewServeMux()
	mcp := server.NewHTTPHandler(mem, server.SocketAuth)
	mcp.Join = ln.join
	mux.Handle("/mcp", mcp)
	mux.HandleFunc("/hook", ln.hook)
	mux.HandleFunc("/sync", ln.syncHandler(opt.Sync))
	ln.http = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		_ = ln.http.Serve(l)
		close(ln.done)
	}()
	if opt.Recorder != nil {
		tl, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			_ = ln.Stop()
			return nil, err
		}
		ln.Port = tl.Addr().(*net.TCPAddr).Port
		ln.claude = proxy.NewAnthropic(opt.Recorder, ln.Hooks, root)
		pm := http.NewServeMux()
		pm.Handle("/proxy/anthropic/", proxy.Handler("/proxy/anthropic", opt.AnthropicUpstream, ln.claude))
		ln.tcp = &http.Server{Handler: pm, ReadHeaderTimeout: 30 * time.Second}
		go func() { _ = ln.tcp.Serve(tl) }()
	}
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

// Stop shuts the listener down and removes the run file and the socket.
func (l *Listener) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if l.tcp != nil {
		_ = l.tcp.Shutdown(ctx)
	}
	err := l.http.Shutdown(ctx)
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

// HookEvent is what `atlas hook <agent> <event>` posts to the listener.
type HookEvent struct {
	Agent     string `json:"agent"`
	Event     string `json:"event"` // session-start, user-prompt-submit, stop, session-end
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	Prompt    string `json:"prompt,omitempty"`
	PIDs      []int  `json:"pids,omitempty"` // the hook process's ancestors
}

func (l *Listener) hook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var ev HookEvent
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&ev); err != nil || ev.SessionID == "" {
		http.Error(w, "bad hook event", http.StatusBadRequest)
		return
	}
	switch ev.Event {
	case "session-start":
		l.Hooks.SessionStart(ev.SessionID, ev.Cwd, ev.PIDs)
	case "user-prompt-submit":
		l.Hooks.Seen(ev.SessionID, ev.Cwd, ev.PIDs)
		l.Hooks.Prompt(ev.SessionID, ev.Cwd, ev.Prompt)
	case "stop":
		if l.claude != nil {
			l.claude.FinishSession(ev.SessionID)
		}
	case "session-end":
		if l.claude != nil {
			l.claude.FinishSession(ev.SessionID)
		}
		l.Hooks.End(ev.SessionID)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (l *Listener) syncHandler(s Syncer) http.HandlerFunc {
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

// join gives a bridge's MCP session the agent's own session id, matched
// through the hooks by process ancestry (ADR-0003).
func (l *Listener) join(g server.Grant, r *http.Request) (server.Grant, bool) {
	bridge := procs.Parse(r.Header.Get(server.HeaderPIDs))
	id, ok := l.Hooks.Match(bridge, g.Cwd, time.Now(), l.excluded)
	if !ok || id == g.SessionID {
		return g, false
	}
	g.SessionID = id
	return g, true
}

// excluded reports processes that are ancestors of many agents: this
// listener, its ancestors, and the shell it started.
func (l *Listener) excluded(pid int) bool {
	self := os.Getpid()
	if pid == self || procs.Parent(pid) == self {
		return true
	}
	for _, a := range procs.Ancestors(self, 8) {
		if a == pid {
			return true
		}
	}
	return false
}
