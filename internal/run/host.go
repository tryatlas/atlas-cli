// Package run hosts what atlas run and atlas serve need around an agent:
// the Project's listener (memory over loopback HTTP), the capture store
// under the writer lock, the sync drain, and the ACP session that feeds the
// recorder (ADR-0009).
package run

import (
	"context"
	"errors"
	"time"

	gitlink "github.com/tryatlas/atlas-cli/internal/capture/git"
	"github.com/tryatlas/atlas-cli/internal/capture/record"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	capsync "github.com/tryatlas/atlas-cli/internal/capture/sync"
	"github.com/tryatlas/atlas-cli/internal/ingest"
	"github.com/tryatlas/atlas-cli/internal/listener"
)

// ErrLocked is the desktop (or another atlas) recording this Project.
var ErrLocked = errors.New("another Atlas window is recording this Project; close the folder in the Atlas app (or stop the other atlas run or atlas serve) and try again")

// HostOptions configure a host.
type HostOptions struct {
	Version string
	Ingest  string        // ingest base URL
	Tokens  ingest.Tokens // the signed-in user's JWTs
	Daemon  bool
	// CaptureOptional runs without capture (memory only) when the writer
	// lock is held elsewhere, instead of failing.
	CaptureOptional bool
}

// Host is a Project's running listener; Store, Recorder and Sync are nil
// when capture could not take the writer lock.
type Host struct {
	Root     string
	Listener *listener.Listener
	Store    *store.Store
	Recorder *record.Recorder
	Sync     *capsync.Runner

	lock   *store.WriterLock
	cancel context.CancelFunc
	done   chan struct{}
}

// StartHost takes the writer lock, opens the capture store, and starts the
// listener and the drain.
func StartHost(root string, opt HostOptions) (*Host, error) {
	h := &Host{Root: root, done: make(chan struct{})}
	lock, err := store.Lock(root)
	switch {
	case errors.Is(err, store.ErrLocked) && opt.CaptureOptional:
	case errors.Is(err, store.ErrLocked):
		return nil, ErrLocked
	case err != nil:
		return nil, err
	default:
		h.lock = lock
		st, err := store.Open(root, false)
		if err != nil && !errors.Is(err, store.ErrNoStore) {
			_ = lock.Release()
			return nil, err
		}
		if st != nil {
			h.Store = st
			h.Recorder = record.New(st, root)
			h.Sync = capsync.NewRunner(capsync.NewClient(opt.Ingest, opt.Tokens), st)
		}
	}
	lo := listener.Options{Daemon: opt.Daemon}
	if h.Sync != nil {
		lo.Sync = h.Sync
	}
	l, err := listener.Start(root, opt.Version, lo)
	if err != nil {
		h.release()
		return nil, err
	}
	h.Listener = l
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	go func() {
		if h.Sync != nil {
			h.Sync.Run(ctx)
		}
		close(h.done)
	}()
	return h, nil
}

// Capturing reports that this host holds the store.
func (h *Host) Capturing() bool { return h.Recorder != nil }

func (h *Host) release() {
	if h.Store != nil {
		_ = h.Store.Close()
	}
	if h.lock != nil {
		_ = h.lock.Release()
	}
}

// Close links new commits, gives the drain one last pass, and stops.
func (h *Host) Close() (gitlink.Result, error) {
	h.cancel()
	<-h.done
	err := h.Listener.Stop()
	var res gitlink.Result
	if h.Store != nil {
		var lerr error
		res, lerr = gitlink.Link(context.Background(), h.Store, h.Root, time.Now)
		if err == nil {
			err = lerr
		}
		_, _ = capsync.Drain(context.Background(), h.Sync.Client, h.Store, capsync.Budget, time.Now)
	}
	h.release()
	return res, err
}
