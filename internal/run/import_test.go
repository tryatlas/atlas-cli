package run

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/agents/acp"
	"github.com/tryatlas/atlas-cli/internal/capture/record"
)

func updates(t *testing.T, js ...string) []acp.Update {
	t.Helper()
	var out []acp.Update
	for _, s := range js {
		var u acp.Update
		if err := json.Unmarshal([]byte(s), &u); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

func TestTurnsFromAReplay(t *testing.T) {
	turns := Turns(updates(t,
		`{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Fix "}}`,
		`{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"the bug"}}`,
		`{"sessionUpdate":"agent_thought_chunk","messageId":"t1","content":{"type":"text","text":"Thinking"}}`,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"On "}}`,
		`{"sessionUpdate":"agent_message_chunk","messageId":"m1","content":{"type":"text","text":"it."}}`,
		`{"sessionUpdate":"tool_call","toolCallId":"c1","title":"Edit a.go","kind":"edit","status":"pending","rawInput":{"file_path":"a.go"}}`,
		`{"sessionUpdate":"tool_call_update","toolCallId":"c1","status":"completed"}`,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Done."}}`,
		`{"sessionUpdate":"user_message_chunk","content":{"type":"text","text":"Thanks"}}`,
		`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Welcome"}}`,
	))
	if len(turns) != 2 || turns[0].Prompt != "Fix the bug" || turns[1].Prompt != "Thanks" {
		t.Fatalf("%+v", turns)
	}
	it := turns[0].Items
	if len(it) != 4 || it[0].Mode != "thinking" || it[1].Body != "On it." || it[1].NativeID != "m1" ||
		it[2].Tool == nil || it[2].Tool.Status != "completed" || it[2].Tool.Title != "Edit a.go" || it[3].Body != "Done." {
		t.Fatalf("%+v", it)
	}
}

func TestImportTurnsAppendsOnly(t *testing.T) {
	root := testProject(t)
	h, err := StartHost(root, HostOptions{Version: "test", Ingest: "http://127.0.0.1:1", Tokens: noTokens{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = h.Close() }()
	ctx := context.Background()
	s := record.Session{Agent: "some-agent", NativeID: "native-1", Cwd: root}
	t1 := record.ImportedTurn{Prompt: "first", Items: []record.ImportedItem{{NativeID: "a1", Mode: "text", Body: "one"}}}
	t2 := record.ImportedTurn{Prompt: "second", Items: []record.ImportedItem{{NativeID: "a2", Mode: "text", Body: "two"},
		{NativeID: "c1", Tool: &record.Tool{NativeID: "c1", Title: "Run ls", Kind: "execute", Status: "pending"}}}}
	if n, err := h.Recorder.ImportTurns(ctx, s, []record.ImportedTurn{t1}, time.Now()); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	// The next replay has both turns: only the second is new.
	if n, err := h.Recorder.ImportTurns(ctx, s, []record.ImportedTurn{t1, t2}, time.Now()); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if n, _ := h.Recorder.ImportTurns(ctx, s, []record.ImportedTurn{t1, t2}, time.Now()); n != 0 {
		t.Fatal("a repeat replay writes nothing")
	}
	db := h.Store.DB()
	if got := one(t, db, `SELECT source || '|' || title || '|' || (SELECT group_concat(state) FROM turn) FROM agent_session`); got != "acp|first|completed,completed" {
		t.Fatal(got)
	}
	if got := one(t, db, `SELECT group_concat(body, ',') FROM (SELECT body FROM agent_message ORDER BY seq)`); got != "first,one,second,two" {
		t.Fatal(got)
	}
	if got := one(t, db, `SELECT tool_name || '|' || status FROM tool_call`); got != "Bash|completed" {
		t.Fatalf("a replayed call already ran: %s", got)
	}
	if got := one(t, db, `SELECT count(*) FROM file_touch`); got != "0" {
		t.Fatal("a replay samples no writes")
	}
}

func TestNewer(t *testing.T) {
	if newer("2026-10-11T11:43:39.576Z", "2026-10-11T11:43:39.763337Z") || !newer("2026-10-11T11:45:00Z", "2026-10-11T11:43:39.763337Z") ||
		!newer("", "2026-10-11T11:43:39Z") {
		t.Fatal("newer")
	}
}
