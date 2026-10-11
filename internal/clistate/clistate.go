// Package clistate is where the CLI keeps its own per-Project files: hook
// session state, the spool, the listener's run file and logs. They live
// outside <Project>/.atlas/ so that folder holds exactly what the desktop app
// writes: $XDG_STATE_HOME/atlas-cli/projects/<16 hex of sha256(root)>/
// (default ~/.local/state/…), with a "root" file naming the Project.
package clistate

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// Dir is the CLI's state directory for a Project root.
func Dir(root string) string {
	base := os.Getenv("XDG_STATE_HOME")
	if !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		base = filepath.Join(home, ".local", "state")
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(base, "atlas-cli", "projects", hex.EncodeToString(sum[:])[:16])
}

// Ensure creates the directory (0700: it holds typed prompts) and its root
// file, and moves what older CLIs left in <root>/.atlas/.
func Ensure(root string) (string, error) {
	d := Dir(root)
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	if _, err := os.Stat(filepath.Join(d, "root")); errors.Is(err, fs.ErrNotExist) {
		_ = os.WriteFile(filepath.Join(d, "root"), []byte(root+"\n"), 0o600)
	}
	migrate(root, d)
	return d, nil
}

// Path is a file in the state directory (created on demand).
func Path(root, name string) string {
	d, err := Ensure(root)
	if err != nil {
		return filepath.Join(Dir(root), name)
	}
	return filepath.Join(d, name)
}

// legacy maps what CLIs up to 0.3.1 wrote into .atlas/ to its new name.
var legacy = map[string]string{
	"hooks":           "hooks",
	"cli-hooks.log":   "hooks.log",
	"cli-serve.log":   "serve.log",
	"cli-import.json": "import.json",
}

func migrate(root, d string) {
	old := filepath.Join(root, ".atlas")
	for from, to := range legacy {
		src := filepath.Join(old, from)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		dst := filepath.Join(d, to)
		if _, err := os.Lstat(dst); err == nil {
			_ = os.RemoveAll(src) // the new copy wins
			continue
		}
		if os.Rename(src, dst) != nil {
			_ = os.RemoveAll(src)
		}
	}
	// A run file of a listener from an older CLI is stale by now.
	_ = os.Remove(filepath.Join(old, "cli-run.json"))
}
