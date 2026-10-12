package record

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCaptureCandidatesAndHandoff(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := Open(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.SessionStart(ctx, "s1", "claude-acp", 1000)
	reply := "Done.\n- We decided to keep the parser streaming.\nGotcha: the cache key must include the locale.\nDecision: ship the CLI for Intel Macs too.\nnothing to see here at all\n"
	n, err := st.CaptureCandidates(ctx, reply, "claude-acp", "s1", 2000)
	if err != nil || n != 3 {
		t.Fatalf("candidates = %d %v", n, err)
	}
	es, _ := st.List(ctx, "", 50)
	kinds := map[string]string{}
	for _, e := range es {
		kinds[e.Kind] = e.State
		if e.Source != "capture" || e.Agent != "claude-acp" {
			t.Fatalf("entry %+v", e)
		}
	}
	if kinds["decision"] != StateCandidate || kinds["failure"] != StateCandidate {
		t.Fatalf("kinds = %v (candidates must never be active)", kinds)
	}
	if _, err := st.Fold(ctx, Event{Kind: KindFileChanged, Key: "a.go", Text: "Edit", Agent: "claude-acp", Session: "s1", At: 2500}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteHandoff(ctx, "s1", "claude-acp", "/repo", 3000); err != nil {
		t.Fatal(err)
	}
	raw, err := st.LatestHandoff(ctx, "s2")
	if err != nil || raw == nil {
		t.Fatalf("handoff = %s %v", raw, err)
	}
	var note HandoffNote
	if err := json.Unmarshal(raw, &note); err != nil {
		t.Fatal(err)
	}
	if note.Session != "s1" || note.Agent != "claude-acp" || note.StartedAt == nil || *note.StartedAt != 1000 || note.EndedAt != 3000 ||
		len(note.Decisions) != 0 || len(note.Failures) != 0 || len(note.Files) != 1 || note.Files[0] != "/repo/a.go" || note.OpenItems == nil {
		t.Fatalf("note = %+v", note)
	}
	if raw, _ := st.LatestHandoff(ctx, "s1"); raw != nil {
		t.Fatal("a session never sees its own handoff")
	}
}
