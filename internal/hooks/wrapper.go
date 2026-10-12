package hooks

import (
	"strings"
)

// Bin is the CLI's executable name, as agent configs and hooks call it.
const Bin = "atlas-cli"

// Prefix is what every Atlas hook command starts with. Ownership is decided
// by this prefix, never by "atlas-cli " alone, so a user's own hook that runs
// atlas-cli status is never pruned.
const Prefix = Bin + " hooks "

// legacyPrefix is what hooks written before the rename to atlas-cli start
// with; they are still Atlas's, so add and remove replace them.
const legacyPrefix = "atlas hooks "

// Command is the bare hook command for an agent and verb.
func Command(agentID, verb string) string { return Prefix + agentID + " " + verb }

// Wrap guards a hook command so a machine without atlas-cli on PATH runs a
// silent no-op instead of failing the agent's operation.
func Wrap(cmd string) string {
	return "sh -c 'if ! command -v " + Bin + " >/dev/null 2>&1; then exit 0; fi; exec " + cmd + "'"
}

// WrapWithMessage is Wrap for a SessionStart hook: a missing atlas-cli prints
// the agent-shaped message instead (JSON for agents that read it).
func WrapWithMessage(cmd, message string) string {
	return "sh -c 'if ! command -v " + Bin + " >/dev/null 2>&1; then printf \"%s\\n\" " + shellQuote(message) + "; exit 0; fi; exec " + cmd + "'"
}

// shellQuote quotes s for a single-quoted sh -c body: double quotes, with
// the characters sh expands inside them escaped.
func shellQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`", `'`, `'\''`)
	return `"` + r.Replace(s) + `"`
}

// MissingMessage is the message a Claude-shaped agent shows when the hook
// is installed but atlas-cli is not.
const MissingMessage = `{"systemMessage":"Atlas is enabled here but atlas-cli is not installed or not on PATH. Install: https://github.com/tryatlas/atlas-cli#install"}`

// Owned reports that a hook command is Atlas's: the bare command, or the
// bare command inside a Wrap.
func Owned(command string) bool {
	c := strings.TrimSpace(command)
	if hasPrefix(c) {
		return true
	}
	if i := strings.LastIndex(c, "; exec "); i >= 0 {
		return hasPrefix(strings.TrimSuffix(strings.TrimSpace(c[i+len("; exec "):]), "'"))
	}
	return false
}

func hasPrefix(c string) bool {
	return strings.HasPrefix(c, Prefix) || strings.HasPrefix(c, legacyPrefix)
}
