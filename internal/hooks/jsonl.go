package hooks

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"time"
)

// Helpers for JSONL transcripts (Claude Code, Codex, Cursor, Copilot, Droid,
// Pi): positions are line numbers; a line may be arbitrarily long.

// CountLines is the number of lines in the file, a final unterminated line
// included.
func CountLines(path string) (int, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the transcript the agent's payload named
	if err != nil {
		return 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	n := 0
	for {
		line, err := r.ReadSlice('\n')
		for errors.Is(err, bufio.ErrBufferFull) {
			line, err = r.ReadSlice('\n')
		}
		if len(line) > 0 {
			n++
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return n, nil
			}
			return n, err
		}
	}
}

// LinesFrom returns the lines from the 0-based line index on, and the new
// line count. Blank lines are kept so positions stay aligned.
func LinesFrom(path string, from int) ([][]byte, int, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the transcript the agent's payload named
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	var out [][]byte
	n := 0
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			if n >= from {
				out = append(out, bytes.TrimRight(line, "\r\n"))
			}
			n++
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return out, n, nil
			}
			return out, n, err
		}
	}
}

// SettleFile waits until the file's size has been stable for quiet, or ctx
// or the limit ends, for agents that may still be flushing at turn end.
func SettleFile(ctx context.Context, path string, quiet, limit time.Duration) {
	deadline := time.Now().Add(limit)
	last := int64(-1)
	since := time.Now()
	for time.Now().Before(deadline) && ctx.Err() == nil {
		fi, err := os.Stat(path)
		if err != nil {
			return
		}
		if fi.Size() != last {
			last, since = fi.Size(), time.Now()
		} else if time.Since(since) >= quiet {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Tail returns the last n bytes of the file.
func Tail(path string, n int64) []byte {
	f, err := os.Open(path) //nolint:gosec // G304: the transcript the agent's payload named
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	off := fi.Size() - n
	if off < 0 {
		off = 0
	}
	buf := make([]byte, fi.Size()-off)
	_, _ = f.ReadAt(buf, off)
	return buf
}
