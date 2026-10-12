// Package hosts resolves the Atlas service base URLs (SPEC §3.2,
// evidence-auth.md §1). There is deliberately no on-disk override: a file that
// redirects sign-in is a phishing foothold.
package hosts

import (
	"os"
	"strings"
)

// Build-time overrides, set with -ldflags "-X github.com/tryatlas/cli/internal/hosts.buildAuth=…"
// (the desktop's option_env! rung, used for staging builds).
var (
	buildAuth   string
	buildIngest string
	buildChat   string
	buildWeb    string
)

// Defaults for the shipped app.
const (
	DefaultAuth    = "https://auth.tryatlas.cc/api/auth"
	DefaultIngest  = "https://ingest.tryatlas.cc"
	DefaultChat    = "https://chat.tryatlas.cc"
	DefaultWeb     = "https://app.tryatlas.cc"
	DefaultGateway = "https://ai.tryatlas.cc/v1"
)

// Auth is the auth host including its /api/auth prefix.
func Auth() string { return resolve("ATLAS_AUTH_URL", buildAuth, DefaultAuth) }

// Ingest is the capture sync and workspaces host.
func Ingest() string { return resolve("ATLAS_INGEST_URL", buildIngest, DefaultIngest) }

// Chat is the team chat host.
func Chat() string { return resolve("ATLAS_CHAT_URL", buildChat, DefaultChat) }

// Web is the web app (links only).
func Web() string { return resolve("ATLAS_WEB_URL", buildWeb, DefaultWeb) }

// Gateway is the AI gateway; the desktop has no override for it.
func Gateway() string { return DefaultGateway }

// resolve walks env → build-time → default. A blank or whitespace value falls
// through. The desktop does this only for auth and ingest; the CLI does it for
// every host, so a blank variable never yields an empty URL.
func resolve(env, build, def string) string {
	if v := Normalize(os.Getenv(env)); v != "" {
		return v
	}
	if v := Normalize(build); v != "" {
		return v
	}
	return def
}

// Normalize trims whitespace and every trailing slash. Endpoint paths are
// appended by concatenation and always start with "/".
func Normalize(u string) string {
	return strings.TrimRight(strings.TrimSpace(u), "/")
}
