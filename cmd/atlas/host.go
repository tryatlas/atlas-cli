package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/auth"
	gitlink "github.com/tryatlas/atlas-cli/internal/capture/git"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	capsync "github.com/tryatlas/atlas-cli/internal/capture/sync"
	"github.com/tryatlas/atlas-cli/internal/hosts"
	"github.com/tryatlas/atlas-cli/internal/listener"
	"github.com/tryatlas/atlas-cli/internal/project"
)

// host is a Project's running listener with everything that needs the
// writer lock: the capture store and the sync drain. atlas serve runs one;
// atlas run will run one in-process.
type host struct {
	Root     string
	Listener *listener.Listener
	Store    *store.Store
	Sync     *capsync.Runner

	lock   *store.WriterLock
	cancel context.CancelFunc
	done   chan struct{}
}

// syncClient is the drain's HTTP client with the signed-in user's tokens.
func syncClient() *capsync.Client {
	var tokens interface {
		Token(context.Context) (string, error)
		Invalidate()
	} = signedOut{}
	if a, err := auth.New(); err == nil {
		tokens = a.Tokens()
	}
	return capsync.NewClient(hosts.Ingest(), tokens)
}

// signedOut is the token source when no session file can be opened.
type signedOut struct{}

func (signedOut) Token(context.Context) (string, error) { return "", auth.ErrSignedOut }
func (signedOut) Invalidate()                           {}

// startHost takes the writer lock, opens the capture store, and starts the
// listener and the drain.
func startHost(root string) (*host, error) {
	lock, err := store.Lock(root)
	if errors.Is(err, store.ErrLocked) {
		return nil, errors.New(lockedHelp)
	}
	if err != nil {
		return nil, err
	}
	st, err := store.Open(root, false)
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	runner := capsync.NewRunner(syncClient(), st)
	l, err := listener.Start(root, version, listener.Options{Sync: runner,
		Daemon: os.Getenv(daemonEnv) != ""})
	if err != nil {
		_ = st.Close()
		_ = lock.Release()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &host{Root: root, Listener: l, Store: st, Sync: runner, lock: lock, cancel: cancel, done: make(chan struct{})}
	go func() {
		runner.Run(ctx)
		close(h.done)
	}()
	return h, nil
}

// Close links new commits, gives the drain one last pass, and stops.
func (h *host) Close() (gitlink.Result, error) {
	h.cancel()
	<-h.done
	err := h.Listener.Stop()
	res, lerr := gitlink.Link(context.Background(), h.Store, h.Root, time.Now)
	if err == nil {
		err = lerr
	}
	_, _ = capsync.Drain(context.Background(), h.Sync.Client, h.Store, capsync.Budget, time.Now)
	_ = h.Store.Close()
	_ = h.lock.Release()
	return res, err
}

// ── atlas capture sync / retry ───────────────────────────────────────────────

// runCaptureSync drains now: in this process when the store is free,
// otherwise by asking the running listener.
func runCaptureSync(retry bool) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		ctx := cmd.Context()
		out := cmd.OutOrStdout()
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		p, err := project.Discover(ctx, cwd)
		if err != nil {
			return err
		}
		if !store.Exists(p.Root) {
			return errors.New("capture has never been enabled here; run atlas init")
		}
		lock, err := store.Lock(p.Root)
		if errors.Is(err, store.ErrLocked) {
			rf := listener.ReadRunFile(p.Root)
			if rf == nil {
				return errors.New(lockedHelp)
			}
			ans, err := postSync(ctx, rf.Socket, retry)
			if err != nil {
				return fmt.Errorf("the running listener did not answer: %w", err)
			}
			if retry {
				fmt.Fprintf(out, "Requeued %d failed row(s).\n", ans.Requeued)
			}
			fmt.Fprintln(out, "Asked the running listener to sync now.")
			return nil
		}
		if err != nil {
			return err
		}
		defer func() { _ = lock.Release() }()
		st, err := store.Open(p.Root, false)
		if err != nil {
			return err
		}
		defer st.Close()
		if retry {
			n, err := st.RetryFailed(ctx)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "Requeued %d failed row(s).\n", n)
		}
		b, err := st.Binding(ctx)
		if err != nil {
			return err
		}
		if _, ok := capsync.TargetOf(b); !ok {
			fmt.Fprintln(out, "This Project is local: nothing syncs. Bind it to a cloud Project with atlas init.")
			return nil
		}
		if b.DrainState == store.DrainNotAuthorized {
			// A human action re-checks access (the desktop re-registers).
			if err := st.SetDrainState(ctx, store.DrainOK, time.Now()); err != nil {
				return err
			}
		}
		total := capsync.Report{}
		c := syncClient()
		for {
			rep, err := capsync.Drain(ctx, c, st, capsync.Budget, time.Now)
			if err != nil {
				return err
			}
			total.Sent += rep.Sent
			total.Failed += rep.Failed
			total.Retired += rep.Retired
			total.Outcome = rep.Outcome
			if rep.Outcome != capsync.Done || rep.Sent+rep.Failed == 0 {
				break
			}
		}
		counts, _ := st.OutboxCounts(ctx)
		fmt.Fprintf(out, "Sent %d row(s); %d failed. Outbox: %d pending, %d failed.\n",
			total.Sent, total.Failed+int(total.Retired), counts[store.SyncPending], counts[store.SyncFailed])
		switch total.Outcome {
		case capsync.Offline:
			return errors.New("the ingest server is unreachable; the listener retries with backoff")
		case capsync.RateLimited:
			return errors.New("the ingest server asked to slow down; try again later")
		case capsync.NotAuthorized:
			return errors.New("the ingest server refused this Project (403); check your access with atlas status")
		case capsync.SignedOut:
			return errors.New("not signed in; run atlas login")
		}
		return nil
	}
}

func postSync(ctx context.Context, sock string, retry bool) (listener.SyncAnswer, error) {
	var ans listener.SyncAnswer
	body, _ := json.Marshal(listener.SyncRequest{Retry: retry})
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://atlas/sync", bytes.NewReader(body))
	if err != nil {
		return ans, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return ans, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ans, fmt.Errorf("listener said %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&ans)
	return ans, err
}

// ── atlas serve ──────────────────────────────────────────────────────────────

// daemonEnv marks the detached child of atlas serve --daemon.
const daemonEnv = "ATLAS_SERVE_DAEMON"

// serveLog is where a detached listener's stderr goes.
func serveLog(root string) string { return filepath.Join(root, store.DirName, "cli-serve.log") }

// runServe runs the listener until interrupted; --daemon detaches it and
// --stop ends a detached one.
func runServe(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Discover(ctx, cwd)
	if err != nil {
		return err
	}
	if s, _ := cmd.Flags().GetBool("stop"); s {
		return stopDaemon(out, p.Root)
	}
	if d, _ := cmd.Flags().GetBool("daemon"); d && os.Getenv(daemonEnv) == "" {
		return startDaemon(cmd, p.Root)
	}
	h, err := startHost(p.Root)
	if err != nil {
		return err
	}
	if os.Getenv(daemonEnv) == "" {
		fmt.Fprintf(out, "Serving %s: memory on %s, sync drain running. Ctrl-C to stop.\n", p.Root, h.Listener.MCPURL())
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	select {
	case <-sig:
	case <-ctx.Done():
	}
	res, err := h.Close()
	if res.Checkpoints > 0 && os.Getenv(daemonEnv) == "" {
		fmt.Fprintf(out, "Linked %d session commit(s).\n", res.Checkpoints)
	}
	return err
}

// startDaemon re-runs `atlas serve` detached in its own session and waits for
// its run file.
func startDaemon(cmd *cobra.Command, root string) error {
	if rf := listener.ReadRunFile(root); rf != nil && alive(rf.PID) {
		return fmt.Errorf("a listener is already running for this Project (pid %d)", rf.PID)
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	logf, err := os.OpenFile(serveLog(root), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer logf.Close()
	child := exec.Command(self, "serve") //nolint:gosec // G204: this binary
	child.Dir = root
	child.Env = append(os.Environ(), daemonEnv+"=1")
	child.Stdout, child.Stderr = logf, logf
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := child.Start(); err != nil {
		return err
	}
	pid := child.Process.Pid
	_ = child.Process.Release()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rf := listener.ReadRunFile(root); rf != nil && rf.PID == pid {
			fmt.Fprintf(cmd.OutOrStdout(), "Listener started (pid %d) on 127.0.0.1:%d. Stop it with atlas serve --stop.\n", pid, rf.Port)
			return nil
		}
		if !alive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the listener did not start; see %s", serveLog(root))
}

// stopDaemon signals the listener named in the run file and waits for it.
func stopDaemon(out interface{ Write([]byte) (int, error) }, root string) error {
	rf := listener.ReadRunFile(root)
	if rf == nil || !alive(rf.PID) {
		fmt.Fprintln(out, "No listener is running for this Project.")
		return nil
	}
	if !rf.Daemon {
		return fmt.Errorf("the listener (pid %d) belongs to atlas run or a foreground atlas serve; exit that instead", rf.PID)
	}
	if err := syscall.Kill(rf.PID, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(rf.PID) {
			fmt.Fprintf(out, "Stopped the listener (pid %d).\n", rf.PID)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("the listener (pid %d) did not stop", rf.PID)
}

func alive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}
