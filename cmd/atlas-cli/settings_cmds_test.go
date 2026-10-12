package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/clistate"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

func TestInitHooksTheAgentsGiven(t *testing.T) {
	root := gitRepo(t)
	code, out, e := runCmd(t, "init", "--local", "--agents", "claude-code,codex")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	for _, id := range []string{"claude-code", "codex"} {
		a, _ := hooks.Get(id)
		if a.State(root) != hooks.Current {
			t.Fatalf("%s not hooked:\n%s", id, out)
		}
	}
	if a, _ := hooks.Get("cursor"); a.State(root) != hooks.Absent {
		t.Fatal("cursor was not asked for")
	}
	if _, err := os.Stat(filepath.Join(root, ".mcp.json")); err != nil {
		t.Fatal("memory entry missing")
	}
	if code, _, e := runCmd(t, "init", "--local", "--yes", "--agents", "nope"); code != 1 || !strings.Contains(e, "nope is not a supported agent") {
		t.Fatalf("exit %d: %s", code, e)
	}
}

func TestInitWithoutAgentFlagsHooksNothingWhenNotInteractive(t *testing.T) {
	root := gitRepo(t)
	if code, _, e := runCmd(t, "init", "--local"); code != 0 {
		t.Fatal(e)
	}
	for _, a := range hooks.All() {
		if a.State(root) != hooks.Absent {
			t.Fatalf("%s hooked without being asked", a.ID())
		}
	}
}

func TestSyncOffAndStatus(t *testing.T) {
	root := gitRepo(t)
	if code, out, e := runCmd(t, "sync"); code != 1 || !strings.Contains(e+out, "init") {
		t.Fatalf("exit %d: %s %s", code, out, e)
	}
	_, _, _ = runCmd(t, "init", "--local")
	code, out, e := runCmd(t, "sync")
	if code != 0 || !strings.Contains(out, "background sync off") {
		t.Fatalf("exit %d: %s %s", code, out, e)
	}
	_ = clistate.SavePrefs(root, clistate.Prefs{BackgroundSync: true})
	if _, out, _ := runCmd(t, "sync", "status"); !strings.Contains(out, "on (not running now") {
		t.Fatal(out)
	}
	if code, out, _ := runCmd(t, "sync", "off"); code != 0 || !strings.Contains(out, "Background sync is off.") {
		t.Fatal(out)
	}
	if clistate.LoadPrefs(root).BackgroundSync {
		t.Fatal("off must be remembered")
	}
	if code, _, _ := runCmd(t, "sync", "maybe"); code != 1 {
		t.Fatal("only on, off and status")
	}
}

func TestSettingsPrintsWithoutATerminal(t *testing.T) {
	gitRepo(t)
	_, _, _ = runCmd(t, "init", "--local", "--agents", "pi")
	code, out, e := runCmd(t, "settings")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, e)
	}
	for _, want := range []string{"Account", "Project", "Session capture", "on", "Shared memory", "Agents", "Pi ", "Sync", "Background sync", "About", "Version"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q:\n%s", want, out)
		}
	}
}

func TestSettingsBackendAppliesThroughTheCommands(t *testing.T) {
	root := gitRepo(t)
	_, _, _ = runCmd(t, "init", "--local")
	be := &settingsBackend{cwd: root}
	ctx := t.Context()
	if msg, err := be.Apply(ctx, "agent:opencode", true); err != nil || !strings.Contains(msg, "OpenCode") {
		t.Fatalf("%q %v", msg, err)
	}
	if a, _ := hooks.Get("opencode"); a.State(root) != hooks.Current {
		t.Fatal("not installed")
	}
	if msg, err := be.Apply(ctx, "agent:opencode", false); err != nil || !strings.Contains(msg, "removed") {
		t.Fatalf("%q %v", msg, err)
	}
	if msg, err := be.Apply(ctx, "capture", false); err != nil || !strings.Contains(msg, "paused") {
		t.Fatalf("%q %v", msg, err)
	}
	if msg, err := be.Apply(ctx, "memory", false); err != nil || !strings.Contains(msg, "off") {
		t.Fatalf("%q %v", msg, err)
	}
	secs, _ := be.Load(ctx)
	for _, s := range secs {
		for _, it := range s.Items {
			if (it.Key == "capture" || it.Key == "memory") && it.On {
				t.Fatalf("%s still on", it.Key)
			}
		}
	}
}

func TestInitCreatesTheMemoryRecord(t *testing.T) {
	root := gitRepo(t)
	if code, _, e := runCmd(t, "init", "--local"); code != 0 {
		t.Fatal(e)
	}
	if _, err := os.Stat(filepath.Join(root, ".atlas", "memory", "memory.sqlite")); err != nil {
		t.Fatalf("no memory record after init: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".atlas", ".gitignore")); err != nil {
		t.Fatal(err)
	}
}
