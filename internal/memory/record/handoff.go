package record

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
)

// ── Handoff notes and capture candidates (the desktop's session lifecycle) ──

// HandoffNote is what a finished session leaves for the next one: the
// episodes row's note, camelCase like the desktop's.
type HandoffNote struct {
	Session      string   `json:"session"`
	Agent        string   `json:"agent"`
	StartedAt    *int64   `json:"startedAt"`
	EndedAt      int64    `json:"endedAt"`
	Plan         *string  `json:"plan"`
	OpenItems    []string `json:"openItems"`
	Decisions    []string `json:"decisions"`
	Failures     []string `json:"failures"`
	Facts        []string `json:"facts"`
	Architecture []string `json:"architecture"`
	Files        []string `json:"files"`
}

// handoffCap bounds each list of a note.
const handoffCap = 10

// WriteHandoff builds the session's handoff note from the entries it wrote
// and stores it as its episode (replacing an earlier one). A session that
// wrote nothing leaves no note.
func (s *Store) WriteHandoff(ctx context.Context, session, agent, root string, endedAt int64) error {
	entries, err := s.EntriesBySessions(ctx, []string{session}, 400)
	if err != nil || len(entries) == 0 {
		return err
	}
	n := HandoffNote{Session: session, Agent: agent, EndedAt: endedAt,
		OpenItems: []string{}, Decisions: []string{}, Failures: []string{}, Facts: []string{}, Architecture: []string{}, Files: []string{}}
	var started int64
	if s.db.QueryRowContext(ctx, `SELECT started_at FROM sessions WHERE session_id = ?`, session).Scan(&started) == nil && started > 0 {
		n.StartedAt = &started
	}
	add := func(list *[]string, v string) {
		if v = strings.TrimSpace(v); v != "" && len(*list) < handoffCap {
			*list = append(*list, v)
		}
	}
	for _, e := range entries {
		if e.State != StateActive {
			continue // candidates are not the session's word yet
		}
		switch e.Kind {
		case KindPlan:
			if n.Plan == nil {
				c := e.Content
				n.Plan = &c
			}
		case "decision":
			add(&n.Decisions, e.Content)
		case "failure":
			add(&n.Failures, e.Content)
		case "fact":
			add(&n.Facts, e.Content)
		case "architecture":
			add(&n.Architecture, e.Content)
		case KindFileChanged:
			p := e.Key
			if p != "" && !filepath.IsAbs(p) && root != "" {
				p = filepath.Join(root, filepath.FromSlash(p))
			}
			add(&n.Files, p)
		}
	}
	b, err := json.Marshal(n)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO episodes (session, agent, started_at, ended_at, note) VALUES (?, ?, ?, ?, ?)
	    ON CONFLICT(session) DO UPDATE SET agent = excluded.agent, ended_at = excluded.ended_at, note = excluded.note`,
		session, agent, n.StartedAt, endedAt, string(b))
	return err
}

// markers are the phrases in an agent's own reply that the desktop records
// as candidate memories (memory_delta.rs): never active until confirmed.
var markers = []struct {
	kind string
	re   *regexp.Regexp
}{
	{"decision", regexp.MustCompile(`(?i)\b(decided to\b|decision:|we will use\b|let's use\b|going with\b)`)},
	{"fact", regexp.MustCompile(`(?i)\b(note:|remember:|convention:)`)},
	{"failure", regexp.MustCompile(`(?i)\b(failed:|doesn't work\b|does not work\b|anti-pattern\b|gotcha:)`)},
	{"architecture", regexp.MustCompile(`(?i)\b(architecture:|structured as\b|the system uses\b)`)},
}

// candidateMax bounds one captured line.
const candidateMax = 500

// CaptureCandidates records each line of an agent's reply that carries a
// marker as a candidate of the marker's kind (source capture, confidence
// 0.3). It returns how many were written.
func (s *Store) CaptureCandidates(ctx context.Context, text, agent, session string, at int64) (int, error) {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "-*>#0123456789. "))
		if len(line) < 12 || len(line) > candidateMax {
			continue
		}
		for _, m := range markers {
			if !m.re.MatchString(line) {
				continue
			}
			if _, _, err := s.Remember(ctx, Write{Kind: m.kind, Content: line, Source: "capture", Agent: agent,
				Session: session, Confidence: Candidate, At: at}); err != nil {
				return n, err
			}
			n++
			break
		}
	}
	return n, nil
}
