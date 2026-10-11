package hooks

import (
	"os"
	"testing"
)

// TestMain keeps the CLI state directory (clistate) out of the real home.
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "atlas-state-")
	_ = os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
