package corpus

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	capstore "github.com/tryatlas/atlas-cli/internal/capture/store"
	memrecord "github.com/tryatlas/atlas-cli/internal/memory/record"
)

// The desktop's caps on durable memories per kind (shared_memory.rs
// durable_entries); preferences are not documents.
var sharedCaps = []struct {
	kind string
	cap  int
}{{"decision", 50}, {"failure", 30}, {"architecture", 30}, {"fact", 50}}

// textCap bounds a captured session's text (agent_memory.rs TEXT_CAP).
const textCap = 12 * 1024

// Collect reads the documents the CLI indexes for the project at root
// (shared memory lives at scope): the project's CLAUDE.md and AGENTS.md,
// .atlas/knowledge notes, durable shared memories, and captured sessions of
// agents the app has no dedicated reader for, as the desktop's
// collect_corpus builds them. It never reads an agent's own folders (Claude
// Code's memory directory, ~/.claude, ~/.codex): the app indexes those into
// the same file, and Owned leaves them alone.
func Collect(ctx context.Context, root, scope string) ([]Doc, error) {
	var docs []Doc
	if t := projectFile(filepath.Join(root, "CLAUDE.md")); t != "" {
		docs = append(docs, Doc{ID: "claude:CLAUDE.md", Title: "CLAUDE.md", Text: t, Source: "claude"})
	}
	if t := projectFile(filepath.Join(root, "AGENTS.md")); t != "" {
		docs = append(docs, Doc{ID: "codex:AGENTS.md", Title: "AGENTS.md", Text: t, Source: "codex"})
	}
	docs = append(docs, knowledge(root)...)
	shared, err := sharedDocs(ctx, scope)
	if err != nil {
		return nil, err
	}
	docs = append(docs, shared...)
	captured, err := captureDocs(ctx, root)
	if err != nil {
		return nil, err
	}
	return append(docs, captured...), nil
}

// Owned reports the documents Collect produces, which a pass may delete when
// they disappear; documents the app reads from agent folders (Claude memory
// files, global CLAUDE.md, Codex threads) are not the CLI's.
func Owned(id string) bool {
	switch {
	case id == "claude:CLAUDE.md" || id == "codex:AGENTS.md":
		return true
	case strings.HasPrefix(id, "shared:") || strings.HasPrefix(id, "kb:"):
		return true
	}
	agent, _, ok := strings.Cut(id, ":")
	return ok && !capturedElsewhere(agent) && agent != "claude" && agent != "codex"
}

// capturedElsewhere: Claude Code and Codex sessions come from their own
// readers in the app, never from capture (agent_memory.rs
// capture_covered_agent).
func capturedElsewhere(agent string) bool {
	return strings.HasPrefix(agent, "claude") || agent == "codex"
}

// projectFile reads a project instruction file without Atlas's mirrored
// block or injected memory context.
func projectFile(path string) string {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the project's own instruction file
	if err != nil {
		return ""
	}
	return StripInjected(removeBlock(string(b)))
}

// The block atlas-instruction-sync mirrors into AGENTS.md.
const (
	blockStart = "<!-- atlas:mirrored-instructions START -->"
	blockEnd   = "<!-- atlas:mirrored-instructions END -->"
)

func removeBlock(s string) string {
	lines := strings.SplitAfter(s, "\n")
	start, end := -1, -1
	for i, l := range lines {
		switch strings.TrimRight(l, "\r\n") {
		case blockStart:
			if start >= 0 {
				return s // not one well-formed pair
			}
			start = i
		case blockEnd:
			if start < 0 || end >= 0 {
				return s
			}
			end = i
		}
	}
	if start < 0 || end < 0 {
		return s
	}
	before := strings.Join(lines[:start], "")
	after := strings.Join(lines[end+1:], "")
	if before == "" {
		after = strings.TrimPrefix(strings.TrimPrefix(after, "\r\n"), "\n")
	} else if after == "" || strings.HasPrefix(after, "\n") || strings.HasPrefix(after, "\r\n") {
		before = strings.TrimSuffix(strings.TrimSuffix(before, "\n"), "\r")
		if !strings.HasSuffix(before, "\n") {
			before += "\n"
		}
	}
	return before + after
}

// Context blocks Atlas injects into prompts, dropped from indexed text
// (atlas-agent-transcript strip_injected_context).
var injectedCores = []string{"SHARED MEMORY", "RELEVANT PROJECT MEMORY", "PROJECT MEMORY", "RECENT SESSION"}

// StripInjected drops <atlas-memory> envelopes and "--- SHARED MEMORY ---" …
// "--- END SHARED MEMORY ---" style blocks, then trims.
func StripInjected(text string) string {
	var out []string
	skipUntil := ""
	inEnvelope := false
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case inEnvelope:
			if l == "</atlas-memory>" {
				inEnvelope = false
			}
			continue
		case skipUntil != "":
			if l == skipUntil {
				skipUntil = ""
			}
			continue
		case l == "<atlas-memory>":
			inEnvelope = true
			continue
		}
		if strings.HasPrefix(l, "--- ") && strings.HasSuffix(l, "---") && !strings.HasPrefix(l, "--- END") {
			inner := strings.TrimLeft(strings.TrimPrefix(l, "--- "), "")
			for _, c := range injectedCores {
				if strings.HasPrefix(inner, c) {
					skipUntil = "--- END " + c + " ---"
					break
				}
			}
			if skipUntil != "" {
				continue
			}
		}
		out = append(out, strings.TrimSuffix(line, "\r"))
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// shortTitle is the first part before a "---" rule, whitespace collapsed,
// at most 80 characters and an ellipsis (agent_memory.rs short_title).
func shortTitle(s string) string {
	cut, _, _ := strings.Cut(s, "\n---\n")
	collapsed := strings.Join(strings.Fields(cut), " ")
	if r := []rune(collapsed); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return collapsed
}

// knowledge is .atlas/knowledge/**/*.md: id kb:<path without .md>, the
// file name as title, source "note".
func knowledge(root string) []Doc {
	base := filepath.Join(root, ".atlas", "knowledge")
	var out []Doc
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error { //nolint:gosec // G703: the project's own .atlas/knowledge
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".md") {
			return nil //nolint:nilerr // an unreadable note is skipped
		}
		b, err := os.ReadFile(p) //nolint:gosec // G304: the project's own notes
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return nil //nolint:nilerr // an unreadable note is skipped
		}
		rel, _ := filepath.Rel(base, p)
		rel = strings.TrimSuffix(filepath.ToSlash(rel), ".md")
		out = append(out, Doc{ID: "kb:" + rel, Title: strings.TrimSuffix(d.Name(), ".md"), Text: string(b), Source: "note"})
		return nil
	})
	return out
}

// sharedDocs are the active durable memories with a log position, newest
// cap per kind, oldest first: id shared:<kind>:<entry id>, text
// "[<agent>] <content>".
func sharedDocs(ctx context.Context, scope string) ([]Doc, error) {
	st, err := memrecord.Open(scope, false)
	if err != nil {
		return nil, nil //nolint:nilerr // no record yet: no shared documents
	}
	defer st.Close()
	var out []Doc
	for _, k := range sharedCaps {
		es, err := st.DurableForCorpus(ctx, k.kind, k.cap)
		if err != nil {
			return nil, err
		}
		for _, e := range es {
			t := strings.TrimSpace(e.Content)
			if t == "" {
				continue
			}
			out = append(out, Doc{ID: fmt.Sprintf("shared:%s:%d", e.Kind, e.ID), Title: shortTitle(t), Text: "[" + e.Agent + "] " + t, Source: "shared"})
		}
	}
	return out, nil
}

// captureDocs are captured sessions of agents without a dedicated reader:
// "role: preview" lines (injection stripped) up to 12 KiB, id
// <agent>:<native session id>, titled by the session's title or first
// prompt.
func captureDocs(ctx context.Context, root string) ([]Doc, error) {
	if _, err := os.Stat(capstore.Path(root)); err != nil { //nolint:gosec // G703: the project's own store
		return nil, nil //nolint:nilerr // capture never enabled here
	}
	db, err := sql.Open("sqlite", "file:"+capstore.Path(root)+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, `SELECT id, agent, native_session_id, COALESCE(title, '') FROM agent_session
	    WHERE workspace_id = ? ORDER BY started_at`, root)
	if err != nil {
		return nil, err
	}
	type sess struct{ id, agent, native, title string }
	var ss []sess
	for rows.Next() {
		var s sess
		var agent, native sql.NullString
		if err := rows.Scan(&s.id, &agent, &native, &s.title); err != nil {
			_ = rows.Close()
			return nil, err
		}
		s.agent, s.native = agent.String, native.String
		if s.agent != "" && !capturedElsewhere(s.agent) {
			ss = append(ss, s)
		}
	}
	_ = rows.Close()
	var out []Doc
	for _, s := range ss {
		mrows, err := db.QueryContext(ctx, `SELECT role, COALESCE(preview, '') FROM agent_message WHERE session_id = ? ORDER BY seq`, s.id)
		if err != nil {
			return nil, err
		}
		var text strings.Builder
		firstUser := ""
		for mrows.Next() {
			var role, preview string
			if err := mrows.Scan(&role, &preview); err != nil {
				_ = mrows.Close()
				return nil, err
			}
			clean := strings.TrimSpace(StripInjected(preview))
			if clean == "" {
				continue
			}
			if firstUser == "" && role == "user" {
				firstUser = clean
			}
			if text.Len() < textCap {
				text.WriteString(role + ": ")
				room := textCap - min(text.Len(), textCap)
				r := []rune(clean)
				text.WriteString(string(r[:min(len(r), room)]))
				text.WriteByte('\n')
			}
		}
		_ = mrows.Close()
		titleSrc := strings.TrimSpace(StripInjected(s.title))
		if titleSrc == "" {
			titleSrc = firstUser
		}
		body := text.String()
		if strings.TrimSpace(titleSrc) == "" && strings.TrimSpace(body) == "" {
			continue
		}
		if strings.TrimSpace(body) == "" {
			body = titleSrc
		}
		out = append(out, Doc{ID: s.agent + ":" + s.native, Title: shortTitle(titleSrc), Text: body, Source: s.agent})
	}
	return out, nil
}
