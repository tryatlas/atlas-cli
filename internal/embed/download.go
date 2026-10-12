package embed

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// BaseURL is where the files come from, as the app downloads them
// (models.rs: https://huggingface.co/{repo}/resolve/main/{file}).
var BaseURL = "https://huggingface.co/" + Repo + "/resolve/main/"

// Download fetches the files that are missing into ModelDir, each written as
// <file>.part and renamed, as the app does; progress reports bytes done of
// the total known so far. Files already there are kept.
func Download(ctx context.Context, progress func(done, total int64)) error {
	dir := ModelDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	var done, total int64
	for _, f := range Files {
		dst := filepath.Join(dir, f)
		if fi, err := os.Stat(dst); err == nil && fi.Size() > 0 {
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, BaseURL+f, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "atlas-cli")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return fmt.Errorf("download %s: %s", f, resp.Status)
		}
		if resp.ContentLength > 0 {
			total += resp.ContentLength
		}
		part := dst + ".part"
		out, err := os.Create(part) //nolint:gosec // G304: our own model directory
		if err != nil {
			_ = resp.Body.Close()
			return err
		}
		buf := make([]byte, 256<<10)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, err := out.Write(buf[:n]); err != nil {
					_ = out.Close()
					_ = resp.Body.Close()
					return err
				}
				done += int64(n)
				if progress != nil {
					progress(done, total)
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				_ = out.Close()
				_ = resp.Body.Close()
				return rerr
			}
		}
		_ = resp.Body.Close()
		if err := out.Close(); err != nil {
			return err
		}
		if err := os.Rename(part, dst); err != nil {
			return err
		}
	}
	return nil
}

// Size is the model's download size, for prompts.
const Size = "87 MB"
