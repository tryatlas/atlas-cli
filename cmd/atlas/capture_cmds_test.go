package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatesWriteNoAgentConfig(t *testing.T) {
	root := gitRepo(t)
	if code, _, e := runCmd(t, "init", "--local"); code != 0 {
		t.Fatal(e)
	}
	if code, out, e := runCmd(t, "memory", "enable"); code != 0 || strings.TrimSpace(out) != "Shared memory is on for "+root+"." {
		t.Fatalf("%d %q %s", code, out, e)
	}
	if code, out, e := runCmd(t, "capture", "enable"); code != 0 || strings.TrimSpace(out) != "Capture is on for "+root+"." {
		t.Fatalf("%d %q %s", code, out, e)
	}
	for _, p := range []string{".mcp.json", ".claude", ".gemini", ".codex", ".atlas/cli.json"} {
		if _, err := os.Stat(filepath.Join(root, p)); err == nil {
			t.Errorf("%s written (ADR-0009: no agent config files)", p)
		}
	}
}

func TestCaptureSyncOnALocalProject(t *testing.T) {
	gitRepo(t)
	_, _, _ = runCmd(t, "init", "--local")
	code, out, e := runCmd(t, "capture", "sync")
	if code != 0 || !strings.Contains(out, "This Project is local") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	code, out, e = runCmd(t, "capture", "retry")
	if code != 0 || !strings.Contains(out, "Requeued 0 failed row(s).") {
		t.Fatalf("%d %s %s", code, out, e)
	}
}

func TestServeStopWithoutAListener(t *testing.T) {
	gitRepo(t)
	_, _, _ = runCmd(t, "init", "--local")
	code, out, _ := runCmd(t, "serve", "--stop")
	if code != 0 || !strings.Contains(out, "No listener is running") {
		t.Fatalf("%d %s", code, out)
	}
}
