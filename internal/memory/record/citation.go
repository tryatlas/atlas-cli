package record

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/zeebo/blake3"
)

// Citation limits (ADR-0018).
const (
	MaxCitations     = 8
	MaxCitationLines = 200
	MaxCitedFile     = 2 << 20
)

// Citation is one piece of code evidence, stored in entries.evidence. The
// hash is computed by the server from disk; an agent never supplies it.
type Citation struct {
	Path      string  `json:"path"` // repository-relative, "/"-separated
	StartLine int     `json:"start_line"`
	EndLine   int     `json:"end_line"`
	Symbol    *string `json:"symbol,omitempty"`
	Hash      string  `json:"hash"`
}

// Lines renders "12-30" or "12".
func (c Citation) Lines() string {
	if c.StartLine == c.EndLine {
		return strconv.Itoa(c.StartLine)
	}
	return fmt.Sprintf("%d-%d", c.StartLine, c.EndLine)
}

// Validity of evidence.
const (
	Valid        = "valid"
	Moved        = "moved"
	Stale        = "stale"
	Unverifiable = "unverifiable"
)

// CiteError is a refusal with the desktop's wording (without the
// "not remembered: " prefix).
type CiteError struct{ Msg string }

func (e *CiteError) Error() string { return e.Msg }

func citeErr(format string, a ...any) error { return &CiteError{Msg: fmt.Sprintf(format, a...)} }

// ParseLines reads "12-30" or "12" (1-based, inclusive).
func ParseLines(s string) (int, int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, 0, citeErr(`give lines ("12-30"); the symbol is used to find them again later`)
	}
	a, b, isRange := strings.Cut(s, "-")
	start, err1 := strconv.Atoi(strings.TrimSpace(a))
	end := start
	var err2 error
	if isRange {
		end, err2 = strconv.Atoi(strings.TrimSpace(b))
	}
	if err1 != nil || err2 != nil || start < 1 || end < 1 {
		return 0, 0, citeErr("bad lines `%s`: give \"12-30\" or \"12\"", s)
	}
	if end < start {
		return 0, 0, citeErr("lines %d-%d are not a range", start, end)
	}
	if end-start+1 > MaxCitationLines {
		return 0, 0, citeErr("a citation spans at most %d lines", MaxCitationLines)
	}
	return start, end, nil
}

// Cite resolves path against cwd, checks it lies inside one of roots (the
// scope root and its worktrees), reads the lines now and hashes them.
func Cite(roots []string, cwd, path, lines string, symbol *string) (Citation, error) {
	start, end, err := ParseLines(lines)
	if err != nil {
		return Citation{}, err
	}
	abs := path
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(cwd, path)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Citation{}, citeErr("`%s` does not exist", path)
	}
	rel, ok := "", false
	for _, root := range roots {
		r, err := filepath.Rel(root, resolved)
		if err == nil && r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)) && r != "." {
			rel, ok = filepath.ToSlash(r), true
			break
		}
	}
	if !ok {
		return Citation{}, citeErr("`%s` is outside the repository", path)
	}
	fileLines, err := readTextLines(resolved)
	if err != nil {
		return Citation{}, citeErr("`%s` can't be read as text", path)
	}
	if end > len(fileLines) {
		return Citation{}, citeErr("`%s` has %d lines", path, len(fileLines))
	}
	if symbol != nil && strings.TrimSpace(*symbol) == "" {
		symbol = nil
	}
	return Citation{Path: rel, StartLine: start, EndLine: end, Symbol: symbol,
		Hash: hashLines(fileLines[start-1 : end])}, nil
}

// readTextLines reads a file of at most 2 MiB that is valid UTF-8 without NUL bytes.
func readTextLines(path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxCitedFile {
		return nil, fmt.Errorf("not a text file")
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: a path checked to lie inside the repository
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return nil, fmt.Errorf("binary")
	}
	s := strings.TrimSuffix(string(b), "\n")
	if s == "" {
		return []string{}, nil
	}
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n"), nil
}

// hashLines is blake3 over the cited lines, whitespace collapsed per line and
// joined with "\n", as lowercase hex.
func hashLines(lines []string) string {
	h := blake3.New()
	for i, l := range lines {
		if i > 0 {
			_, _ = h.Write([]byte{'\n'})
		}
		_, _ = h.Write([]byte(strings.Join(strings.Fields(l), " ")))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Check re-reads a citation now: the same lines hash the same → valid; the
// file is missing → stale; it can't be read as text → unverifiable; else a
// window of the same length that hashes the same → moved (with the new lines,
// preferring one that contains the symbol); else stale.
func Check(scopeRoot string, c Citation) (string, Citation) {
	path := filepath.Join(scopeRoot, filepath.FromSlash(c.Path))
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return Stale, c
		}
		return Unverifiable, c
	}
	lines, err := readTextLines(path)
	if err != nil {
		return Unverifiable, c
	}
	n := c.EndLine - c.StartLine + 1
	if c.EndLine <= len(lines) && hashLines(lines[c.StartLine-1:c.EndLine]) == c.Hash {
		return Valid, c
	}
	best := -1
	for i := 0; i+n <= len(lines); i++ {
		if hashLines(lines[i:i+n]) != c.Hash {
			continue
		}
		if best < 0 {
			best = i
		}
		if c.Symbol != nil && strings.Contains(strings.Join(lines[i:i+n], "\n"), *c.Symbol) {
			best = i
			break
		}
	}
	if best < 0 {
		return Stale, c
	}
	moved := c
	moved.StartLine, moved.EndLine = best+1, best+n
	return Moved, moved
}

// Overall combines several validities: stale if any is stale, unverifiable
// only if all are, else moved if any moved, else valid.
func Overall(vs []string) string {
	if len(vs) == 0 {
		return ""
	}
	unverifiable, moved := 0, false
	for _, v := range vs {
		switch v {
		case Stale:
			return Stale
		case Unverifiable:
			unverifiable++
		case Moved:
			moved = true
		}
	}
	switch {
	case unverifiable == len(vs):
		return Unverifiable
	case moved:
		return Moved
	}
	return Valid
}
