package hooks

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	memrecord "github.com/tryatlas/atlas-cli/internal/memory/record"
)

func TestPlanOfEachAgentsPlanTool(t *testing.T) {
	for _, c := range []struct {
		name, tool, args string
		text, status     string
	}{
		{"claude todos", "TodoWrite", `{"todos":[{"content":"Read the code","status":"completed","activeForm":"Reading"},{"content":"Fix the bug","status":"in_progress"},{"content":"Run tests","status":"pending"}]}`,
			"- [completed] Read the code\n- [in_progress] Fix the bug\n- [pending] Run tests", "active"},
		{"codex update_plan", "update_plan", `{"explanation":"x","plan":[{"step":"Write it","status":"completed"},{"step":"Ship it","status":"completed"}]}`,
			"- [completed] Write it\n- [completed] Ship it", "done"},
		{"opencode todowrite", "todowrite", `{"todos":[{"id":"1","content":"Add  greet","status":"in_progress","priority":"high"}]}`,
			"- [in_progress] Add greet", "active"},
		{"cursor todo_write", "todo_write", `{"merge":false,"todos":[{"id":"a","content":"Lint","status":"cancelled"}]}`,
			"- [cancelled] Lint", "active"},
		{"copilot checklist", "update_todo", `{"todos":"- [x] One\n- [x] Two"}`, "- [x] One\n- [x] Two", "done"},
		{"claude plan mode", "ExitPlanMode", `{"plan":"## Plan\n1. Do it"}`, "## Plan\n1. Do it", "active"},
	} {
		text, status, ok := planOf(record.Tool{Name: c.tool, Arguments: []byte(c.args)})
		if !ok || text != c.text || status != c.status {
			t.Errorf("%s: %v %q %q", c.name, ok, text, status)
		}
	}
	if _, _, ok := planOf(record.Tool{Name: "Bash", Arguments: []byte(`{"command":"ls"}`)}); ok {
		t.Fatal("a shell call is not a plan")
	}
	long := `{"todos":[{"content":"` + strings.Repeat("x", 800) + `","status":"pending"}]}`
	if text, _, _ := planOf(record.Tool{Name: "TodoWrite", Arguments: []byte(long)}); len([]rune(text)) != planMaxRunes {
		t.Fatalf("not capped: %d", len([]rune(text)))
	}
}

func TestTrackTasksBuildsThePlan(t *testing.T) {
	st := &State{}
	res := func(s string) *string { return &s }
	call := func(name, args string, result *string) *PlanFold {
		return trackTasks(st, record.Tool{Name: name, Arguments: []byte(args), Result: result})
	}
	call("TaskCreate", `{"subject":"Write farewell()","activeForm":"Writing"}`, res("Task #1 created successfully: Write farewell()"))
	call("TaskCreate", `{"subject":"Add a test"}`, res("Task #2 created successfully: Add a test"))
	p := call("TaskUpdate", `{"taskId":"1","status":"in_progress"}`, res("Updated task #1 status"))
	if p == nil || p.Text != "- [in_progress] Write farewell()\n- [pending] Add a test" || p.Status != "active" {
		t.Fatalf("%+v", p)
	}
	call("TaskUpdate", `{"taskId":"1","status":"completed"}`, nil)
	p = call("TaskUpdate", `{"taskId":2,"status":"completed"}`, nil)
	if p == nil || p.Status != "done" {
		t.Fatalf("%+v", p)
	}
	if p := call("TaskUpdate", `{"taskId":"9","status":"completed"}`, nil); p != nil {
		t.Fatal("an unknown task changes nothing")
	}
	p = call("TaskUpdate", `{"taskId":"2","status":"deleted"}`, nil)
	if p == nil || strings.Contains(p.Text, "Add a test") {
		t.Fatalf("%+v", p)
	}
	if call("Read", `{"file_path":"x"}`, nil) != nil {
		t.Fatal("not a task tool")
	}
}

func TestTaskCallsBecomeThePlanAtTurnEnd(t *testing.T) {
	root := gitRepo(t)
	_ = enabledStore(t, root)
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	_ = os.WriteFile(transcript, nil, 0o600)
	d := &Dispatcher{Root: root, Version: "test", Now: time.Now, Deliver: Deliver("test", nil)}
	sess := map[string]any{"Session": "s-plan", "Transcript": transcript}
	runHook(t, d, "start", sess)
	res := "Task #1 created successfully: Write farewell()"
	// No prompt was delivered: the call is held for the turn's end.
	runHook(t, d, "tool", map[string]any{"Session": "s-plan", "Transcript": transcript,
		"Tool": &record.Tool{NativeID: "tc1", Name: "TaskCreate", Arguments: []byte(`{"subject":"Write farewell()"}`), Result: &res, Status: "completed"}})
	f, _ := os.OpenFile(transcript, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"kind":"text","body":"OK."}` + "\n")
	_ = f.Close()
	runHook(t, d, "stop", sess)
	mem, err := memrecord.Open(root, false)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	es, _ := mem.Active(context.Background(), memrecord.KindPlan)
	if len(es) != 1 || es[0].Content != "- [pending] Write farewell()" {
		t.Fatalf("plan = %+v", es)
	}
}
