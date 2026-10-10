package main

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/tryatlas/atlas-cli/internal/auth"
)

// TestMain keeps every command test away from the real session file and the
// real auth host: a test that needs auth installs its own fake.
func TestMain(m *testing.M) {
	newAuth = func() (*auth.Auth, error) { return nil, errors.New("test did not install a fake auth") }
	interactive = func() bool { return false }
	_ = os.Setenv("ATLAS_INGEST_URL", "http://127.0.0.1:1") // unreachable unless a test installs a fake
	os.Exit(m.Run())
}

// wantCommands is the full command surface from SPEC §1.1, §3.7, §4.4, §5.5,
// §6.7, §7.4–7.6 and §8, plus D13's memory export/import and verify.
var wantCommands = []string{
	"capture disable",
	"capture enable",
	"capture retry",
	"env",
	"hook claude session-end",
	"hook claude session-start",
	"hook claude stop",
	"hook claude user-prompt-submit",
	"hook codex notify",
	"hook gemini session-start",
	"init",
	"login",
	"logout",
	"mcp-bridge",
	"memory disable",
	"memory enable",
	"memory export",
	"memory import",
	"memory verify",
	"org create",
	"org list",
	"org use",
	"run",
	"serve",
	"shell",
	"status",
	"statusline claude",
	"whoami",
}

func TestCommandSurface(t *testing.T) {
	got := commandPaths(newRootCmd())
	slices.Sort(got)
	if !slices.Equal(got, wantCommands) {
		t.Fatalf("command paths differ\n got: %q\nwant: %q", got, wantCommands)
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
}

func TestStubsReportNotImplemented(t *testing.T) {
	for _, path := range wantCommands {
		if implemented[path] {
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
