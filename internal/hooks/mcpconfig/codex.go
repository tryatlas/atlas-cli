package mcpconfig

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/tryatlas/cli/internal/hooks"
)

// Codex: MCP servers live in TOML under [mcp_servers.<name>]. The project's
// .codex/config.toml is layered over ~/.codex/config.toml, but Codex loads a
// project config only for a trusted project (trust_level = "trusted" in the
// user's config), so the entry is inert until the user trusts the checkout.
//
// The file is edited as text: a TOML round trip would drop the user's
// comments, so the entry sits in a managed block between two marker lines
// and everything outside the block is preserved byte for byte.

const codexID = "codex"

func init() { Register(codexID, codexWriter{}) }

// Marker lines around the managed block.
const (
	codexBegin = "# atlas_memory: managed by atlas agents add (begin)"
	codexEnd   = "# atlas_memory: managed by atlas agents add (end)"
)

// codexHeader is the table the block defines; one outside the block is the
// user's own and is left alone.
const codexHeader = "[mcp_servers." + ServerName + "]"

type codexWriter struct{}

// Path is the project-level Codex config.
func (codexWriter) Path(root string) string { return filepath.Join(root, ".codex", "config.toml") }

// codexBlock renders the managed block for root.
func codexBlock(root string) string {
	args := make([]string, 0, 5)
	for _, a := range Args(root, codexID) {
		args = append(args, tomlString(a))
	}
	return codexBegin + "\n" +
		codexHeader + "\n" +
		"command = " + tomlString(hooks.Bin) + "\n" +
		"args = [" + strings.Join(args, ", ") + "]\n" +
		codexEnd + "\n"
}

// tomlString quotes s as a TOML basic string. JSON's escapes are a subset of
// TOML's, so the JSON encoding (without HTML escaping) is valid TOML.
func tomlString(s string) string { return string(hooks.Raw(s)) }

// splitBlock finds the managed block: the text before it, the block itself
// (marker lines included) and the text after. Without a block, before holds
// the whole file.
func splitBlock(text string) (before, block, after string, found bool) {
	i := strings.Index(text, codexBegin)
	if i < 0 || (i > 0 && text[i-1] != '\n') {
		return text, "", "", false
	}
	j := strings.Index(text[i:], codexEnd)
	if j < 0 {
		return text, "", "", false
	}
	end := i + j + len(codexEnd)
	if k := strings.IndexByte(text[end:], '\n'); k >= 0 {
		end += k + 1
	} else {
		end = len(text)
	}
	return text[:i], text[i:end], text[end:], true
}

// unmanagedEntry reports a [mcp_servers.atlas_memory] table outside the
// block; adding a second one would make the file invalid TOML.
func unmanagedEntry(before, after string) bool {
	for _, s := range []string{before, after} {
		for _, l := range strings.Split(s, "\n") {
			if strings.TrimSpace(l) == codexHeader {
				return true
			}
		}
	}
	return false
}

func readCodex(path string) (string, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the agent config the user asked to edit
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// Write adds or refreshes the managed block at the end of the file.
func (w codexWriter) Write(root string) (bool, error) {
	path := w.Path(root)
	text, err := readCodex(path)
	if err != nil {
		return false, err
	}
	before, block, after, found := splitBlock(text)
	if unmanagedEntry(before, after) {
		return false, fmt.Errorf("%s: %s already has an %s entry Atlas does not manage", path, filepath.Base(path), ServerName)
	}
	want := codexBlock(root)
	if found && block == want {
		return false, nil
	}
	var out string
	switch {
	case found:
		out = before + want + after
	case strings.TrimSpace(text) == "":
		out = want
	default:
		out = text
		if !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += "\n" + want
	}
	return true, writeCodex(path, out)
}

// Remove deletes the managed block, and the file when nothing else is left.
func (w codexWriter) Remove(root string) error {
	path := w.Path(root)
	text, err := readCodex(path)
	if err != nil {
		return err
	}
	before, _, after, found := splitBlock(text)
	if !found {
		return nil
	}
	// Drop the blank line Write put before the block.
	before = strings.TrimRight(before, "\n")
	if before != "" {
		before += "\n"
	}
	out := before + after
	if strings.TrimSpace(out) == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeCodex(path, out)
}

// State compares the managed block with what Write would put there.
func (w codexWriter) State(root string) hooks.InstallState {
	text, err := readCodex(w.Path(root))
	if err != nil {
		return hooks.Absent
	}
	_, block, _, found := splitBlock(text)
	switch {
	case !found:
		return hooks.Absent
	case block == codexBlock(root):
		return hooks.Current
	}
	return hooks.Outdated
}

// writeCodex writes the file atomically, creating .codex.
func writeCodex(path, text string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".atlas-tmp"
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil { //nolint:gosec // G306: a project config file, no secret in it
		return err
	}
	return os.Rename(tmp, path)
}
