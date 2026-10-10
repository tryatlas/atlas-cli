package record

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTitle(t *testing.T) {
	long := strings.Repeat("word ", 40)
	cases := []struct{ in, want string }{
		{"Fix the login bug\nmore detail", "Fix the login bug"},
		{"<system-reminder>ignore me</system-reminder>\n\n  # Add a cache layer", "Add a cache layer"},
		{"<command-name>/review</command-name>\n<command-args>PR 12\nsecond line</command-args>", "/review PR 12"},
		{"<command-name>/clear</command-name>", "/clear"},
		{"<bash-input>ls -la</bash-input>", "ls -la"},
		{"<user_query>Why is CI red?</user_query>", "Why is CI red?"},
		{"[Image #1]\n<pasted_content>\nstack trace line one\nline two\n</pasted_content>", "stack trace line one"},
		{"[Pasted text #1 +40 lines]", "Pasted text"},
		{"[Image #1]", "Image"},
		{"> - *   quoted   bullet   ", "quoted bullet"},
		{long, strings.TrimSpace(strings.Repeat("word ", 23)) + "…"},
		{strings.Repeat("x", 200), strings.Repeat("x", 119) + "…"},
	}
	for _, c := range cases {
		if got := Title(c.in); got != c.want {
			t.Errorf("Title(%q)\n got %q\nwant %q", c.in, got, c.want)
		}
	}
	if n := len([]rune(Title(long))); n > 120 {
		t.Fatalf("title %d runes", n)
	}
}

func TestStripMachinery(t *testing.T) {
	if got := StripUser("do it\n\n═══ Atlas next-steps ═══\nappend a block"); got != "do it\n\n" {
		t.Fatalf("%q", got)
	}
	if got := StripAssistant("Done.\n<next_steps>\n- a\n</next_steps>\nbye"); got != "Done.\n\nbye" {
		t.Fatalf("%q", got)
	}
	if got := StripAssistant("open <next_steps> never closed"); got != "open <next_steps> never closed" {
		t.Fatalf("%q", got)
	}
}

func TestCanonicalToolName(t *testing.T) {
	args := func(s string) json.RawMessage { return json.RawMessage(s) }
	cases := []struct {
		wire, title, kind, args, want string
	}{
		{"Read", "", "", `{"file_path":"a"}`, "Read"},
		{"MultiEdit", "", "", `{"file_path":"a","edits":[]}`, "Edit"},
		{"Write", "", "", `{"file_path":"a","content":"x"}`, "Write"},
		{"Bash", "", "", `{"command":"ls"}`, "Bash"},
		{"whatever", "", "", `{"command":"ls"}`, "Bash"},
		{"Grep", "", "", `{"pattern":"x"}`, "Search"},
		{"WebFetch", "", "", `{"url":"x"}`, "Fetch"},
		{"mcp__atlas__memory_search", "", "", `{}`, "Other"},
		{"mcp__fs__read_file", "", "", `{}`, "Other"},
		{"mcp__fs__readfile", "", "", `{}`, "Read"},
		{"fs.write", "", "", `{}`, "Write"},
		{"Task", "", "", `{"prompt":"x"}`, "Task"},
		{"x", "Read src/a.go", "", `{}`, "Read"},
		{"x", "", "edit", `{"old_string":"a"}`, "Edit"},
		{"x", "", "edit", `{"content":"a"}`, "Write"},
		{"x", "", "execute", `{}`, "Bash"},
		{"x", "", "search", `{}`, "Search"},
		{"x", "", "think", `{}`, "Think"},
		{"x", "", "", `{"tool_name":"apply_patch"}`, "Edit"},
		{"unknown", "", "", `{}`, "Other"},
	}
	for _, c := range cases {
		if got := CanonicalToolName(c.wire, c.title, c.kind, args(c.args)); got != c.want {
			t.Errorf("%s/%s/%s %s → %s, want %s", c.wire, c.title, c.kind, c.args, got, c.want)
		}
	}
}

func TestSketch(t *testing.T) {
	// Distinct trimmed non-blank lines, FNV-1a 32, numeric order, unpadded hex.
	got := Sketch([]byte("  b\n\na\n a \n\t\n"))
	want := fnvHex("a") + "," + fnvHex("b")
	if fnv("a") > fnv("b") {
		want = fnvHex("b") + "," + fnvHex("a")
	}
	if got == nil || *got != want {
		t.Fatalf("got %v want %s", got, want)
	}
	if Sketch([]byte("\n  \n")) != nil || Sketch([]byte{0, 1, 2}) != nil || Sketch(nil) != nil {
		t.Fatal("blank and binary must be NULL")
	}
	var b strings.Builder
	for i := 0; i < 600; i++ {
		b.WriteString("line ")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(strings.Repeat("y", i/26))
		b.WriteString("\n")
	}
	if n := len(strings.Split(*Sketch([]byte(b.String())), ",")); n != 512 {
		t.Fatalf("%d hashes, want 512", n)
	}
}

func TestEditPatch(t *testing.T) {
	cases := []struct{ args, want string }{
		{`{"file_path":"a","old_string":"one\ntwo","new_string":"uno"}`, "--- before\n+++ after\n-one\n-two\n+uno"},
		{`{"file_path":"a","content":"# Title\nbody"}`, "--- before\n+++ after\n+# Title\n+body"},
		{`{"patch":"*** Begin Patch\n..."}`, "*** Begin Patch\n..."},
		{`{"file_path":"a"}`, ""},
	}
	for _, c := range cases {
		if got := EditPatch(json.RawMessage(c.args), ""); got != c.want {
			t.Errorf("%s\n got %q\nwant %q", c.args, got, c.want)
		}
	}
}

func TestWritePaths(t *testing.T) {
	got := WritePaths(json.RawMessage(`{"file_path":"/w/a.go","path":"/w/b.go"}`), nil)
	if len(got) != 1 || got[0] != "/w/a.go" {
		t.Fatalf("%v", got)
	}
	got = WritePaths(json.RawMessage(`{"notebook_path":"/w/n.ipynb"}`), nil)
	if len(got) != 1 || got[0] != "/w/n.ipynb" {
		t.Fatalf("%v", got)
	}
	got = WritePaths(json.RawMessage(`{"file_path":"x"}`), []string{"/w/l1", "/w/l2"})
	if len(got) != 2 {
		t.Fatalf("locations win: %v", got)
	}
}

func TestProjectPath(t *testing.T) {
	cases := []struct {
		root, p, want string
		out           bool
	}{
		{"/w", "/w/src/a.go", "src/a.go", false},
		{"/w", "src/../b.go", "b.go", false},
		{"/w", "/etc/hosts", "/etc/hosts", true},
		{"/w", "/w2/a.go", "/w2/a.go", true},
	}
	for _, c := range cases {
		got, out := ProjectPath(c.root, c.p)
		if got != c.want || out != c.out {
			t.Errorf("%s %s → %s %v", c.root, c.p, got, out)
		}
	}
}

func TestEditPatchFromDiffBlocks(t *testing.T) {
	old := "a\nb"
	diffs := []Diff{{Path: "/w/x.go", OldText: &old, NewText: "a\nc"}, {Path: "/w/y.go", NewText: "new"}}
	if got := EditPatch(nil, "/w/x.go", diffs...); got != "--- before\n+++ after\n-a\n-b\n+a\n+c" {
		t.Fatalf("%q", got)
	}
	if got := EditPatch(nil, "/w/y.go", diffs...); got != "--- before\n+++ after\n+new" {
		t.Fatalf("new file: %q", got)
	}
	if got := EditPatch(nil, "", diffs...); got != "" {
		t.Fatal("several blocks and no target: nothing")
	}
	if got := EditPatch(nil, "", diffs[1]); got != "--- before\n+++ after\n+new" {
		t.Fatal("a lone block is used without a target")
	}
	if got := EditPatch(json.RawMessage(`{"old_string":"q","new_string":"r"}`), "/w/x.go", diffs...); got != "--- before\n+++ after\n-q\n+r" {
		t.Fatal("arguments win over diff blocks")
	}
	if got := WritePaths(nil, nil, diffs...); len(got) != 2 || got[1] != "/w/y.go" {
		t.Fatalf("%v", got)
	}
}
