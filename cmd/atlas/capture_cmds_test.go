package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnableWritesClaudeConfigAndDisableRemovesIt(t *testing.T) {
	root := gitRepo(t)
	_ = os.MkdirAll(filepath.Join(root, ".claude"), 0o755) // the Project uses Claude Code
	if code, _, e := runCmd(t, "init", "--local"); code != 0 {
		t.Fatal(e)
	}
	code, out, e := runCmd(t, "memory", "enable")
	if code != 0 || !strings.Contains(out, `claude-code: MCP server "atlas" installed`) {
		t.Fatalf("%d %s %s", code, out, e)
	}
	mcp, _ := os.ReadFile(filepath.Join(root, ".mcp.json"))
	if !strings.Contains(string(mcp), `"mcp-bridge"`) || !strings.Contains(string(mcp), `"claude-code"`) {
		t.Fatalf(".mcp.json: %s", mcp)
	}
	if code, out, e := runCmd(t, "capture", "enable"); code != 0 || !strings.Contains(out, "hooks and status line") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	settings, _ := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if !strings.Contains(string(settings), "hook claude user-prompt-submit") || !strings.Contains(string(settings), "statusline claude") {
		t.Fatalf("settings.json: %s", settings)
	}
	state, _ := os.ReadFile(filepath.Join(root, ".atlas", "cli.json"))
	if !strings.Contains(string(state), `"hooks": true`) || !strings.Contains(string(state), `"proxy": true`) {
		t.Fatalf("cli.json: %s", state)
	}
	_, _, _ = runCmd(t, "capture", "disable")
	_, _, _ = runCmd(t, "memory", "disable")
	for _, p := range []string{".mcp.json", ".claude/settings.json", ".claude/settings.local.json"} {
		if _, err := os.Stat(filepath.Join(root, p)); err == nil {
			b, _ := os.ReadFile(filepath.Join(root, p))
			t.Errorf("%s left behind: %s", p, b)
		}
	}
}

func TestHookBannerIsJSONOnly(t *testing.T) {
	root := gitRepo(t)
	_, _, _ = runCmd(t, "init", "--local")
	old := stdin
	stdin = strings.NewReader(`{"session_id":"abc","cwd":"` + root + `","source":"startup"}`)
	defer func() { stdin = old }()
	code, out, e := runCmd(t, "hook", "claude", "session-start")
	if code != 0 || e != "" {
		t.Fatalf("%d %s", code, e)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("not JSON: %q", out)
	}
	if m["systemMessage"] != "Atlas: capture off (run claude inside atlas shell) · memory on" ||
		m["hookSpecificOutput"].(map[string]any)["hookEventName"] != "SessionStart" {
		t.Fatalf("%v", m)
	}
	if _, ok := m["hookSpecificOutput"].(map[string]any)["additionalContext"]; ok {
		t.Fatal("nothing may be injected into the model's context (D6)")
	}
}

func TestHookNeverFails(t *testing.T) {
	t.Chdir(t.TempDir())
	old := stdin
	stdin = strings.NewReader(`not json`)
	defer func() { stdin = old }()
	if code, _, _ := runCmd(t, "hook", "claude", "user-prompt-submit"); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestStatusline(t *testing.T) {
	root := gitRepo(t)
	_, _, _ = runCmd(t, "init", "--local")
	old := stdin
	stdin = strings.NewReader(`{"workspace":{"current_dir":"` + root + `"}}`)
	defer func() { stdin = old }()
	_, out, _ := runCmd(t, "statusline", "claude")
	if strings.TrimSpace(out) != "Atlas · memory on · capture off" {
		t.Fatalf("%q", out)
	}
}

func TestEnvNeedsARunningListener(t *testing.T) {
	gitRepo(t)
	_, _, _ = runCmd(t, "init", "--local")
	code, _, e := runCmd(t, "env")
	if code != 1 || !strings.Contains(e, "start one with atlas shell") {
		t.Fatalf("%d %s", code, e)
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
