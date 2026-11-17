package tools

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/toolvocab"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

const testClaudeModel = "claude-sonnet-5-5"

func namesOf(array []wire.Tool) []string {
	out := make([]string, len(array))
	for i, t := range array {
		out[i] = t.Function.Name
	}
	return out
}

// TestClaudeArrayIsClaudeCodes pins what a Claude model is offered: Claude
// Code's tools under Claude Code's names, each one a canonical tool the
// executor dispatches, and nothing Claude Code has no counterpart for.
func TestClaudeArrayIsClaudeCodes(t *testing.T) {
	array := DefinitionsFor(testClaudeModel)
	want := []string{"Read", "Write", "Edit", "Bash", "TaskOutput", "TaskStop", "Glob", "Grep",
		"TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "Agent"}
	if got := namesOf(array); !reflect.DeepEqual(got, want) {
		t.Fatalf("Claude's array is %v, want %v", got, want)
	}
	vocab := toolvocab.Of(array)
	for _, tool := range array {
		canonical := vocab.Canonical(tool.Function.Name)
		if canonical == tool.Function.Name {
			t.Errorf("%s has no canonical name, so a call to it could not be dispatched", tool.Function.Name)
		}
		if _, ok := toolFuncs[canonical]; !ok {
			t.Errorf("%s translates to %s, which the executor does not dispatch", tool.Function.Name, canonical)
		}
		if !json.Valid(tool.Function.Parameters) {
			t.Errorf("%s's parameters are not valid JSON", tool.Function.Name)
		}
	}
	for _, absent := range []string{"apply_patch", "list", "job_list", "complete", "web_fetch"} {
		if vocab.Offers(absent) {
			t.Errorf("Claude's array offers %s, which Claude Code has no counterpart for", absent)
		}
	}
}

// TestClaudeArgumentsReachTheCanonicalHandlers pins that every argument a
// Claude Code schema names is one its handler reads once translated: the
// schema's properties, renamed, are a subset of the canonical schema's —
// apart from TaskOutput's timeout, which job_output reads without
// advertising, and TaskCreate's fields, which land inside its tasks array.
func TestClaudeArgumentsReachTheCanonicalHandlers(t *testing.T) {
	canonicalByName := map[string]wire.Tool{}
	for _, tool := range Definitions() {
		canonicalByName[tool.Function.Name] = tool
	}
	propsOf := func(raw json.RawMessage) map[string]json.RawMessage {
		var s struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			t.Fatal(err)
		}
		return s.Properties
	}
	array := DefinitionsFor(testClaudeModel)
	vocab := toolvocab.Of(array)
	for _, tool := range array {
		name := tool.Function.Name
		canonical := vocab.Canonical(name)
		if canonical == "task_create" {
			continue
		}
		canonicalProps := propsOf(canonicalByName[canonical].Function.Parameters)
		for prop := range propsOf(tool.Function.Parameters) {
			_, args := vocab.FromModel(name, `{"`+prop+`":0}`)
			var translated map[string]json.RawMessage
			if err := json.Unmarshal([]byte(args), &translated); err != nil {
				t.Fatal(err)
			}
			for arg := range translated {
				if _, ok := canonicalProps[arg]; ok || (canonical == "job_output" && arg == "timeoutMs") {
					continue
				}
				t.Errorf("%s.%s becomes %s.%s, which %s does not read", name, prop, canonical, arg, canonical)
			}
		}
	}
}

// TestNoBashVariantDropsClaudeCodesShellTools pins that a variant's canonical
// drop list reaches Claude's array through the vocabulary.
func TestNoBashVariantDropsClaudeCodesShellTools(t *testing.T) {
	got := namesOf(DefinitionsForVariant(testClaudeModel, "no-bash"))
	for _, name := range got {
		if name == "Bash" || name == "TaskOutput" || name == "TaskStop" {
			t.Errorf("the no-bash variant left %s in Claude's array: %v", name, got)
		}
	}
	if len(got) != len(DefinitionsFor(testClaudeModel))-3 {
		t.Errorf("the no-bash variant's Claude array is %v, want three fewer tools", got)
	}
}

// TestBackgroundStartedMessageNamesTheArraysTools pins the reply a
// backgrounded command returns: it points the model at the tools it was
// offered.
func TestBackgroundStartedMessageNamesTheArraysTools(t *testing.T) {
	canonical := backgroundStartedMessage(toolvocab.Vocabulary{}, "bash_1")
	if want := "Running in the background with id bash_1. Use job_output with this id to read its output, job_list to see every job, and job_kill to stop it."; canonical != want {
		t.Errorf("canonical reply = %q, want %q", canonical, want)
	}
	claude := backgroundStartedMessage(toolvocab.Of(DefinitionsFor(testClaudeModel)), "bash_1")
	if want := "Running in the background with id bash_1. Use TaskOutput with this id to read its output, and TaskStop to stop it."; claude != want {
		t.Errorf("Claude reply = %q, want %q", claude, want)
	}
}

// TestTaskOutputTimeoutBoundsTheWait pins the one argument job_output reads
// without advertising it: Claude Code's TaskOutput timeout, translated to
// timeoutMs, ends a wait on a job still running and leaves the job alone.
func TestTaskOutputTimeoutBoundsTheWait(t *testing.T) {
	e, _ := newTestExecutor(t)
	t.Cleanup(e.Close)
	vocab := toolvocab.Of(DefinitionsFor(testClaudeModel))
	e.Vocabulary = vocab

	start := e.Execute(context.Background(), claudeCall(vocab, "Bash", `{"command":"sleep 30","run_in_background":true}`))
	if start.Result.IsError || !strings.Contains(start.Result.Content, "TaskOutput") {
		t.Fatalf("background start = %+v", start.Result)
	}
	id := extractShellID(t, start.Result.Content)

	began := time.Now()
	out := e.Execute(context.Background(), claudeCall(vocab, "TaskOutput", `{"task_id":"`+id+`","timeout":200}`))
	if elapsed := time.Since(began); elapsed > 5*time.Second {
		t.Fatalf("TaskOutput waited %s on a 200ms timeout", elapsed)
	}
	if out.Result.IsError || !strings.Contains(out.Result.Content, "Status: running") ||
		!strings.Contains(out.Result.Content, "the job is still running") {
		t.Fatalf("TaskOutput result = %q, want a running job whose wait ran out", out.Result.Content)
	}
	stop := e.Execute(context.Background(), claudeCall(vocab, "TaskStop", `{"task_id":"`+id+`"}`))
	if stop.Result.IsError {
		t.Fatalf("TaskStop = %q", stop.Result.Content)
	}
}

// claudeCall is a call a Claude model made, translated the way the session
// loop translates it before it reaches Execute.
func claudeCall(vocab toolvocab.Vocabulary, name, args string) wire.ToolCall {
	cname, cargs := vocab.FromModel(name, args)
	return wire.ToolCall{ID: "call_" + name, Type: "function", Function: wire.ToolCallFunc{Name: cname, Arguments: cargs}}
}
