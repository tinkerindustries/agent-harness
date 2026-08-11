package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Todo is one entry of the model's working plan. The taskId is minted by
// TaskCreate in array order and is stable for the task's whole life, so a
// later TaskUpdate can name one task cheaply instead of rewriting the list.
// The field names match Claude Code's own Task tools: subject is the brief
// actionable title and description the longer explanation (docs/TOOLS.md).
type Todo struct {
	TaskID      string `json:"taskId"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
	ActiveForm  string `json:"activeForm"`
}

// validTodoStatus is the 3-value status domain a task can actually hold.
// "deleted" exists only as a TaskUpdate input trigger and is never a stored
// status, so it deliberately does not appear here.
var validTodoStatus = map[string]bool{"pending": true, "in_progress": true, "completed": true}

// renderTodo renders one checklist line, e.g. "[ ] #3 Fix bug".
func renderTodo(t Todo) string {
	mark := "[ ]"
	if t.Status == "completed" {
		mark = "[x]"
	} else if t.Status == "in_progress" {
		mark = "[~]"
	}
	return mark + " #" + t.TaskID + " " + t.Subject + "\n"
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
	Subject     string `json:"subject"`
	Description string `json:"description"`
	ActiveForm  string `json:"activeForm"`
	Status      string `json:"status"`
}

// execTaskCreate implements TaskCreate: a state write with no side effects
// outside the session (docs/TOOLS.md). Every item is validated before any is
// committed, so a bad call mutates nothing; ids are minted in array order
// from the executor's nextTaskID. subject and description are both required
// on every item, matching Claude Code's real TaskCreate schema.
func execTaskCreate(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args taskCreateArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if len(args.Tasks) == 0 {
		return errorResult("tasks is required")
	}
	for i, t := range args.Tasks {
		if strings.TrimSpace(t.Subject) == "" {
			return errorResult("tasks[%d].subject is required", i)
		}
		if strings.TrimSpace(t.Description) == "" {
			return errorResult("tasks[%d].description is required", i)
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
			TaskID:      strconv.Itoa(e.nextTaskID),
			Subject:     t.Subject,
			Description: t.Description,
			Status:      status,
			ActiveForm:  t.ActiveForm,
		})
	}
	e.todosMu.Unlock()

	return Result{Content: renderChecklist(e.Todos())}
}

type taskGetArgs struct {
	TaskID string `json:"taskId"`
}

// execTaskGet implements TaskGet: fetch one task by taskId. A linear scan is
// right — plans are small, and the whole point of the id is a cheap targeted
// read (docs/TOOLS.md). The single-item result shows the description too,
// which the compact checklist line deliberately leaves out.
func execTaskGet(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args taskGetArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.TaskID == "" {
		return errorResult("taskId is required")
	}
	for _, t := range e.Todos() {
		if t.TaskID == args.TaskID {
			return Result{Content: renderTodo(t) + "Description: " + t.Description + "\n"}
		}
	}
	return errorResult("no task with id %q", args.TaskID)
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
	TaskID      string `json:"taskId"`
	Status      string `json:"status"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	ActiveForm  string `json:"activeForm"`
}

// execTaskUpdate implements TaskUpdate: patch only the fields present on one
// task, or remove it with status "deleted". "deleted" is only ever an input
// trigger — a deleted task is removed, never marked — so a "deleted" combined
// with a patch, an update with nothing to set, or an unknown id are all
// errors and mutate nothing.
func execTaskUpdate(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args taskUpdateArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.TaskID == "" {
		return errorResult("taskId is required")
	}
	if args.Status == "deleted" && (args.Subject != "" || args.Description != "" || args.ActiveForm != "") {
		return errorResult("status \"deleted\" cannot be combined with subject, description, or activeForm")
	}
	if args.Status == "" && args.Subject == "" && args.Description == "" && args.ActiveForm == "" {
		return errorResult("nothing to update: set status, subject, description, or activeForm")
	}
	if args.Status != "" && args.Status != "deleted" && !validTodoStatus[args.Status] {
		return errorResult("status must be pending, in_progress, completed, or deleted, got %q", args.Status)
	}

	e.todosMu.Lock()
	defer e.todosMu.Unlock()
	for i := range e.todos {
		if e.todos[i].TaskID != args.TaskID {
			continue
		}
		if args.Status == "deleted" {
			e.todos = append(e.todos[:i], e.todos[i+1:]...)
			return Result{Content: fmt.Sprintf("Task #%s deleted.", args.TaskID)}
		}
		if args.Status != "" {
			e.todos[i].Status = args.Status
		}
		if args.Subject != "" {
			e.todos[i].Subject = args.Subject
		}
		if args.Description != "" {
			e.todos[i].Description = args.Description
		}
		if args.ActiveForm != "" {
			e.todos[i].ActiveForm = args.ActiveForm
		}
		return Result{Content: renderTodo(e.todos[i])}
	}
	return errorResult("no task with id %q", args.TaskID)
}

// Todos returns a copy of the current plan.
func (e *Executor) Todos() []Todo {
	e.todosMu.Lock()
	defer e.todosMu.Unlock()
	out := make([]Todo, len(e.todos))
	copy(out, e.todos)
	return out
}
