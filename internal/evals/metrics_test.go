package evals

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// call builds a tool_call and its tool_result as the store holds them.
func call(seq int64, name, args, result string, isError bool) []store.Event {
	id := name + string(rune('a'+seq))
	callPayload, _ := json.Marshal(store.ToolCallPayload{ID: id, Name: name, Arguments: args})
	resultPayload, _ := json.Marshal(store.ToolResultPayload{ToolCallID: id, Name: name, Content: result, IsError: isError})
	return []store.Event{
		{Seq: seq * 2, Kind: store.KindToolCall, Payload: callPayload},
		{Seq: seq*2 + 1, Kind: store.KindToolResult, Payload: resultPayload},
	}
}

func bash(seq int64, command string) []store.Event {
	args, _ := json.Marshal(map[string]string{"command": command})
	return call(seq, "Bash", string(args), "", false)
}

func events(groups ...[]store.Event) []store.Event {
	var all []store.Event
	for _, g := range groups {
		all = append(all, g...)
	}
	return all
}

func TestSearchViaToolSplitsToolCallsFromShellSearches(t *testing.T) {
	got := Score(events(
		call(1, "Grep", `{"pattern":"foo"}`, "", false),
		call(2, "Glob", `{"pattern":"**/*.go"}`, "", false),
		bash(3, "grep -rn foo ."),
		bash(4, "go test ./..."), // not a search
	))
	if want := 2.0 / 3.0; math.Abs(got["search_via_tool"]-want) > 1e-9 {
		t.Errorf("search_via_tool = %v, want %v", got["search_via_tool"], want)
	}
	if got["searches_total"] != 3 {
		t.Errorf("searches_total = %v, want 3", got["searches_total"])
	}
}

// A path that merely contains a search tool's name is not a search.
func TestSearchViaToolIgnoresACommandThatOnlyMentionsATool(t *testing.T) {
	got := Score(events(
		call(1, "Grep", `{"pattern":"foo"}`, "", false),
		bash(2, "go build ./internal/grep/..."),
		bash(3, "cat src/find/main.go"),
	))
	if got["searches_total"] != 1 {
		t.Errorf("searches_total = %v, want 1", got["searches_total"])
	}
	if got["search_via_tool"] != 1 {
		t.Errorf("search_via_tool = %v, want 1", got["search_via_tool"])
	}
}

// A search after a pipe or a semicolon is still a search.
func TestSearchViaToolFindsASearchLaterInACommand(t *testing.T) {
	got := Score(events(
		bash(1, "go test ./... 2>&1 | grep FAIL"),
		bash(2, "cd web && find . -name '*.tsx'"),
	))
	if got["searches_total"] != 2 {
		t.Errorf("searches_total = %v, want 2", got["searches_total"])
	}
	if got["search_via_tool"] != 0 {
		t.Errorf("search_via_tool = %v, want 0", got["search_via_tool"])
	}
}

// A run that never searched has no share to report. Scoring a zero would
// average in a behaviour that did not happen.
func TestSearchViaToolIsAbsentWhenNothingSearched(t *testing.T) {
	got := Score(events(call(1, "Read", `{"file_path":"a.go"}`, "", false)))
	if _, ok := got["search_via_tool"]; ok {
		t.Errorf("search_via_tool = %v, want absent", got["search_via_tool"])
	}
	if got["tool_calls"] != 1 {
		t.Errorf("tool_calls = %v, want 1", got["tool_calls"])
	}
}

func TestToolErrorRateAndReadBeforeEditMisses(t *testing.T) {
	got := Score(events(
		call(1, "Edit", `{"file_path":"a.go"}`, "a.go has not been read in this session; Read it before editing", true),
		call(2, "Read", `{"file_path":"a.go"}`, "package main", false),
		call(3, "Edit", `{"file_path":"a.go"}`, "edited", false),
		call(4, "Write", `{"file_path":"b.go"}`, "b.go exists and has not been read in this session; Read it before overwriting", true),
	))
	if got["read_before_edit_misses"] != 2 {
		t.Errorf("read_before_edit_misses = %v, want 2", got["read_before_edit_misses"])
	}
	if want := 0.5; got["tool_error_rate"] != want {
		t.Errorf("tool_error_rate = %v, want %v", got["tool_error_rate"], want)
	}
}

// A call whose result never arrived — the run was cut off — still counts as a
// call that happened.
func TestScoreCountsACallWithNoResult(t *testing.T) {
	payload, _ := json.Marshal(store.ToolCallPayload{ID: "x", Name: "Bash", Arguments: `{"command":"grep -rn foo ."}`})
	got := Score([]store.Event{{Seq: 1, Kind: store.KindToolCall, Payload: payload}})
	if got["tool_calls"] != 1 {
		t.Errorf("tool_calls = %v, want 1", got["tool_calls"])
	}
	if got["searches_total"] != 1 {
		t.Errorf("searches_total = %v, want 1", got["searches_total"])
	}
}
