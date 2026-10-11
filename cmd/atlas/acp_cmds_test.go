package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cachedRegistry writes a fresh registry cache so no test fetches one.
func cachedRegistry(t *testing.T) string {
	t.Helper()
	data := t.TempDir()
	t.Setenv("XDG_DATA_HOME", data)
	dir := filepath.Join(data, "dev.atlas.ide", "external-agents", "registry")
	_ = os.MkdirAll(dir, 0o755)
	reg := `{"version":"1.0.0","agents":[
	 {"id":"npx-agent","name":"Npx Agent","version":"1.2.0","description":"d","distribution":{"npx":{"package":"npx-agent@1.2.0"}}},
	 {"id":"bin-only","name":"Elsewhere","version":"2.0.0","description":"d","distribution":{"binary":{"plan9-mips":{"archive":"https://x/a.zip","cmd":"a"}}}}]}`
	if err := os.WriteFile(filepath.Join(dir, "registry.json"), []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestAgentsListInstallUninstall(t *testing.T) {
	data := cachedRegistry(t)
	code, out, e := runCmd(t, "acp", "list")
	if code != 0 || !strings.Contains(out, "npx-agent") || !strings.Contains(out, "available") || !strings.Contains(out, "not for ") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	if code, out, e := runCmd(t, "acp", "install", "npx-agent"); code != 0 || !strings.Contains(out, "Installed Npx Agent 1.2.0") {
		t.Fatalf("%d %s %s", code, out, e)
	}
	b, _ := os.ReadFile(filepath.Join(data, "dev.atlas.ide", "external-agents", "installed.json"))
	if string(b) != "{\n  \"npx-agent\": {\n    \"type\": \"registry\"\n  }\n}" {
		t.Fatalf("installed.json: %q", b)
	}
	if code, _, e := runCmd(t, "acp", "install", "bin-only"); code == 0 || !strings.Contains(e, "does not run on") {
		t.Fatalf("%d %s", code, e)
	}
	if _, out, _ := runCmd(t, "acp", "list"); !strings.Contains(out, "installed") {
		t.Fatal(out)
	}
	if code, out, _ := runCmd(t, "acp", "uninstall", "npx-agent"); code != 0 || !strings.Contains(out, "Uninstalled") {
		t.Fatal(out)
	}
	if code, _, e := runCmd(t, "acp", "uninstall", "npx-agent"); code == 0 || !strings.Contains(e, "not installed") {
		t.Fatal(e)
	}
}

func TestRunWithoutPromptNeedsTheChatScreen(t *testing.T) {
	cachedRegistry(t)
	if code, _, e := runCmd(t, "acp", "run", "npx-agent"); code == 0 || !strings.Contains(e, "--prompt") {
		t.Fatalf("%d %s", code, e)
	}
}
