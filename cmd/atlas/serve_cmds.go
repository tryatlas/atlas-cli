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
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/auth"
	gitlink "github.com/tryatlas/atlas-cli/internal/capture/git"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	capsync "github.com/tryatlas/atlas-cli/internal/capture/sync"
	"github.com/tryatlas/atlas-cli/internal/clistate"
	"github.com/tryatlas/atlas-cli/internal/hooks"
	"github.com/tryatlas/atlas-cli/internal/hosts"
	"github.com/tryatlas/atlas-cli/internal/ingest"
	"github.com/tryatlas/atlas-cli/internal/listener"
	"github.com/tryatlas/atlas-cli/internal/project"
	atlasrun "github.com/tryatlas/atlas-cli/internal/run"
)

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
func serveLog(root string) string { return clistate.Path(root, "serve.log") }

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
	h, err := atlasrun.StartHost(p.Root, hostOptions(os.Getenv(daemonEnv) != ""))
	if err != nil {
		return err
	}
	if h.Capturing() {
		h.Listener.SetHookSink(func(ctx context.Context, body []byte) error {
			var d hooks.Delivery
			if err := json.Unmarshal(body, &d); err != nil {
				return err
			}
			d.Root = p.Root
			if err := hooks.Apply(ctx, h.Recorder, h.Listener.Memory, []hooks.Delivery{d}); err != nil {
				return err
			}
			if h.Sync != nil && !d.End {
				h.Sync.Force() // the prompt, call or reply goes to the cloud now, not at the next tick
			}
			return nil
		})
		h.Listener.SetLinker(func(ctx context.Context) error {
			_, err := gitlink.Link(ctx, h.Store, p.Root, time.Now)
			return err
		})
		if err := hooks.DrainSpool(ctx, p.Root, h.Recorder, h.Listener.Memory); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "atlas: spooled hook turns:", err)
		}
	}
	every, _ := cmd.Flags().GetDuration("import-every")
	var im *atlasrun.Importer
	if every > 0 && h.Capturing() {
		im = atlasrun.NewImporter(agentsStore(), h, p.Root)
		h.Listener.SetImporter(im)
		ictx, stop := context.WithCancel(ctx)
		defer stop()
		go im.Loop(ictx, every, func(res []atlasrun.ImportResult, err error) {
			logImport(cmd.ErrOrStderr(), res, err, false)
		})
	}
	if os.Getenv(daemonEnv) == "" {
		fmt.Fprintf(out, "Serving %s: memory on %s, sync drain running, agent hooks delivered here. Ctrl-C to stop.\n", p.Root, h.Listener.MCPURL())
		if im != nil {
			fmt.Fprintf(out, "Importing sessions from the agents' own UIs every %s.\n", every)
		}
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	select {
	case <-sig:
	case <-ctx.Done():
	}
	if im != nil {
		im.Close()
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
	every, _ := cmd.Flags().GetDuration("import-every")
	args := []string{"serve"}
	if every > 0 {
		args = append(args, "--import-every", every.String())
	}
	child := exec.Command(self, args...) //nolint:gosec // G204: this binary
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

// hostOptions are the listener host's settings for this CLI.
func hostOptions(daemon bool) atlasrun.HostOptions {
	return atlasrun.HostOptions{Version: version, Ingest: hosts.Ingest(), Tokens: tokenSource(), Daemon: daemon}
}

// syncClient is the drain's HTTP client with the signed-in user's tokens.
func syncClient() *capsync.Client { return capsync.NewClient(hosts.Ingest(), tokenSource()) }

// tokenSource is the signed-in user's JWT source, or one that always says
// signed out.
func tokenSource() ingest.Tokens {
	if a, err := newAuth(); err == nil && a != nil {
		return a.Tokens()
	}
	return signedOut{}
}

// signedOut is the token source when no session file can be opened.
type signedOut struct{}

func (signedOut) Token(context.Context) (string, error) { return "", auth.ErrSignedOut }
func (signedOut) Invalidate()                           {}

// runCaptureImport imports native-UI sessions now: in this process when the
// store is free, else by asking the running listener.
func runCaptureImport(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	only, _ := cmd.Flags().GetStringArray("agent")
	dry, _ := cmd.Flags().GetBool("dry-run")
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Discover(ctx, cwd)
	if err != nil {
		return err
	}
	h, err := atlasrun.StartHost(p.Root, hostOptions(false))
	if errors.Is(err, atlasrun.ErrLocked) {
		rf := listener.ReadRunFile(p.Root)
		if rf == nil {
			return err
		}
		body, _ := json.Marshal(listener.SyncRequest{Import: true})
		if err := postControl(ctx, rf.Socket, body); err != nil {
			return fmt.Errorf("the running listener did not answer: %w", err)
		}
		fmt.Fprintln(out, "Asked the running listener to import now.")
		return nil
	}
	if err != nil {
		return err
	}
	im := atlasrun.NewImporter(agentsStore(), h, p.Root)
	res, ierr := im.Run(ctx, only, dry)
	im.Close()
	logImport(out, res, ierr, true)
	if _, err := h.Close(); err != nil && ierr == nil {
		ierr = err
	}
	return ierr
}

// logImport reports a pass: every agent when verbose, else only news.
func logImport(w interface{ Write([]byte) (int, error) }, res []atlasrun.ImportResult, err error, verbose bool) {
	for _, r := range res {
		switch {
		case r.Err != nil:
			fmt.Fprintf(w, "%s: %v\n", r.Agent, firstErrLine(r.Err))
		case r.Turns > 0 || verbose:
			line := fmt.Sprintf("%s: %d session(s) here, %d replayed, %d turn(s) captured", r.Agent, r.Listed, r.Replayed, r.Turns)
			if r.Note != "" {
				line += " (" + r.Note + ")"
			}
			fmt.Fprintln(w, line)
		}
	}
	if err != nil {
		fmt.Fprintln(w, "import:", err)
	}
}

func postControl(ctx context.Context, sock string, body []byte) error {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://atlas/sync", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("listener said %d", resp.StatusCode)
	}
	return nil
}
