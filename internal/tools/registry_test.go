package tools

import (
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// callWithArguments sends arguments verbatim, without marshalling them, so a
// call the model emitted with no arguments field at all can be reproduced:
// that arrives as the empty string.
func callWithArguments(t *testing.T, e *Executor, name, arguments string) Outcome {
	t.Helper()
	outcome := e.Execute(t.Context(), wire.ToolCall{
		ID:   "call_test",
		Type: "function",
		Function: wire.ToolCallFunc{
			Name:      name,
			Arguments: arguments,
		},
	})
	if outcome.Denied {
		t.Fatalf("unexpected denial: %s", outcome.Rule)
	}
	return outcome
}

// TestExecuteAbsentArgumentsRunAnAllOptionalTool proves a call whose
// arguments field never arrived is the call with an empty object: TaskList's
// fields are all optional, so it runs and lists the whole plan. Before the
// normalisation the same call came back "invalid arguments: unexpected end of
// JSON input", which sent the model looking for a filter it did not want.
func TestExecuteAbsentArgumentsRunAnAllOptionalTool(t *testing.T) {
	e, _ := newTestExecutor(t)
	created := callWithArguments(t, e, "TaskCreate",
		`{"tasks":[{"subject":"Fix the bug","description":"why","activeForm":"Fixing the bug"}]}`)
	if created.Result.IsError {
		t.Fatalf("TaskCreate failed: %s", created.Result.Content)
	}

	for _, arguments := range []string{"", " ", "\n\t "} {
		got := callWithArguments(t, e, "TaskList", arguments)
		if got.Result.IsError {
			t.Fatalf("TaskList with arguments %q failed: %s", arguments, got.Result.Content)
		}
		if !strings.Contains(got.Result.Content, "Fix the bug") {
			t.Fatalf("TaskList with arguments %q returned %q, want the whole plan", arguments, got.Result.Content)
		}
	}
}

// TestExecuteAbsentArgumentsRefuseByFieldName proves the same call against a
// tool with a required field is refused in terms the model can act on — the
// missing field's name — rather than by a JSON parse error naming a byte
// position it cannot see. Complete is in the table because Execute dispatches
// it beside toolFuncs rather than through it.
func TestExecuteAbsentArgumentsRefuseByFieldName(t *testing.T) {
	e, _ := newTestExecutor(t)
	for _, tc := range []struct{ name, want string }{
		{"Grep", "pattern is required"},
		{"Complete", "summary is required"},
	} {
		got := callWithArguments(t, e, tc.name, "")
		if !got.Result.IsError {
			t.Fatalf("%s with no arguments succeeded, want a refusal: %s", tc.name, got.Result.Content)
		}
		if !strings.Contains(got.Result.Content, tc.want) {
			t.Fatalf("%s with no arguments said %q, want %q", tc.name, got.Result.Content, tc.want)
		}
	}
}
