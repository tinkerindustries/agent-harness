package fold

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

func testSession() store.Session {
	return store.Session{
		ID:           "sess-1",
		Model:        "deepseek-v4-pro",
		Effort:       "high",
		SystemPrompt: "you are a coding agent",
		CreatedAt:    time.Unix(0, 0).UTC(),
	}
}

// eventBuilder assigns sequential seq numbers to make test cases readable.
type eventBuilder struct {
	seq int64
}

func (b *eventBuilder) ev(kind store.EventKind, payload any) store.Event {
	b.seq++
	p, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return store.Event{SessionID: "sess-1", Seq: b.seq, Kind: kind, Payload: p, CreatedAt: time.Unix(0, b.seq).UTC()}
}

func requireEqualMessages(t *testing.T, got, want []wire.Message) {
	t.Helper()
	gj, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal got: %v", err)
	}
	wj, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal want: %v", err)
	}
	if string(gj) != string(wj) {
		t.Fatalf("messages differ:\n got: %s\nwant: %s", gj, wj)
	}
}

// TestFoldPlainTurn covers a turn with no tool calls: reasoning and content
// deltas accumulate into one assistant message with no tool_calls.
func TestFoldPlainTurn(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "fix the bug"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "let me think. "}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "ok, done thinking."}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "The bug is "}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "fixed."}),
		// ElapsedMs is display-only: the fold must ignore it, so a log that
		// carries it folds to the same messages as one that does not.
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop", ElapsedMs: 124200}),
		b.ev(store.KindRunFinished, store.RunFinishedPayload{Reason: "no_tool_calls", Text: "The bug is fixed."}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	reasoning := "let me think. ok, done thinking."
	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("fix the bug"),
		{Role: wire.RoleAssistant, Content: wire.TextContent("The bug is fixed."), ReasoningContent: &reasoning},
	}
	requireEqualMessages(t, got, want)
}

// TestFoldToolCallTurn covers a turn where the assistant calls one tool with
// no content: content must serialise as "" rather than being omitted.
func TestFoldToolCallTurn(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "list the files"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "I should use List."}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_abc", Name: "List", Arguments: `{"path":"."}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_abc", Name: "List", Content: "a.go\nb.go"}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	reasoning := "I should use List."
	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("list the files"),
		{
			Role:             wire.RoleAssistant,
			Content:          wire.TextContent(""),
			ReasoningContent: &reasoning,
			ToolCalls: []wire.ToolCall{
				{ID: "call_00_abc", Type: "function", Function: wire.ToolCallFunc{Name: "List", Arguments: `{"path":"."}`}},
			},
		},
		{Role: wire.RoleTool, Content: wire.TextContent("a.go\nb.go"), ToolCallID: "call_00_abc"},
	}
	requireEqualMessages(t, got, want)

	// The tool-call assistant message must serialise content as "" per
	// docs/DESIGN.md §4.4, never as a null.
	raw, err := json.Marshal(got[2])
	if err != nil {
		t.Fatal(err)
	}
	if !jsonContains(raw, `"content":""`) {
		t.Fatalf("expected literal empty-string content, got %s", raw)
	}
}

func jsonContains(raw []byte, sub string) bool {
	return len(raw) >= len(sub) && indexOf(string(raw), sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// TestFoldParallelToolCallTurn covers two tool calls in one sub-turn. Tool
// results must fold back in tool_calls array order even when this test
// deliberately appends the events in completion order (index 1 finishing
// before index 0), because the runner is responsible for ordering the
// appends, not the fold.
func TestFoldParallelToolCallTurn(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "check the weather in two cities"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_a", Name: "get_weather", Arguments: `{"location":"Hobart"}`}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 1, ID: "call_01_b", Name: "get_weather", Arguments: `{"location":"Perth"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		// Appended in tool_calls order (index 0 then 1) regardless of which
		// finished executing first — that ordering is the runner's job.
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_a", Name: "get_weather", Content: "18C"}),
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_01_b", Name: "get_weather", Content: "24C"}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("check the weather in two cities"),
		{
			Role:    wire.RoleAssistant,
			Content: wire.TextContent(""),
			ToolCalls: []wire.ToolCall{
				{ID: "call_00_a", Type: "function", Function: wire.ToolCallFunc{Name: "get_weather", Arguments: `{"location":"Hobart"}`}},
				{ID: "call_01_b", Type: "function", Function: wire.ToolCallFunc{Name: "get_weather", Arguments: `{"location":"Perth"}`}},
			},
		},
		{Role: wire.RoleTool, Content: wire.TextContent("18C"), ToolCallID: "call_00_a"},
		{Role: wire.RoleTool, Content: wire.TextContent("24C"), ToolCallID: "call_01_b"},
	}
	requireEqualMessages(t, got, want)
}

// TestFoldMultiTurnCarriesReasoning covers a two-sub-turn session: turn 1's
// reasoning_content must still be present, verbatim, once turn 2 is folded
// in (docs/DESIGN.md §3.1).
func TestFoldMultiTurnCarriesReasoning(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "add a test"}),

		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "turn one reasoning"}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_x", Name: "Read", Arguments: `{"file_path":"a.go"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_x", Name: "Read", Content: "1\tpackage a"}),

		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 2}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "turn two reasoning"}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "done"}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
		b.ev(store.KindRunFinished, store.RunFinishedPayload{Reason: "no_tool_calls", Text: "done"}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	turn1Reasoning := "turn one reasoning"
	turn2Reasoning := "turn two reasoning"
	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("add a test"),
		{
			Role:             wire.RoleAssistant,
			Content:          wire.TextContent(""),
			ReasoningContent: &turn1Reasoning,
			ToolCalls: []wire.ToolCall{
				{ID: "call_00_x", Type: "function", Function: wire.ToolCallFunc{Name: "Read", Arguments: `{"file_path":"a.go"}`}},
			},
		},
		{Role: wire.RoleTool, Content: wire.TextContent("1\tpackage a"), ToolCallID: "call_00_x"},
		{Role: wire.RoleAssistant, Content: wire.TextContent("done"), ReasoningContent: &turn2Reasoning},
	}
	requireEqualMessages(t, got, want)
}

// TestFoldSteer pins the two-kind asymmetry in the fold: steer_message
// contributes nothing to the messages array, and only the loop's later
// steer_applied becomes a user message carrying the text verbatim
// (docs/RUN-CONTROL.md "Two event kinds, not one").
func TestFoldSteer(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "do it"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "done"}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
		// A steer accepted after the turn finished but never applied.
		b.ev(store.KindSteerMessage, store.SteerMessagePayload{Text: "be terse", Source: "cli"}),
	}
	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}
	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("do it"),
		{Role: wire.RoleAssistant, Content: wire.TextContent("done")},
	}
	requireEqualMessages(t, got, want)

	// Once the loop applies it at the next boundary, the text becomes a user
	// message verbatim, in seq order after the turn that finished.
	applied := append(events,
		b.ev(store.KindSteerApplied, store.SteerAppliedPayload{SourceSeq: 5, Text: "be terse", SubTurn: 2}))
	got, err = Fold(testSession(), applied)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, wire.UserMessage("be terse"))
	requireEqualMessages(t, got, want)
}

// TestFoldTwoSteersInOneBatch covers the loop's batch append: two steers the
// loop applies at one sub-turn boundary (a single AppendEvents batch, so the
// mirror and hub see one batch) fold to two user messages in seq order — the
// ordering the brief pins for "two steers arrive as two user messages in the
// order they were sent".
func TestFoldTwoSteersInOneBatch(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "do it"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "done"}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
		b.ev(store.KindSteerApplied, store.SteerAppliedPayload{SourceSeq: 6, Text: "first", SubTurn: 2}),
		b.ev(store.KindSteerApplied, store.SteerAppliedPayload{SourceSeq: 7, Text: "second", SubTurn: 2}),
	}
	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}
	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("do it"),
		{Role: wire.RoleAssistant, Content: wire.TextContent("done")},
		wire.UserMessage("first"),
		wire.UserMessage("second"),
	}
	requireEqualMessages(t, got, want)
}

// TestAppendOnly is the load-bearing property: folding events[:n] for every
// n must be a strict prefix, message for message, of folding the full log.
// Breaking this breaks the prompt cache (docs/CACHE.md).
func TestAppendOnly(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "do three things"}),

		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "thinking "}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "more"}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_a", Name: "Read", Arguments: `{"file_path":"a.go"}`}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 1, ID: "call_01_b", Name: "Read", Arguments: `{"file_path":"b.go"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		// The awkward position: a steer sent while the tool calls are
		// outstanding lands between the assistant's tool_call events and its
		// tool_result events. If the fold turned steer_message into a user
		// message here, a longer fold would have to move it below the tool
		// messages — the append-only violation the two-kind split exists to
		// prevent (docs/RUN-CONTROL.md "Two event kinds, not one").
		b.ev(store.KindSteerMessage, store.SteerMessagePayload{Text: "answer in one line", Source: "web"}),
		b.ev(store.KindUsage, store.UsagePayload{PromptTokens: 100}),
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_a", Name: "Read", Content: "1\ta"}),
		b.ev(store.KindToolDenied, store.ToolDeniedPayload{ToolCallID: "call_01_b", Name: "Read", Rule: "readonly", Content: "denied"}),
		// The loop applies the steer at the next sub-turn boundary, where the
		// message array is at rest — a user message whose fold position is
		// final the moment the event exists.
		b.ev(store.KindSteerApplied, store.SteerAppliedPayload{SourceSeq: 8, Text: "answer in one line", SubTurn: 2}),

		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 2}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "all "}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "done"}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
		b.ev(store.KindRunFinished, store.RunFinishedPayload{Reason: "no_tool_calls", Text: "all done"}),
	}

	sess := testSession()
	full, err := Fold(sess, events)
	if err != nil {
		t.Fatal(err)
	}
	fullJSON := make([]string, len(full))
	for i, m := range full {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		fullJSON[i] = string(raw)
	}

	for n := 0; n <= len(events); n++ {
		partial, err := Fold(sess, events[:n])
		if err != nil {
			t.Fatalf("fold events[:%d]: %v", n, err)
		}
		if len(partial) > len(full) {
			t.Fatalf("fold events[:%d] produced %d messages, more than the full fold's %d", n, len(partial), len(full))
		}
		for i, m := range partial {
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if string(raw) != fullJSON[i] {
				t.Fatalf("fold events[:%d] message %d differs from the full fold:\n partial: %s\n   full: %s", n, i, raw, fullJSON[i])
			}
		}
	}
}
