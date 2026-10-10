package record

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The Agent Memory Repo layout (evidence-memory.md §4.2): MEMORY.md with an
// index and the preferences, then one file per durable kind.
var amrFiles = []struct {
	kind, stem, title string
}{
	{KindDecision, "decisions", "Decisions and why"},
	{KindArchitecture, "architecture", "Architecture"},
	{KindFact, "facts", "Facts"},
	{KindFailure, "failures", "Failures"},
}

const amrMaxBytes = 2 << 20

// amrValue makes a metadata value safe inside [k: v; …].
var amrValue = strings.NewReplacer(";", ",", "[", "(", "]", ")")

// AMRLine renders one entry: "- <text> [source: …; added: …; atlas-id: …; revision: …; code: …]".
func AMRLine(e *Entry, source string) string {
	text := strings.Join(strings.Fields(e.Content), " ")
	meta := []string{
		"source: " + amrValue.Replace(source),
		"added: " + Date(e.CreatedAt),
		fmt.Sprintf("atlas-id: %d", e.ID),
		fmt.Sprintf("revision: %d", e.Rev),
	}
	for _, c := range e.Evidence {
		meta = append(meta, "code: "+amrValue.Replace(fmt.Sprintf("%s#L%d-%d", c.Path, c.StartLine, c.EndLine)))
	}
	return "- " + text + " [" + strings.Join(meta, "; ") + "]"
}

// Export writes the active, non-stale durable entries to dir in the Agent
// Memory Repo layout and returns how many lines were written.
func (s *Store) Export(ctx context.Context, dir string) (int, error) {
	entries, err := s.Active(ctx, DurableKinds...)
	if err != nil {
		return 0, err
	}
	var keep []*Entry
	for _, e := range entries {
		if s.isStale(e) {
			continue
		}
		keep = append(keep, e)
	}
	var idsList []int64
	for _, e := range keep {
		idsList = append(idsList, e.ID)
	}
	sources, err := s.Sources(ctx, idsList)
	if err != nil {
		return 0, err
	}
	byKind := map[string][]string{}
	for _, e := range keep {
		src := e.Source
		if list := sources[e.ID]; len(list) > 0 {
			src = list[len(list)-1]
		}
		byKind[e.Kind] = append(byKind[e.Kind], AMRLine(e, src))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	var index strings.Builder
	index.WriteString("# Memory\n\n")
	for _, f := range amrFiles {
		fmt.Fprintf(&index, "- [[%s]] %s (%d)\n", f.stem, f.title, len(byKind[f.kind]))
		body := "# " + f.title + "\n\n" + joinLines(byKind[f.kind])
		if err := os.WriteFile(filepath.Join(dir, f.stem+".md"), []byte(body), 0o644); err != nil { //nolint:gosec // G306: exported memory meant to be committed
			return 0, err
		}
	}
	index.WriteString("\n## Preferences\n\n" + joinLines(byKind[KindPreference]))
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(index.String()), 0o644); err != nil { //nolint:gosec // G306: as above
		return 0, err
	}
	return len(keep), nil
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\n") + "\n"
}

// isStale reports an entry whose citations no longer hold.
func (s *Store) isStale(e *Entry) bool {
	if len(e.Evidence) == 0 {
		return false
	}
	var vs []string
	for _, c := range e.Evidence {
		v, _ := Check(s.ScopeRoot, c)
		vs = append(vs, v)
	}
	return Overall(vs) == Stale
}

var (
	amrLink = regexp.MustCompile(`\[\[([^\]]+)\]\]`)
	amrMeta = regexp.MustCompile(`^(.*?)\s*\[([^\[\]]*:[^\[\]]*)\]\s*$`)
)

// ImportResult counts an import.
type ImportResult struct {
	Imported int
	Skipped  int // already held (same kind and content)
}

// Import reads an Agent Memory Repo (a folder with MEMORY.md, or one file and
// the files its [[links]] reach, at most 2 MiB in all) and records every line
// as a candidate: confidence 0.3, source import:amr, no agent or session, the
// line's metadata kept as the revision note and never trusted as the writer.
func (s *Store) Import(ctx context.Context, path string, at int64) (ImportResult, error) {
	var res ImportResult
	info, err := os.Stat(path)
	if err != nil {
		return res, err
	}
	start, dir := path, filepath.Dir(path)
	if info.IsDir() {
		start, dir = filepath.Join(path, "MEMORY.md"), path
	}
	queue := []string{start}
	seen := map[string]bool{}
	total := 0
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		if seen[f] {
			continue
		}
		seen[f] = true
		b, err := os.ReadFile(f) //nolint:gosec // G304: a path the user asked to import
		if err != nil {
			if f == start {
				return res, err
			}
			continue
		}
		if total += len(b); total > amrMaxBytes {
			return res, fmt.Errorf("the memory repo is larger than 2 MiB")
		}
		stem := strings.ToLower(strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)))
		inPrefs := false
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "#") {
				inPrefs = strings.EqualFold(strings.TrimSpace(strings.TrimLeft(trimmed, "#")), "preferences")
				continue
			}
			for _, m := range amrLink.FindAllStringSubmatch(trimmed, -1) {
				target := strings.TrimSpace(m[1])
				if !strings.HasSuffix(target, ".md") {
					target += ".md"
				}
				p := filepath.Join(dir, filepath.Clean("/" + target)[1:]) // stays inside dir
				queue = append(queue, p)
			}
			if !strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "- [[") {
				continue
			}
			text, note := trimmed[2:], ""
			if m := amrMeta.FindStringSubmatch(text); m != nil {
				text, note = m[1], m[2]
			}
			text = strings.TrimSpace(text)
			if text == "" {
				continue
			}
			kind := amrKind(stem, inPrefs)
			w := Write{Kind: kind, Content: text, Source: "import:amr", Confidence: Candidate, At: at,
				Op: OpImport, OnlyNew: true, Note: strings.TrimSuffix("imported from "+f+": "+note, ": ")}
			out, _, err := s.Remember(ctx, w)
			if err != nil {
				return res, err
			}
			if out == "" {
				res.Skipped++
			} else {
				res.Imported++
			}
		}
	}
	return res, nil
}

// amrKind maps a file stem (and the MEMORY.md preferences section) to a kind.
func amrKind(stem string, inPrefs bool) string {
	switch stem {
	case "decisions":
		return KindDecision
	case "architecture":
		return KindArchitecture
	case "failures":
		return KindFailure
	case "preferences", "memory":
		return KindPreference
	}
	if inPrefs {
		return KindPreference
	}
	return KindFact
}
