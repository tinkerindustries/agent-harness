package store

import (
	"testing"
)

// applyTaskEvent replays TaskCreate/TaskUpdate/TaskDelete in event order,
// with a delete leaving the id burned.
func TestApplyTaskEventReplaysCreateUpdateDeleteInOrder(t *testing.T) {
	nextID := 0
	var todos []StatusTodo

	todos = applyTaskEvent(todos, &nextID, "TaskCreate", `{"tasks":[
		{"subject":"read the task","description":"read it","activeForm":"Reading the task"},
		{"subject":"fix the bug","description":"fix it","activeForm":"Fixing the bug"}]}`)
	if len(todos) != 2 || todos[0].TaskID != "1" || todos[1].TaskID != "2" || todos[0].Status != "pending" {
		t.Fatalf("unexpected create result: %+v", todos)
	}
	if todos[0].Subject != "read the task" || todos[0].Description != "read it" {
		t.Fatalf("expected subject and description carried through replay, got %+v", todos[0])
	}
	if nextID != 2 {
		t.Fatalf("expected nextID 2 after two creates, got %d", nextID)
	}

	// An event the live handler would have rejected leaves the plan untouched
	// and must not burn an id.
	rejected := []struct{ name, args string }{
		{"TaskCreate", `{"tasks":[{"subject":"bad","description":"d","status":"bogus","activeForm":"Badding"}]}`},
		{"TaskCreate", `{"tasks":[{"subject":"","description":"d","activeForm":"Eing"}]}`},
		{"TaskCreate", `{"tasks":[{"subject":"fine","description":"","activeForm":"Eing"}]}`},
		{"TaskCreate", `{"tasks":[]}`},
		{"TaskCreate", `not json`},
		{"TaskUpdate", `not json`},
		{"TaskUpdate", `{"taskId":"2","status":"deleted","subject":"renamed"}`},
		{"TaskUpdate", `{"taskId":"2","status":"deleted","description":"changed"}`},
		{"TaskUpdate", `{"taskId":"2","status":"deleted","activeForm":"Renaming"}`},
		{"TaskUpdate", `{"taskId":"2"}`},
		{"TaskUpdate", `{"taskId":"99","status":"deleted"}`},
		{"TaskUpdate", `{"taskId":"2","status":"bogus"}`},
		{"TaskList", `{}`},
		{"Bash", `{"command":"ls"}`},
	}
	for _, c := range rejected {
		before := len(todos)
		todos = applyTaskEvent(todos, &nextID, c.name, c.args)
		if len(todos) != before {
			t.Fatalf("%s %s must be skipped, got %+v", c.name, c.args, todos)
		}
	}
	if nextID != 2 {
		t.Fatalf("rejected events must not burn ids, nextID = %d", nextID)
	}

	todos = applyTaskEvent(todos, &nextID, "TaskUpdate", `{"taskId":"2","status":"in_progress","subject":"fix the bug","description":"fix it better"}`)
	if len(todos) != 2 || todos[1].Status != "in_progress" || todos[1].Subject != "fix the bug" || todos[1].Description != "fix it better" || todos[1].ActiveForm != "Fixing the bug" {
		t.Fatalf("unexpected patch result: %+v", todos)
	}

	// "deleted" removes the task, preserving the order of the rest and
	// leaving the id counter alone.
	todos = applyTaskEvent(todos, &nextID, "TaskUpdate", `{"taskId":"1","status":"deleted"}`)
	if len(todos) != 1 || todos[0].TaskID != "2" {
		t.Fatalf("unexpected delete result: %+v", todos)
	}

	// A create after a delete mints the next id in sequence.
	todos = applyTaskEvent(todos, &nextID, "TaskCreate", `{"tasks":[{"subject":"push","description":"push it","activeForm":"Pushing"}]}`)
	if len(todos) != 2 || todos[1].TaskID != "3" {
		t.Fatalf("unexpected append after delete: %+v", todos)
	}
	if nextID != 3 {
		t.Fatalf("expected nextID 3 after the third create, got %d", nextID)
	}
}
