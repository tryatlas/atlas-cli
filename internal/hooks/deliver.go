package hooks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/listener"
	memrecord "github.com/tryatlas/atlas-cli/internal/memory/record"
	"github.com/tryatlas/atlas-cli/internal/memory/server"
)

// Delivery routes (ADR-0013 §5): a running atlas serve holds the writer
// lock, so a hook posts to its Unix socket; otherwise the hook takes the lock
// and writes itself; when the desktop app holds the lock the turn is spooled
// and the next writer drains it.

// ErrSpooled says the delivery was written to the spool, not the store.
var ErrSpooled = errors.New("spooled")

// SpoolDir holds deliveries waiting for a writer.
func SpoolDir(root string) string { return filepath.Join(Dir(root), "spool") }

// Deliver is the default Dispatcher.Deliver. afterDirect, when set, runs
// after a direct write with the store still open: the caller syncs the new
// rows to the cloud there, so a turn does not wait for the next atlas serve.
func Deliver(version string, afterDirect func(ctx context.Context, st *store.Store, d Delivery)) func(ctx context.Context, d Delivery) error {
	return func(ctx context.Context, d Delivery) error {
		if rf := listener.ReadRunFile(d.Root); rf != nil {
			if err := postHook(ctx, rf.Socket, d); err == nil {
				return nil
			}
			// The listener is there but did not answer: fall through to a
			// direct write, which fails on its lock and spools.
		}
		err := applyDirect(ctx, version, d.Root, []Delivery{d}, afterDirect)
		if errors.Is(err, store.ErrLocked) {
			if serr := spool(d); serr != nil {
				return serr
			}
			return ErrSpooled
		}
		return err
	}
}

// Apply records deliveries with an open recorder and memory server; the
// listener and the direct path share it.
func Apply(ctx context.Context, rec *record.Recorder, mem *server.Server, ds []Delivery) error {
	for _, d := range ds {
		g := server.Grant{SessionID: d.Session.NativeID, Agent: d.Session.Agent, Cwd: d.Root}
		switch {
		case d.Start:
			if mem != nil {
				mem.SessionStarted(ctx, g)
			}
		case d.End:
			if mem != nil {
				mem.SessionEnded(ctx, g)
			}
		case d.Open != nil:
			if _, err := rec.OpenTurn(ctx, d.Session, d.Open.Prompt, d.Open.At); err != nil {
				return err
			}
		case d.Tool != nil:
			if err := rec.RecordTool(ctx, d.Session, *d.Tool, d.Changed, d.At); err != nil {
				return err
			}
			if mem != nil {
				foldChanges(ctx, mem, g, d.Root, &record.CapturedTurn{Items: []record.CapturedItem{{Tool: d.Tool}}, Changed: d.Changed})
			}
		case d.Turn != nil:
			if _, err := rec.RecordTurn(ctx, d.Session, *d.Turn); err != nil {
				return err
			}
			if mem != nil {
				foldChanges(ctx, mem, g, d.Root, d.Turn)
			}
		}
	}
	return nil
}

// foldChanges tells shared memory which files the turn changed, as the ACP
// session does for live runs.
func foldChanges(ctx context.Context, mem *server.Server, g server.Grant, root string, t *record.CapturedTurn) {
	seen := map[string]bool{}
	note := func(rel, summary string) {
		if rel == "" || seen[rel] || filepath.IsAbs(rel) {
			return
		}
		seen[rel] = true
		_ = mem.Fold(ctx, g, memrecord.Event{Kind: memrecord.KindFileChanged, Key: rel, Text: summary})
	}
	for _, it := range t.Items {
		if it.Tool == nil {
			continue
		}
		name := record.CanonicalToolName(it.Tool.Name, it.Tool.Title, it.Tool.Kind, it.Tool.Arguments)
		if !record.WritesFiles(name) && len(it.Tool.Diffs) == 0 {
			continue
		}
		for _, p := range record.WritePaths(it.Tool.Arguments, it.Tool.Locations, it.Tool.Diffs...) {
			rel, out := record.ProjectPath(root, p)
			if !out {
				note(rel, name+" by "+g.Agent)
			}
		}
	}
	for _, p := range t.Changed {
		note(p, "changed during a "+g.Agent+" turn")
	}
}

// applyDirect takes the writer lock for the duration of the write, drains
// the spool first, then applies ds.
func applyDirect(ctx context.Context, version, root string, ds []Delivery, after func(ctx context.Context, st *store.Store, d Delivery)) error {
	lock, err := store.Lock(root)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()
	st, err := store.Open(root, false)
	if err != nil {
		return err
	}
	defer st.Close()
	rec := record.New(st, root)
	mem := server.New(version)
	defer mem.Close()
	if err := DrainSpool(ctx, root, rec, mem); err != nil {
		return err
	}
	if err := Apply(ctx, rec, mem, ds); err != nil {
		return err
	}
	ClearLastError(root)
	if after != nil {
		for _, d := range ds {
			after(ctx, st, d)
		}
	}
	return nil
}

// spool writes a delivery to the spool directory.
func spool(d Delivery) error {
	dir := SpoolDir(d.Root)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := fmt.Sprintf("%020d-%s.json", time.Now().UnixNano(), hex.EncodeToString(rnd[:]))
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, name+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

// DrainSpool applies every spooled delivery in order and removes it; the
// Pending counters of the sessions involved are reset.
func DrainSpool(ctx context.Context, root string, rec *record.Recorder, mem *server.Server) error {
	ents, err := os.ReadDir(SpoolDir(root))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		if filepath.Ext(e.Name()) == ".json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	touched := map[[2]string]bool{}
	for _, n := range names {
		p := filepath.Join(SpoolDir(root), n)
		b, err := os.ReadFile(p) //nolint:gosec // G304: the Project's own spool
		if err != nil {
			continue
		}
		var d Delivery
		if json.Unmarshal(b, &d) != nil {
			_ = os.Remove(p) // unreadable: never deliverable
			continue
		}
		d.Root = root
		if err := Apply(ctx, rec, mem, []Delivery{d}); err != nil {
			return err
		}
		_ = os.Remove(p)
		touched[[2]string{d.Session.Agent, d.Session.NativeID}] = true
	}
	for k := range touched {
		if st, err := LoadState(root, k[0], k[1]); err == nil && st != nil {
			st.Pending = 0
			_ = st.Save(root)
		}
	}
	return nil
}

// postHook sends a delivery to a running listener's control socket.
func postHook(ctx context.Context, sock string, d Delivery) error {
	body, err := json.Marshal(d)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{Proxy: nil,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", sock)
		}}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://atlas/hook", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("listener said %d", resp.StatusCode)
	}
	return nil
}
