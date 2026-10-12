package memview

import (
	"context"
	"regexp"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/tryatlas/cli/internal/memory/record"
)

type fake struct{ searched string }

func (f *fake) Active(_ context.Context, kinds ...string) ([]*record.Entry, error) {
	all := []*record.Entry{
		{ID: 1, Kind: "decision", Content: "Use Postgres for the ledger", State: "active", Agent: "claude-acp", Rev: 2,
			Evidence: []record.Citation{{Path: "db/ledger.go", StartLine: 3, EndLine: 9}}},
		{ID: 2, Kind: "file_changed", Key: "a.go", Content: "Edit a.go", State: "active"},
	}
	if len(kinds) == 0 {
		return all, nil
	}
	var out []*record.Entry
	for _, e := range all {
		if e.Kind == kinds[0] {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fake) Search(_ context.Context, q string, _ []string, _ int) ([]record.Hit, error) {
	f.searched = q
	return []record.Hit{{Entry: &record.Entry{ID: 3, Kind: "fact", Content: "Found " + q, State: "candidate"}}}, nil
}

func (f *fake) History(context.Context, int64) ([]record.Revision, error) {
	return []record.Revision{{Rev: 1, Op: "insert", Agent: "codex-acp", Content: "Use MySQL"}, {Rev: 2, Op: "update", Content: "Use Postgres for the ledger"}}, nil
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;:]*[A-Za-z]`)

func key(m *Model, k string, code rune) {
	_, _ = m.Update(tea.KeyPressMsg{Code: code, Text: k})
}

func TestBrowser(t *testing.T) {
	f := &fake{}
	m := New(context.Background(), f, "/repo")
	_, _ = m.Update(tea.WindowSizeMsg{Width: 140, Height: 30})
	s := ansi.ReplaceAllString(m.View().Content, "")
	for _, want := range []string{"Atlas  memory / all", "/repo", "Entries · 2", "decision     Use Postgres", "db/ledger.go:3-9", "claude-acp", "[TAB] kind"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q\n%s", want, s)
		}
	}
	key(m, "h", 'h')
	if s := ansi.ReplaceAllString(m.View().Content, ""); !strings.Contains(s, "HISTORY") || !strings.Contains(s, "rev 1 · insert · codex-acp") || !strings.Contains(s, "Use MySQL") {
		t.Fatal(s)
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab})
	if len(m.entries) != 1 || m.entries[0].Kind != "file_changed" {
		t.Fatalf("tab filters by kind: %+v", m.entries)
	}
	key(m, "/", '/')
	for _, r := range "ledger" {
		key(m, string(r), r)
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if f.searched != "ledger" || m.entries[0].Content != "Found ledger" {
		t.Fatalf("%q %+v", f.searched, m.entries)
	}
}
