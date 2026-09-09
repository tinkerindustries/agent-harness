package store

import (
	"encoding/json"
	"strconv"
	"strings"
)

// StatusTodo is one entry of a run's working plan, recovered by replaying
// TaskCreate/TaskUpdate tool-call events in event order. The field names
// mirror internal/tools.Todo so a todo survives the trip unchanged.
type StatusTodo struct {
	TaskID      string `json:"taskId"`
	Subject     string `json:"subject"`
	Description string `json:"description"`
	Status      string `json:"status"`
	ActiveForm  string `json:"activeForm"`
}

// applyTaskEvent patches current with one TaskCreate or TaskUpdate tool-call
// event in event order, the way the live handlers mutate the executor's plan:
// TaskCreate mints ids from *nextID (increment then use, decimal string) and
// appends; TaskUpdate finds by taskId and patches the fields present or
// removes the task with status "deleted". Events that the live handler would
// have rejected — malformed JSON, an invalid status, an empty subject or
// description, a "deleted" combined with a patch, an update with nothing to
// set, an unknown id — are skipped with current returned unchanged. This
// replay runs over already-committed data and must never fail loudly, so
// nothing here produces an error.
func applyTaskEvent(current []StatusTodo, nextID *int, name, argsJSON string) []StatusTodo {
	switch name {
	case "TaskCreate":
		var args struct {
			Tasks []StatusTodo `json:"tasks"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) != nil || len(args.Tasks) == 0 {
			return current
		}
		for _, t := range args.Tasks {
			if strings.TrimSpace(t.Subject) == "" || strings.TrimSpace(t.Description) == "" || strings.TrimSpace(t.ActiveForm) == "" {
				return current
			}
			if t.Status != "" && !validTaskStatus(t.Status) {
				return current
			}
		}
		out := make([]StatusTodo, len(current), len(current)+len(args.Tasks))
		copy(out, current)
		for _, t := range args.Tasks {
			*nextID++
			status := t.Status
			if status == "" {
				status = "pending"
			}
			out = append(out, StatusTodo{
				TaskID:      strconv.Itoa(*nextID),
				Subject:     t.Subject,
				Description: t.Description,
				Status:      status,
				ActiveForm:  t.ActiveForm,
			})
		}
		return out
	case "TaskUpdate":
		var args struct {
			TaskID      string `json:"taskId"`
			Status      string `json:"status"`
			Subject     string `json:"subject"`
			Description string `json:"description"`
			ActiveForm  string `json:"activeForm"`
		}
		if json.Unmarshal([]byte(argsJSON), &args) != nil || args.TaskID == "" {
			return current
		}
		if args.Status == "deleted" && (args.Subject != "" || args.Description != "" || args.ActiveForm != "") {
			return current
		}
		if args.Status == "" && args.Subject == "" && args.Description == "" && args.ActiveForm == "" {
			return current
		}
		if args.Status != "" && args.Status != "deleted" && !validTaskStatus(args.Status) {
			return current
		}
		for i, t := range current {
			if t.TaskID != args.TaskID {
				continue
			}
			if args.Status == "deleted" {
				out := make([]StatusTodo, 0, len(current)-1)
				out = append(out, current[:i]...)
				out = append(out, current[i+1:]...)
				return out
			}
			out := make([]StatusTodo, len(current))
			copy(out, current)
			if args.Status != "" {
				out[i].Status = args.Status
			}
			if args.Subject != "" {
				out[i].Subject = args.Subject
			}
			if args.Description != "" {
				out[i].Description = args.Description
			}
			if args.ActiveForm != "" {
				out[i].ActiveForm = args.ActiveForm
			}
			return out
		}
		return current
	default:
		return current
	}
}

// validTaskStatus mirrors internal/tools' status enum for the replay above.
func validTaskStatus(status string) bool {
	switch status {
	case "pending", "in_progress", "completed":
		return true
	}
	return false
}
