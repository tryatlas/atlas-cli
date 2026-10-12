package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/cli/internal/clistate"
	"github.com/tryatlas/cli/internal/hooks"
)

func TestUninstallCleansProjectsStateAndBinary(t *testing.T) {
	root := gitRepo(t)
	if code, _, e := runCmd(t, "init", "--local", "--agents", "claude-code,codex"); code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	_ = clistate.Path(root, "prefs.json") // the state directory exists, naming the Project
	exe := filepath.Join(t.TempDir(), "atlas-cli")
	_ = os.WriteFile(exe, []byte("bin"), 0o755)
	old := uninstallTarget
	uninstallTarget = func() (string, error) { return exe, nil }
	t.Cleanup(func() { uninstallTarget = old })

	if code, _, e := runCmd(t, "uninstall"); code != 1 || !strings.Contains(e, "--yes") {
		t.Fatalf("without a terminal it needs --yes: exit %d: %s", code, e)
	}
	t.Chdir(t.TempDir()) // found through the state directory, not the cwd
	code, out, e := runCmd(t, "uninstall", "--yes")
	if code != 0 {
		t.Fatalf("exit %d: %s %s", code, out, e)
	}
	for _, a := range hooks.All() {
		if a.State(root) != hooks.Absent {
			t.Fatalf("%s still hooked:\n%s", a.ID(), out)
		}
	}
	if hooks.GitHooksState(t.Context(), root) != hooks.Absent {
		t.Fatal("git hook left behind")
	}
	if _, err := os.Stat(clistate.Base()); !os.IsNotExist(err) {
		t.Fatalf("state directory left behind: %v", err)
	}
	if _, err := os.Stat(exe); !os.IsNotExist(err) {
		t.Fatal("binary left behind")
	}
	if _, err := os.Stat(filepath.Join(root, ".atlas")); err != nil {
		t.Fatal(".atlas/ must stay: the desktop app reads it")
	}
}

func TestUninstallKeepBinary(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "atlas-cli")
	_ = os.WriteFile(exe, []byte("bin"), 0o755)
	old := uninstallTarget
	uninstallTarget = func() (string, error) { return exe, nil }
	t.Cleanup(func() { uninstallTarget = old })
	if code, out, e := runCmd(t, "uninstall", "--yes", "--keep-binary"); code != 0 || strings.Contains(out, exe) {
		t.Fatalf("exit %d: %s %s", code, out, e)
	}
	if _, err := os.Stat(exe); err != nil {
		t.Fatal("--keep-binary deleted the binary")
	}
}
