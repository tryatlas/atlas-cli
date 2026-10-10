package ambient

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const userMCP = `{
  "mcpServers": {
    "github": {
      "command": "gh-mcp"
    }
  }
}
`

const userSettings = `{
  "model": "opus",
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "say done"
          }
        ]
      }
    ]
  },
  "statusLine": {
    "type": "command",
    "command": "~/bin/my-status"
  }
}
`

func write(t *testing.T, p, s string) {
	t.Helper()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<missing>"
	}
	return string(b)
}

func TestClaudeAdapterInstallsAndRemovesExactly(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".mcp.json"), userMCP)
	write(t, filepath.Join(dir, ".claude", "settings.json"), userSettings)
	c := Claude{Dir: dir, Atlas: "/opt/atlas bin/atlas"}
	st, err := LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // idempotent
		if err := c.InstallMCP(st); err != nil {
			t.Fatal(err)
		}
		if err := c.InstallHooks(st); err != nil {
			t.Fatal(err)
		}
		if err := c.InstallStatus(st); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.Save()

	mcp := read(t, filepath.Join(dir, ".mcp.json"))
	wantMCP := `{
  "mcpServers": {
    "github": {
      "command": "gh-mcp"
    },
    "atlas": {
      "type": "stdio",
      "command": "/opt/atlas bin/atlas",
      "args": [
        "mcp-bridge",
        "--project",
        "` + dir + `",
        "--agent",
        "claude-code"
      ]
    }
  }
}
`
	if mcp != wantMCP {
		t.Fatalf(".mcp.json:\n%s", mcp)
	}
	if strings.Contains(mcp, "TOKEN") {
		t.Fatal("a secret in .mcp.json")
	}
	settings := read(t, filepath.Join(dir, ".claude", "settings.json"))
	for _, want := range []string{`"model": "opus"`, `"command": "say done"`,
		`"command": "'/opt/atlas bin/atlas' hook claude session-start"`, `"command": "'/opt/atlas bin/atlas' hook claude user-prompt-submit"`,
		`"command": "'/opt/atlas bin/atlas' hook claude stop"`, `"command": "'/opt/atlas bin/atlas' hook claude session-end"`,
		`"command": "'/opt/atlas bin/atlas' statusline claude"`} {
		if !strings.Contains(settings, want) {
			t.Errorf("settings.json lacks %s:\n%s", want, settings)
		}
	}
	if n := strings.Count(settings, "hook claude stop"); n != 1 {
		t.Fatalf("stop hook installed %d times", n)
	}
	if WrappedStatusCommand(st) != "~/bin/my-status" {
		t.Fatalf("wrapped %q", WrappedStatusCommand(st))
	}
	if local := read(t, filepath.Join(dir, ".claude", "settings.local.json")); local != "{\n  \"enabledMcpjsonServers\": [\n    \"atlas\"\n  ]\n}\n" {
		t.Fatalf("settings.local.json:\n%s", local)
	}

	st2, _ := LoadState(dir) // removal works from what cli.json recorded
	if err := c.RemoveStatus(st2); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveHooks(st2); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveMCP(st2); err != nil {
		t.Fatal(err)
	}
	_ = st2.Save()
	if got := read(t, filepath.Join(dir, ".mcp.json")); got != userMCP {
		t.Fatalf(".mcp.json not restored:\n%s", got)
	}
	if got := read(t, filepath.Join(dir, ".claude", "settings.json")); got != userSettings {
		t.Fatalf("settings.json not restored:\n%s", got)
	}
	if got := read(t, filepath.Join(dir, ".claude", "settings.local.json")); got != "<missing>" {
		t.Fatalf("settings.local.json left behind:\n%s", got)
	}
	if got := read(t, filepath.Join(dir, ".atlas", "cli.json")); got != "{\n  \"version\": 1,\n  \"agents\": {}\n}\n" {
		t.Fatalf("cli.json:\n%s", got)
	}
}

func TestFreshProjectFilesAreRemovedAgain(t *testing.T) {
	dir := t.TempDir()
	c := Claude{Dir: dir, Atlas: "atlas"}
	st, _ := LoadState(dir)
	_ = c.InstallMCP(st)
	_ = c.InstallHooks(st)
	_ = c.InstallStatus(st)
	_ = c.RemoveMCP(st)
	_ = c.RemoveHooks(st)
	_ = c.RemoveStatus(st)
	for _, p := range []string{".mcp.json", ".claude/settings.json", ".claude/settings.local.json"} {
		if got := read(t, filepath.Join(dir, p)); got != "<missing>" {
			t.Errorf("%s left behind: %s", p, got)
		}
	}
}

func TestQuote(t *testing.T) {
	for in, want := range map[string]string{"atlas": "atlas", "/a b/atlas": "'/a b/atlas'", "it's": `'it'\''s'`} {
		if got := quote(in); got != want {
			t.Errorf("quote(%q) = %q", in, got)
		}
	}
}
