package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Todo is one entry of the model's working plan. The ID is minted by
// TaskCreate in array order and is stable for the task's whole life, so a
// later TaskUpdate can name one task cheaply instead of rewriting the list.
type Todo struct {
	ID         string `json:"id"`
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm"`
}

var validTodoStatus = map[string]bool{"pending": true, "in_progress": true, "completed": true}

// renderTodo renders one checklist line, e.g. "[ ] #3 Fix bug".
func renderTodo(t Todo) string {
	mark := "[ ]"
	if t.Status == "completed" {
		mark = "[x]"
	} else if t.Status == "in_progress" {
		mark = "[~]"
	}
	return mark + " #" + t.ID + " " + t.Content + "\n"
}

// renderChecklist renders the whole list in plan order, one line per task.
func renderChecklist(todos []Todo) string {
	var b strings.Builder
	for _, t := range todos {
		b.WriteString(renderTodo(t))
	}
	return b.String()
}

type taskCreateArgs struct {
	Tasks []taskCreateItem `json:"tasks"`
}

type taskCreateItem struct {
	Content    string `json:"content"`
	ActiveForm string `json:"activeForm"`
	Status     string `json:"status"`
}

// execTaskCreate implements TaskCreate: a state write with no side effects
// outside the session (docs/TOOLS.md). Every item is validated before any is
// committed, so a bad call mutates nothing; ids are minted in array order
// from the executor's nextTaskID.
func execTaskCreate(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args taskCreateArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if len(args.Tasks) == 0 {
		return errorResult("tasks is required")
	}
	for i, t := range args.Tasks {
		if strings.TrimSpace(t.Content) == "" {
			return errorResult("tasks[%d].content is required", i)
		}
		if strings.TrimSpace(t.ActiveForm) == "" {
			return errorResult("tasks[%d].activeForm is required", i)
		}
		if t.Status != "" && !validTodoStatus[t.Status] {
			return errorResult("tasks[%d].status must be pending, in_progress, or completed, got %q", i, t.Status)
		}
	}

	e.todosMu.Lock()
	for _, t := range args.Tasks {
		e.nextTaskID++
		status := t.Status
		if status == "" {
			status = "pending"
		}
		e.todos = append(e.todos, Todo{
			ID:         strconv.Itoa(e.nextTaskID),
			Content:    t.Content,
			Status:     status,
			ActiveForm: t.ActiveForm,
		})
	}
	e.todosMu.Unlock()

	return Result{Content: renderChecklist(e.Todos())}
}

type taskGetArgs struct {
	ID string `json:"id"`
}

// execTaskGet implements TaskGet: fetch one task by id. A linear scan is
// right — plans are small, and the whole point of the id is a cheap targeted
// read (docs/TOOLS.md).
func execTaskGet(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args taskGetArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.ID == "" {
		return errorResult("id is required")
	}
	for _, t := range e.Todos() {
		if t.ID == args.ID {
			return Result{Content: renderTodo(t)}
		}
	}
	return errorResult("no task with id %q", args.ID)
}

type taskListArgs struct {
	Status string `json:"status"`
}

// execTaskList implements TaskList: the rendered checklist of matching tasks,
// or a short message when nothing matches. A fully optional call — no
// arguments at all lists the whole plan.
func execTaskList(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args taskListArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.Status != "" && !validTodoStatus[args.Status] {
		return errorResult("status must be pending, in_progress, or completed, got %q", args.Status)
	}
	var matched []Todo
	for _, t := range e.Todos() {
		if args.Status == "" || t.Status == args.Status {
			matched = append(matched, t)
		}
	}
	if len(matched) == 0 {
		return Result{Content: "No tasks."}
	}
	return Result{Content: renderChecklist(matched)}
}

type taskUpdateArgs struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Content    string `json:"content"`
	ActiveForm string `json:"activeForm"`
	Delete     bool   `json:"delete"`
}

// execTaskUpdate implements TaskUpdate: patch only the fields present on one
// task, or remove it. A delete combined with a patch, an update with nothing
// to set, or an unknown id are all errors and mutate nothing.
func execTaskUpdate(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args taskUpdateArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.ID == "" {
		return errorResult("id is required")
	}
	if args.Delete && (args.Status != "" || args.Content != "" || args.ActiveForm != "") {
		return errorResult("delete cannot be combined with status, content, or activeForm")
	}
	if !args.Delete && args.Status == "" && args.Content == "" && args.ActiveForm == "" {
		return errorResult("nothing to update: set status, content, or activeForm, or delete")
	}
	if args.Status != "" && !validTodoStatus[args.Status] {
		return errorResult("status must be pending, in_progress, or completed, got %q", args.Status)
	}

	e.todosMu.Lock()
	defer e.todosMu.Unlock()
	for i := range e.todos {
		if e.todos[i].ID != args.ID {
			continue
		}
		if args.Delete {
			e.todos = append(e.todos[:i], e.todos[i+1:]...)
			return Result{Content: fmt.Sprintf("Task #%s deleted.", args.ID)}
		}
		if args.Status != "" {
			e.todos[i].Status = args.Status
		}
		if args.Content != "" {
			e.todos[i].Content = args.Content
		}
		if args.ActiveForm != "" {
			e.todos[i].ActiveForm = args.ActiveForm
		}
		return Result{Content: renderTodo(e.todos[i])}
	}
	return errorResult("no task with id %q", args.ID)
}

// Todos returns a copy of the current plan.
func (e *Executor) Todos() []Todo {
	e.todosMu.Lock()
	defer e.todosMu.Unlock()
	out := make([]Todo, len(e.todos))
	copy(out, e.todos)
	return out
}
