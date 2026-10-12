package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/tryatlas/atlas-cli/internal/clistate"
	"github.com/tryatlas/atlas-cli/internal/embed"
	"github.com/tryatlas/atlas-cli/internal/memory/corpus"
	"github.com/tryatlas/atlas-cli/internal/memory/record"
	"github.com/tryatlas/atlas-cli/internal/project"
	"github.com/tryatlas/atlas-cli/internal/tui"
)

// atlas memory index: what the app's memory indexer does for a Project:
// the MiniLM model (downloaded once, shared with the app), a vector for
// every durable memory, and the corpus of documents memory_search returns.
func newMemoryIndexCmd() *cobra.Command {
	c := withRun(leaf("index", "Build the memory search index: memory vectors and the project's documents (downloads the model once)", cobra.NoArgs), runMemoryIndex)
	c.Flags().Bool("no-download", false, "do not download the model; index keywords only when it is missing")
	c.Flags().Bool("background", false, "run quietly, never download, skip when another pass runs (used by hooks)")
	_ = c.Flags().MarkHidden("background")
	return c
}

func runMemoryIndex(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Discover(ctx, cwd)
	if errors.Is(err, project.ErrNotInitialised) {
		return errNoProject
	}
	if err != nil {
		return err
	}
	bg, _ := cmd.Flags().GetBool("background")
	noDL, _ := cmd.Flags().GetBool("no-download")
	if bg {
		_, err := indexMemory(ctx, p.Root, p.ScopeRoot, false, func(string) {}, func(float64) {})
		if err != nil && !errors.Is(err, errIndexBusy) {
			// The hooks log is where background work reports.
			fmt.Fprintln(cmd.ErrOrStderr(), "atlas: memory index:", err)
		}
		return nil
	}
	return tui.WorkProgress(cmd.OutOrStdout(), "Indexing memory", func(step tui.StepFunc, frac tui.FracFunc) (string, error) {
		return indexMemory(ctx, p.Root, p.ScopeRoot, !noDL, step, frac)
	})
}

var errIndexBusy = errors.New("another index pass is running")

// indexMemory is one pass: model (download when allowed), memory vectors,
// corpus. One pass per Project at a time.
func indexMemory(ctx context.Context, root, scope string, download bool, step tui.StepFunc, frac tui.FracFunc) (string, error) {
	if !project.MemorySharing(scope) {
		return "Shared memory is off here (atlas memory enable); nothing to index.", nil
	}
	lock, err := os.OpenFile(clistate.Path(root, "index.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		return "", errIndexBusy
	}
	defer func() { _ = unix.Flock(int(lock.Fd()), unix.LOCK_UN) }()

	if !embed.Available() && download {
		step("Downloading the memory search model (" + embed.Size + ", shared with the Atlas app)")
		if err := embed.Download(ctx, func(done, total int64) {
			if total > 0 {
				frac(0.8 * float64(done) / float64(total))
			}
		}); err != nil {
			return "", fmt.Errorf("download the model: %w", err)
		}
	}
	vectors := record.Vectors()
	embedded := 0
	if st, err := record.Open(scope, true); err == nil {
		if vectors != nil {
			step("Embedding memories")
			embedded, err = st.SyncVectors(ctx)
			if err != nil && !errors.Is(err, record.ErrNoVectors) {
				_ = st.Close()
				return "", err
			}
		}
		_ = st.Close()
	} else {
		return "", err
	}
	frac(0.85)
	step("Reading the project's documents")
	docs, err := corpus.Collect(ctx, root, scope)
	if err != nil {
		return "", err
	}
	ix, err := corpus.Open(root, true)
	if err != nil {
		return "", err
	}
	defer ix.Close()
	step(fmt.Sprintf("Indexing %d documents", len(docs)))
	stats, err := ix.Update(ctx, docs, corpus.Owned, vectors)
	if err != nil {
		return "", err
	}
	frac(1)
	msg := fmt.Sprintf("Memory index ready: %d documents (%d new, %d changed, %d removed); %d memories embedded.",
		len(docs), stats.Added, stats.Updated, stats.Deleted, embedded)
	if vectors == nil {
		msg += " No model on this machine: keyword search only (atlas memory index downloads it)."
	}
	return msg, nil
}

// spawnIndex runs a background index pass detached, as hooks do after a
// session (the app indexes when a turn finishes and after extraction).
func spawnIndex(root string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	c := exec.Command(self, "memory", "index", "--background") //nolint:gosec // G204: this binary
	c.Dir = root
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if c.Start() == nil {
		_ = c.Process.Release()
	}
}
