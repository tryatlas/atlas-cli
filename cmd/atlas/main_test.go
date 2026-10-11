package main

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/auth"
	"github.com/tryatlas/atlas-cli/internal/hooks"
)

// TestMain keeps every command test away from the real session file and the
// real auth host: a test that needs auth installs its own fake.
func TestMain(m *testing.M) {
	newAuth = func() (*auth.Auth, error) { return nil, errors.New("test did not install a fake auth") }
	interactive = func() bool { return false }
	_ = os.Setenv("ATLAS_INGEST_URL", "http://127.0.0.1:1") // unreachable unless a test installs a fake
	data, _ := os.MkdirTemp("", "atlas-data-")              // never the user's agents
	_ = os.Setenv("XDG_DATA_HOME", data)
	state, _ := os.MkdirTemp("", "atlas-state-") // never the user's CLI state
	_ = os.Setenv("XDG_STATE_HOME", state)
	os.Exit(m.Run())
}

// wantCommands is the full command surface from SPEC §1.1, §3.7, §4.4, §5.5,
// §6.7, §7.4–7.6 and §8, plus D13's memory export/import and verify.
var wantCommands = []string{
	"acp check",
	"acp detect",
	"acp import",
	"acp install",
	"acp list",
	"acp logout",
	"acp run",
	"acp sessions",
	"acp uninstall",
	"agents add",
	"agents list",
	"agents remove",
	"agents sessions",
	"capture disable",
	"capture enable",
	"capture link",
	"capture retry",
	"capture sync",
	"hooks git post-commit",
	"init",
	"login",
	"logout",
	"mcp-bridge",
	"memory browse",
	"memory disable",
	"memory enable",
	"memory export",
	"memory import",
	"memory list",
	"memory verify",
	"org create",
	"org list",
	"org use",
	"serve",
	"status",
	"whoami",
}

// allCommands is wantCommands plus one hooks entry point per registered
// agent verb (the integrations declare their own verbs).
func allCommands() []string {
	out := append([]string(nil), wantCommands...)
	for _, a := range hooks.All() {
		for _, v := range a.Verbs() {
			out = append(out, "hooks "+a.ID()+" "+v)
		}
	}
	slices.Sort(out)
	return out
}

func TestCommandSurface(t *testing.T) {
	got := commandPaths(newRootCmd())
	slices.Sort(got)
	if want := allCommands(); !slices.Equal(got, want) {
		t.Fatalf("command paths differ\n got: %q\nwant: %q", got, want)
	}
	if len(hooks.All()) != 8 {
		t.Fatalf("agents registered = %d, want 8", len(hooks.All()))
	}
}

// argsFor gives each command the positional arguments it requires.
var argsFor = map[string][]string{
	"org use":       {"org_1"},
	"org create":    {"Acme"},
	"memory export": {"out.md"},
	"memory import": {"in.md"},
}

// implemented lists commands that no longer stub; they have their own tests.
var implemented = map[string]bool{
	"login": true, "logout": true, "whoami": true, "org list": true, "org use": true, "org create": true,
	"init": true, "status": true, "memory enable": true, "memory disable": true,
	"capture enable": true, "capture disable": true,
	"memory verify": true, "memory export": true, "memory import": true, "mcp-bridge": true, "serve": true,
	"capture link": true, "capture sync": true, "acp import": true, "capture retry": true, "acp run": true, "memory browse": true, "memory list": true,
	"acp check": true, "acp detect": true, "acp install": true, "acp list": true, "acp sessions": true, "acp logout": true, "acp uninstall": true,
	"agents add": true, "agents list": true, "agents remove": true, "agents sessions": true,
	"hooks git post-commit": true,
}

func TestStubsReportNotImplemented(t *testing.T) {
	for _, path := range wantCommands {
		if implemented[path] || strings.HasPrefix(path, "hooks ") {
			continue
		}
		t.Run(path, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			args := append(strings.Fields(path), argsFor[path]...)
			code := run(args, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("exit code = %d, want 2 (stderr %q)", code, stderr.String())
			}
			want := "atlas " + path + ": not implemented\n"
			if stderr.String() != want {
				t.Fatalf("stderr = %q, want %q", stderr.String(), want)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty", stdout.String())
			}
		})
	}
}

func TestBadArgsAreUsageErrors(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"org", "use"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.HasPrefix(stderr.String(), "atlas: ") {
		t.Fatalf("stderr = %q, want an atlas: error", stderr.String())
	}
}

func TestVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got := stdout.String(); got != "atlas dev\n" {
		t.Fatalf("stdout = %q", got)
	}
}
