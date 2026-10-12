package hooks

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/tryatlas/atlas-cli/internal/capture/record"
	memrecord "github.com/tryatlas/atlas-cli/internal/memory/record"
	"github.com/tryatlas/atlas-cli/internal/memory/server"
)

// planMaxRunes caps a plan as the ACP session does.
const planMaxRunes = 600

// planOf reads a plan out of an agent's plan tool call: Claude Code's and
// Factory Droid's TodoWrite, Codex's update_plan, OpenCode's todowrite,
// Cursor's todo_write, Copilot's update_todo (a list, or a Markdown
// checklist), and Claude Code's ExitPlanMode (the plan as Markdown). The
// text is the ACP session's: "- [status] item" lines, at most 600
// characters; status is done once every item is completed.
func planOf(t record.Tool) (text, status string, ok bool) {
	name := strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(t.Name))
	var args map[string]json.RawMessage
	if json.Unmarshal(t.Arguments, &args) != nil {
		return "", "", false
	}
	switch {
	case name == "exitplanmode":
		var md string
		if json.Unmarshal(args["plan"], &md) != nil || strings.TrimSpace(md) == "" {
			return "", "", false
		}
		return clipPlan(strings.TrimSpace(md)), "active", true
	case strings.Contains(name, "todo") || name == "updateplan" || name == "plan":
	default:
		return "", "", false
	}
	for _, k := range []string{"todos", "plan", "items", "tasks"} {
		raw, has := args[k]
		if !has {
			continue
		}
		var md string
		if json.Unmarshal(raw, &md) == nil {
			if strings.TrimSpace(md) == "" {
				return "", "", false
			}
			return clipPlan(strings.TrimSpace(md)), checklistStatus(md), true
		}
		var items []map[string]any
		if json.Unmarshal(raw, &items) != nil || len(items) == 0 {
			continue
		}
		var lines []string
		all := true
		for _, it := range items {
			content := firstString(it, "content", "step", "text", "title", "description", "task")
			if content == "" {
				continue
			}
			st := itemStatus(it)
			if st != "completed" {
				all = false
			}
			lines = append(lines, fmt.Sprintf("- [%s] %s", st, strings.Join(strings.Fields(content), " ")))
		}
		if len(lines) == 0 {
			return "", "", false
		}
		status := "active"
		if all {
			status = "done"
		}
		return clipPlan(strings.Join(lines, "\n")), status, true
	}
	return "", "", false
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// itemStatus is ACP's vocabulary: pending, in_progress, completed (and
// cancelled, kept as said).
func itemStatus(it map[string]any) string {
	if b, ok := it["completed"].(bool); ok && b {
		return "completed"
	}
	if b, ok := it["done"].(bool); ok && b {
		return "completed"
	}
	s, _ := it["status"].(string)
	if s == "" {
		s, _ = it["state"].(string)
	}
	switch strings.ToLower(strings.ReplaceAll(s, "-", "_")) {
	case "completed", "complete", "done", "finished":
		return "completed"
	case "in_progress", "inprogress", "active", "running", "doing":
		return "in_progress"
	case "cancelled", "canceled":
		return "cancelled"
	}
	return "pending"
}

// checklistStatus is done when a Markdown checklist has items and all are
// ticked.
func checklistStatus(md string) string {
	open, done := 0, 0
	for _, l := range strings.Split(md, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(l, "- [ ]") || strings.HasPrefix(l, "* [ ]"):
			open++
		case strings.HasPrefix(strings.ToLower(l), "- [x]") || strings.HasPrefix(strings.ToLower(l), "* [x]"):
			done++
		}
	}
	if done > 0 && open == 0 {
		return "done"
	}
	return "active"
}

func clipPlan(s string) string {
	if r := []rune(s); len(r) > planMaxRunes {
		return string(r[:planMaxRunes])
	}
	return s
}

// foldPlans records the latest plan among items as the session's plan
// (plan_set: it supersedes the one before, as the app's working memory).
func foldPlans(ctx context.Context, mem *server.Server, g server.Grant, items []record.CapturedItem) {
	var text, status string
	for _, it := range items {
		if it.Tool == nil {
			continue
		}
		if t, s, ok := planOf(*it.Tool); ok {
			text, status = t, s
		}
	}
	if text != "" {
		_ = mem.Fold(ctx, g, memrecord.Event{Kind: "plan_set", Text: text, Status: status})
	}
}

// PlanTask is one task of an incremental task list.
type PlanTask struct {
	ID      string `json:"id"`
	Subject string `json:"subject"`
	Status  string `json:"status"`
}

// PlanFold is a plan to record.
type PlanFold struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

var taskNumber = regexp.MustCompile(`#(\d+)`)

// trackTasks follows Claude Code's task tools: TaskCreate adds a task (its
// number is in the result, "Task #3 created"), TaskUpdate changes one
// (status, subject; "deleted" removes it). It returns the whole list as a
// plan when the call changed it.
func trackTasks(st *State, t record.Tool) *PlanFold {
	name := strings.ToLower(t.Name)
	if name != "taskcreate" && name != "taskupdate" {
		return nil
	}
	if t.NativeID != "" {
		if slices.Contains(st.TaskCalls, t.NativeID) {
			return nil
		}
		st.TaskCalls = append(st.TaskCalls, t.NativeID)
	}
	var args map[string]any
	if json.Unmarshal(t.Arguments, &args) != nil {
		return nil
	}
	switch name {
	case "taskcreate":
		subject := firstString(args, "subject", "title", "content", "description")
		if subject == "" {
			return nil
		}
		id := ""
		if t.Result != nil {
			if m := taskNumber.FindStringSubmatch(*t.Result); m != nil {
				id = m[1]
			}
		}
		if id == "" {
			id = fmt.Sprint(len(st.Tasks) + 1)
		}
		st.Tasks = append(st.Tasks, PlanTask{ID: id, Subject: strings.Join(strings.Fields(subject), " "), Status: "pending"})
	case "taskupdate":
		id := fmt.Sprint(args["taskId"])
		if s, ok := args["taskId"].(string); ok {
			id = s
		} else if f, ok := args["taskId"].(float64); ok {
			id = fmt.Sprint(int64(f))
		}
		i := -1
		for j, task := range st.Tasks {
			if task.ID == id {
				i = j
			}
		}
		if i < 0 {
			return nil
		}
		if s, _ := args["status"].(string); s != "" {
			if strings.EqualFold(s, "deleted") {
				st.Tasks = append(st.Tasks[:i], st.Tasks[i+1:]...)
				break
			}
			st.Tasks[i].Status = itemStatus(map[string]any{"status": s})
		}
		if s := firstString(args, "subject"); s != "" {
			st.Tasks[i].Subject = strings.Join(strings.Fields(s), " ")
		}
	default:
		return nil
	}
	if len(st.Tasks) == 0 {
		return nil
	}
	return planFromTasks(st.Tasks)
}

// hadTaskCalls reports a task tool among items.
func hadTaskCalls(items []record.CapturedItem) bool {
	for _, it := range items {
		if it.Tool != nil {
			if n := strings.ToLower(it.Tool.Name); n == "taskcreate" || n == "taskupdate" {
				return true
			}
		}
	}
	return false
}

// planFromTasks is the task list as plan text.
func planFromTasks(tasks []PlanTask) *PlanFold {
	var lines []string
	all := true
	for _, task := range tasks {
		lines = append(lines, fmt.Sprintf("- [%s] %s", task.Status, task.Subject))
		if task.Status != "completed" {
			all = false
		}
	}
	status := "active"
	if all {
		status = "done"
	}
	return &PlanFold{Text: clipPlan(strings.Join(lines, "\n")), Status: status}
}
