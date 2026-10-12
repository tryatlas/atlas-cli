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
	"strings"
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
	"github.com/tryatlas/atlas-cli/internal/tui"
)

// ── atlas-cli capture sync / retry ───────────────────────────────────────────────

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
			return errors.New("capture has never been enabled here; run atlas-cli init")
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
				tui.Done(out, fmt.Sprintf("Requeued %d failed row(s).", ans.Requeued))
			}
			tui.Done(out, "Asked the running listener to sync now.")
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
		title := "Syncing to the cloud Project"
		if retry {
			title = "Retrying failed rows"
		}
		return tui.Work(out, title, func(step tui.StepFunc) (string, error) {
			var lines []string
			if retry {
				n, err := st.RetryFailed(ctx)
				if err != nil {
					return "", err
				}
				lines = append(lines, fmt.Sprintf("Requeued %d failed row(s).", n))
				step(lines[0])
			}
			b, err := st.Binding(ctx)
			if err != nil {
				return "", err
			}
			if _, ok := capsync.TargetOf(b); !ok {
				return strings.Join(append(lines, "This Project is local: nothing syncs. Bind it to a cloud Project with atlas-cli init."), "\n"), nil
			}
			if b.DrainState == store.DrainNotAuthorized {
				// A human action re-checks access (the desktop re-registers).
				if err := st.SetDrainState(ctx, store.DrainOK, time.Now()); err != nil {
					return "", err
				}
			}
			total := capsync.Report{}
			c := syncClient()
			for {
				rep, err := capsync.Drain(ctx, c, st, capsync.Budget, time.Now)
				if err != nil {
					return "", err
				}
				total.Sent += rep.Sent
				total.Failed += rep.Failed
				total.Retired += rep.Retired
				total.Outcome = rep.Outcome
				step(fmt.Sprintf("%d row(s) sent", total.Sent))
				if rep.Outcome != capsync.Done || rep.Sent+rep.Failed == 0 {
					break
				}
			}
			counts, _ := st.OutboxCounts(ctx)
			summary := fmt.Sprintf("Sent %d row(s); %d failed. Outbox: %d pending, %d failed.",
				total.Sent, total.Failed+int(total.Retired), counts[store.SyncPending], counts[store.SyncFailed])
			switch total.Outcome {
			case capsync.Offline:
				return "", fmt.Errorf("%s The ingest server is unreachable; the listener retries with backoff", summary)
			case capsync.RateLimited:
				return "", fmt.Errorf("%s The ingest server asked to slow down; try again later", summary)
			case capsync.NotAuthorized:
				return "", fmt.Errorf("%s The ingest server refused this Project (403); check your access with atlas-cli status", summary)
			case capsync.SignedOut:
				return "", fmt.Errorf("%s Not signed in; run atlas-cli login", summary)
			}
			return strings.Join(append(lines, summary), "\n"), nil
		})
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

// ── atlas-cli serve ──────────────────────────────────────────────────────────────

// daemonEnv marks the detached child of atlas-cli serve --daemon.
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
	var live *tui.Live
	if os.Getenv(daemonEnv) == "" && interactive() {
		live = tui.StartLive([]string{"serve"}, [][2]string{
			{"Project", p.Root},
			{"Memory", h.Listener.MCPURL()},
			{"Sync", tui.Dot(tui.Good, "drain running")},
			{"Hooks", tui.Dot(tui.Good, "delivered here")},
		})
	}
	if h.Capturing() {
		h.Listener.SetHookSink(func(ctx context.Context, body []byte) error {
			var d hooks.Delivery
			if err := json.Unmarshal(body, &d); err != nil {
				return err
			}
			d.Root = p.Root
			if h.Stale() {
				return errStaleStore // the hook writes to the new store itself
			}
			err := hooks.Apply(ctx, h.Recorder, h.Listener.Memory, []hooks.Delivery{d})
			if live != nil {
				live.Event(deliveryEvent(d, err))
			}
			if err != nil {
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
			fmt.Fprintln(cmd.ErrOrStderr(), "atlas-cli: spooled hook turns:", err)
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
	if os.Getenv(daemonEnv) == "" && live == nil {
		fmt.Fprintf(out, "Serving %s: memory on %s, sync drain running, agent hooks delivered here. Ctrl-C to stop.\n", p.Root, h.Listener.MCPURL())
		if im != nil {
			fmt.Fprintf(out, "Importing sessions from the agents' own UIs every %s.\n", every)
		}
	}
	// The memory index follows the record and the sessions, as the app's
	// indexer does: now, then every 15 minutes (never downloading).
	go func() {
		t := time.NewTicker(15 * time.Minute)
		defer t.Stop()
		for {
			if project.MemorySharing(p.ScopeRoot) {
				_, _ = indexMemory(ctx, p.Root, p.ScopeRoot, false, func(string) {}, func(float64) {})
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	// A store deleted or re-created under the listener: stop rather than
	// write into a file no one can see. Hooks then write directly, and the
	// next one restarts a listener when background sync is on.
	stale := make(chan struct{})
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if h.Stale() {
					fmt.Fprintln(cmd.ErrOrStderr(), "atlas-cli: this Project's .atlas was replaced; stopping the listener")
					close(stale)
					return
				}
			}
		}
	}()
	if live != nil {
		ended := make(chan struct{})
		go func() { _ = live.Run(); close(ended) }()
		select {
		case <-ended: // q, esc or ctrl+c on the screen
		case <-sig:
			live.Stop()
			<-ended
		case <-stale:
			live.Stop()
			<-ended
		case <-ctx.Done():
			live.Stop()
			<-ended
		}
	} else {
		select {
		case <-sig:
		case <-stale:
		case <-ctx.Done():
		}
	}
	if im != nil {
		im.Close()
	}
	res, err := h.Close()
	if res.Checkpoints > 0 && os.Getenv(daemonEnv) == "" {
		tui.Done(out, fmt.Sprintf("Linked %d session commit(s).", res.Checkpoints))
	}
	if live != nil && err == nil {
		tui.Done(out, "Listener stopped.")
	}
	return err
}

// errStaleStore refuses a hook delivery to a listener whose store was
// replaced; the hook falls back to writing the store itself.
var errStaleStore = errors.New("the listener's store was replaced")

// deliveryEvent is a hook delivery as one line of serve's live feed.
func deliveryEvent(d hooks.Delivery, err error) tui.LiveEvent {
	e := tui.LiveEvent{At: time.Now(), Who: d.Session.Agent, Bad: err != nil}
	switch {
	case err != nil:
		e.What = "failed: " + err.Error()
	case d.Start:
		e.What = "session started"
	case d.Open != nil:
		e.What = "prompt  " + tui.Default.Faint.Render(strings.Join(strings.Fields(d.Open.Prompt), " "))
	case d.Tool != nil:
		e.What = "tool    " + tui.Default.Faint.Render(d.Tool.Name+" "+strings.Join(d.Changed, ", "))
	case d.Turn != nil:
		e.What = "reply   " + tui.Default.Faint.Render("turn closed")
	case d.End:
		e.What = "session ended"
	default:
		e.What = "event"
	}
	if d.LinkFrom != "" && err == nil {
		e.What += tui.Default.Success.Render("  ✓ commit linked")
	}
	return e
}

// startDaemon re-runs `atlas-cli serve` detached in its own session and waits for
// its run file.
func startDaemon(cmd *cobra.Command, root string) error {
	every, _ := cmd.Flags().GetDuration("import-every")
	pid, port, err := launchListener(root, every)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Listener started (pid %d) on 127.0.0.1:%d. Stop it with atlas-cli sync off.\n", pid, port)
	return nil
}

// spawnListener starts the detached listener without waiting for it.
func spawnListener(root string, every time.Duration) (*exec.Cmd, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(serveLog(root), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close()
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
		return nil, err
	}
	return child, nil
}

// launchListener starts the detached listener and waits for its run file.
func launchListener(root string, every time.Duration) (pid, port int, err error) {
	if rf := listener.ReadRunFile(root); rf != nil && alive(rf.PID) {
		return 0, 0, fmt.Errorf("a listener is already running for this Project (pid %d)", rf.PID)
	}
	child, err := spawnListener(root, every)
	if err != nil {
		return 0, 0, err
	}
	pid = child.Process.Pid
	_ = child.Process.Release()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if rf := listener.ReadRunFile(root); rf != nil && rf.PID == pid {
			return pid, rf.Port, nil
		}
		if !alive(pid) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, 0, fmt.Errorf("the listener did not start; see %s", serveLog(root))
}

// stopDaemon signals the listener named in the run file and waits for it.
func stopDaemon(out interface{ Write([]byte) (int, error) }, root string) error {
	pid, err := haltListener(root)
	if err != nil {
		return err
	}
	if pid == 0 {
		fmt.Fprintln(out, "No listener is running for this Project.")
		return nil
	}
	fmt.Fprintf(out, "Stopped the listener (pid %d).\n", pid)
	return nil
}

// haltListener stops the detached listener; pid 0 means none ran.
func haltListener(root string) (int, error) {
	rf := listener.ReadRunFile(root)
	if rf == nil || !alive(rf.PID) {
		return 0, nil
	}
	if !rf.Daemon {
		return 0, fmt.Errorf("the listener (pid %d) belongs to atlas-cli run or a foreground atlas-cli serve; exit that instead", rf.PID)
	}
	if err := syscall.Kill(rf.PID, syscall.SIGTERM); err != nil {
		return 0, err
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(rf.PID) {
			return rf.PID, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return 0, fmt.Errorf("the listener (pid %d) did not stop", rf.PID)
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
