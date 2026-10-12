package record

import (
	"os"
	"testing"
)

// TestMain keeps the CLI state directory (clistate) out of the real home.
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "atlas-state-")
	_ = os.Setenv("XDG_STATE_HOME", dir)
	_ = os.Setenv("ATLAS_MODELS_DIR", dir) // no model: keyword-only and deterministic
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
