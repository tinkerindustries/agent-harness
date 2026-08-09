package tools

import (
	"context"
	"encoding/json"
	"strings"
)

// Todo is one entry of the model's working plan.
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm"`
}

type todoWriteArgs struct {
	Todos []Todo `json:"todos"`
}

var validTodoStatus = map[string]bool{"pending": true, "in_progress": true, "completed": true}

// execTodoWrite implements TodoWrite: a state write with no side effects
// outside the session (docs/TOOLS.md).
func execTodoWrite(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args todoWriteArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	for i, t := range args.Todos {
		if strings.TrimSpace(t.Content) == "" {
			return errorResult("todos[%d].content is required", i)
		}
		if !validTodoStatus[t.Status] {
			return errorResult("todos[%d].status must be pending, in_progress, or completed, got %q", i, t.Status)
		}
		if strings.TrimSpace(t.ActiveForm) == "" {
			return errorResult("todos[%d].activeForm is required", i)
		}
	}

	e.todosMu.Lock()
	e.todos = args.Todos
	e.todosMu.Unlock()

	if len(args.Todos) == 0 {
		return Result{Content: "Todo list cleared."}
	}
	var b strings.Builder
	for _, t := range args.Todos {
		mark := "[ ]"
		if t.Status == "completed" {
			mark = "[x]"
		} else if t.Status == "in_progress" {
			mark = "[~]"
		}
		b.WriteString(mark + " " + t.Content + "\n")
	}
	return Result{Content: b.String()}
}

// Todos returns a copy of the current plan.
func (e *Executor) Todos() []Todo {
	e.todosMu.Lock()
	defer e.todosMu.Unlock()
	out := make([]Todo, len(e.todos))
	copy(out, e.todos)
	return out
}
