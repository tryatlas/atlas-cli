// Package record turns agent activity into capture rows: prompts and titles,
// assistant runs, tool calls with file touches and edit patches, usage and
// turn boundaries (SPEC §6.4, evidence-capture.md §4).
package record

import (
	"bytes"
	"encoding/json"
	"fmt"
	hashfnv "hash/fnv"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// ── Host machinery (evidence-capture.md §4.8) ────────────────────────────────

const nextStepsMarker = "═══ Atlas next-steps ═══"

// StripUser cuts everything from the Atlas next-steps marker on.
func StripUser(s string) string {
	if i := strings.Index(s, nextStepsMarker); i >= 0 {
		return s[:i]
	}
	return s
}

var nextStepsBlock = regexp.MustCompile(`(?s)<next_steps>.*?</next_steps>`)

// StripAssistant removes a closed <next_steps>…</next_steps> block; an
// unclosed one is kept verbatim.
func StripAssistant(s string) string {
	return nextStepsBlock.ReplaceAllString(s, "")
}

// ── Titles (evidence-capture.md §4.7) ────────────────────────────────────────

var (
	droppedBlocks = []string{"system-reminder", "local-command-caveat", "local-command-stdout", "local-command-stderr",
		"command-message", "command-stdout", "command-stderr", "bash-stdout", "bash-stderr", "task-notification",
		"user-prompt-submit-hook", "ide_opened_file", "ide_selection", "ide_diagnostics", "atlas-memory"}
	unwrapped        = []string{"bash-input", "user_query"}
	commandName      = regexp.MustCompile(`(?s)<command-name>\s*(.*?)\s*</command-name>`)
	commandArgs      = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
	pastedContent    = regexp.MustCompile(`(?s)<pasted_content[^>]*>(.*?)</pasted_content>`)
	imagePlaceholder = regexp.MustCompile(`\[Image #\d+\]`)
	pastePlaceholder = regexp.MustCompile(`\[Pasted text #\d+[^\]]*\]`)
	onlyTag          = regexp.MustCompile(`^</?[A-Za-z_][\w-]*[^>]*>$`)
)

func blockRE(tag string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)<` + regexp.QuoteMeta(tag) + `(?:\s[^>]*)?>.*?</` + regexp.QuoteMeta(tag) + `>`)
}

var (
	droppedREs   []*regexp.Regexp
	unwrapOpenRE = regexp.MustCompile(`</?(?:bash-input|user_query)(?:\s[^>]*)?>`)
)

func init() {
	for _, t := range droppedBlocks {
		droppedREs = append(droppedREs, blockRE(t))
	}
	_ = unwrapped
}

// Title derives a session title from the first prompt (already redacted):
// a slash command, else the first meaningful line of the prose, else of the
// first pasted content, else "Pasted text", else "Image". At most 120
// characters including the ellipsis.
func Title(prompt string) string {
	if m := commandName.FindStringSubmatch(prompt); m != nil {
		name := strings.TrimSpace(m[1])
		if !strings.HasPrefix(name, "/") {
			name = "/" + name
		}
		if a := commandArgs.FindStringSubmatch(prompt); a != nil {
			if line := firstLine(a[1]); line != "" {
				return clip(name + " " + line)
			}
		}
		return clip(name)
	}
	hadImage := imagePlaceholder.MatchString(prompt)
	hadPaste := pastePlaceholder.MatchString(prompt) || pastedContent.MatchString(prompt)
	prose := prompt
	pastes := pastedContent.FindAllStringSubmatch(prose, -1)
	prose = pastedContent.ReplaceAllString(prose, "")
	for _, re := range droppedREs {
		prose = re.ReplaceAllString(prose, "")
	}
	prose = unwrapOpenRE.ReplaceAllString(prose, "")
	prose = imagePlaceholder.ReplaceAllString(prose, "")
	prose = pastePlaceholder.ReplaceAllString(prose, "")
	if line := firstLine(prose); line != "" {
		return clip(line)
	}
	for _, p := range pastes {
		if line := firstLine(p[1]); line != "" {
			return clip(line)
		}
	}
	switch {
	case hadPaste:
		return "Pasted text"
	case hadImage:
		return "Image"
	}
	return ""
}

// firstLine is the first line that is not blank and not only a tag, with
// markdown prefix characters stripped and whitespace collapsed.
func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || onlyTag.MatchString(t) {
			continue
		}
		t = strings.TrimLeft(t, "#>-* \t")
		t = strings.Join(strings.Fields(t), " ")
		if t != "" {
			return t
		}
	}
	return ""
}

// clip keeps 120 characters: the first 119, cut back to a word boundary when
// that keeps more than two thirds, then "…".
func clip(s string) string {
	r := []rune(s)
	if len(r) <= 120 {
		return s
	}
	cut := r[:119]
	if i := lastSpace(cut); i > len(cut)*2/3 {
		cut = cut[:i]
	}
	return strings.TrimRight(string(cut), " ") + "…"
}

func lastSpace(r []rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == ' ' {
			return i
		}
	}
	return -1
}

// ── Canonical tool names (evidence-capture.md §4.9) ──────────────────────────

var aliases = map[string]string{}

func init() {
	for name, words := range map[string]string{
		"Read":   "read readfile view cat",
		"Edit":   "edit multiedit str_replace str_replace_editor apply_patch applypatch patch notebookedit",
		"Write":  "write writefile create create_file createfile",
		"Bash":   "bash shell exec execute run powershell terminal",
		"Search": "grep glob search find find_files list ls codebase_search",
		"Fetch":  "fetch webfetch websearch web browse",
		"Delete": "delete rm remove",
		"Move":   "move mv rename",
		"Think":  "think thinking reason",
		"Task":   "task agent subagent dispatch",
	} {
		for _, w := range strings.Fields(words) {
			aliases[w] = name
		}
	}
}

func alias(token string) string { return aliases[strings.ToLower(token)] }

// leadingToken is the first token of s split on whitespace and ( : [ <, or
// the tool half of mcp__<server>__<tool> or <server>.<tool>.
func leadingToken(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	tok := strings.FieldsFunc(s, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' || strings.ContainsRune("(:[<", r) })
	if len(tok) == 0 {
		return nil
	}
	out := []string{tok[0]}
	if strings.HasPrefix(tok[0], "mcp__") {
		parts := strings.SplitN(tok[0], "__", 3)
		if len(parts) == 3 {
			out = append(out, parts[2])
		}
	} else if i := strings.LastIndexByte(tok[0], '.'); i > 0 && i < len(tok[0])-1 {
		out = append(out, tok[0][i+1:])
	}
	return out
}

// CanonicalToolName maps a wire tool to Read, Edit, Write, Bash, Search,
// Fetch, Delete, Move, Think, Task or Other.
func CanonicalToolName(wire, title, kind string, args json.RawMessage) string {
	var a map[string]json.RawMessage
	_ = json.Unmarshal(args, &a)
	for _, k := range []string{"tool_name", "toolName", "name"} {
		var s string
		if json.Unmarshal(a[k], &s) == nil {
			if n := alias(s); n != "" {
				return n
			}
		}
	}
	var cmd string
	if json.Unmarshal(a["command"], &cmd) == nil {
		return "Bash"
	}
	for _, src := range []string{wire, title} {
		for _, t := range leadingToken(src) {
			if n := alias(t); n != "" {
				return n
			}
		}
	}
	has := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := a[k]; ok {
				return true
			}
		}
		return false
	}
	switch strings.ToLower(kind) {
	case "read":
		return "Read"
	case "edit":
		switch {
		case has("old_string", "oldText", "old_str", "diff", "patch"):
			return "Edit"
		case has("content", "new_str"):
			return "Write"
		}
		return "Edit"
	case "execute":
		return "Bash"
	case "search":
		return "Search"
	case "fetch":
		return "Fetch"
	case "delete":
		return "Delete"
	case "move":
		return "Move"
	case "think":
		return "Think"
	}
	return "Other"
}

// WritesFiles reports the canonical names that write files.
func WritesFiles(name string) bool {
	return name == "Edit" || name == "Write" || name == "Delete" || name == "Move"
}

// ACPKind is the ACP kind string the desktop stores for a canonical name.
func ACPKind(name string) string {
	switch name {
	case "Read":
		return "read"
	case "Edit", "Write":
		return "edit"
	case "Bash":
		return "execute"
	case "Search":
		return "search"
	case "Fetch":
		return "fetch"
	case "Delete":
		return "delete"
	case "Move":
		return "move"
	case "Think":
		return "think"
	}
	return "other"
}

// ── Write paths and patches (evidence-capture.md §4.10, §4.12) ───────────────

var pathKeys = []string{"file_path", "path", "filePath", "target_file", "file", "notebook_path"}

// WritePaths are the files a call wrote: every location, else the first of
// file_path, path, filePath, target_file, file, notebook_path.
func WritePaths(args json.RawMessage, locations []string) []string {
	if len(locations) > 0 {
		return locations
	}
	var a map[string]json.RawMessage
	_ = json.Unmarshal(args, &a)
	for _, k := range pathKeys {
		var s string
		if json.Unmarshal(a[k], &s) == nil && s != "" {
			return []string{s}
		}
	}
	return nil
}

// EditPatch is arguments.patch or .diff verbatim, else a synthesised
// "--- before / +++ after" from the old and new strings.
func EditPatch(args json.RawMessage) string {
	var a map[string]json.RawMessage
	_ = json.Unmarshal(args, &a)
	str := func(keys ...string) (string, bool) {
		for _, k := range keys {
			var s string
			if json.Unmarshal(a[k], &s) == nil {
				return s, true
			}
		}
		return "", false
	}
	if p, ok := str("patch", "diff"); ok {
		return p
	}
	oldS, hasOld := str("old_string", "oldText", "old_str")
	newS, hasNew := str("new_string", "newText", "new_str", "content")
	if !hasOld && !hasNew {
		return ""
	}
	var b strings.Builder
	b.WriteString("--- before\n+++ after")
	if hasOld && oldS != "" {
		for _, l := range strings.Split(oldS, "\n") {
			b.WriteString("\n-" + l)
		}
	}
	if hasNew && newS != "" {
		for _, l := range strings.Split(newS, "\n") {
			b.WriteString("\n+" + l)
		}
	}
	return b.String()
}

// ProjectPath makes p project-relative with "/" and lexically collapsed
// "."/".."; a path outside root stays absolute and is flagged.
func ProjectPath(root, p string) (string, bool) {
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(root, p)
	}
	abs = filepath.Clean(abs)
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return abs, true
	}
	return filepath.ToSlash(rel), false
}

// ── Sketch (evidence-capture.md §3.4; checked on 257 desktop touches) ───────

const sketchMax = 512

func fnv(s string) uint32 {
	h := hashfnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func fnvHex(s string) string { return fmt.Sprintf("%x", fnv(s)) }

// Sketch is the comma-joined lowercase unpadded hex FNV-1a 32 of the
// distinct trimmed non-blank lines, sorted numerically, at most 512; nil for
// blank or binary content.
func Sketch(data []byte) *string {
	if len(data) == 0 || bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return nil
	}
	seen := map[uint32]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		if t := strings.TrimSpace(line); t != "" {
			seen[fnv(t)] = true
		}
	}
	if len(seen) == 0 {
		return nil
	}
	hs := make([]uint32, 0, len(seen))
	for h := range seen {
		hs = append(hs, h)
	}
	sort.Slice(hs, func(i, j int) bool { return hs[i] < hs[j] })
	if len(hs) > sketchMax {
		hs = hs[:sketchMax]
	}
	parts := make([]string, len(hs))
	for i, h := range hs {
		parts[i] = fmt.Sprintf("%x", h)
	}
	out := strings.Join(parts, ",")
	return &out
}
