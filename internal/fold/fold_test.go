package fold

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
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

// requireEqualMessages renders the fold's items back into a messages array
// and compares that with what the fold used to produce directly.
//
// It is deliberately not a comparison of items with items. The loop's
// vocabulary moved to the Responses shape; the Chat Completions providers
// still render from it and must still send the bytes they always sent, and
// running sessions have a prompt cache that depends on that
// (internal/wire/messages.go). Every `want` in this file is the array the
// old fold built, left untouched, so each of these tests now pins the whole
// chain: log to items to messages.
func requireEqualMessages(t *testing.T, gotItems []wire.Item, want []wire.Message) {
	t.Helper()
	got := wire.MessagesFromItems(gotItems)
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
	// docs/DESIGN.md §4.4, never as a null. That is a property of the Chat
	// Completions rendering, so it is asserted there: the items themselves
	// carry a tool call as its own function_call item with no content field
	// at all.
	raw, err := json.Marshal(wire.MessagesFromItems(got)[2])
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

// TestFoldThoughtSignature covers Gemini's ordinary case: a thought step's
// signature rides alongside reasoning prose on the reasoning_delta event,
// and the fold must set it as Message.ThoughtSignature — a distinct field
// from ReasoningContent, not concatenated into it
// (docs/GEMINI-INTEGRATION.md §5.2).
func TestFoldThoughtSignature(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "list the files"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "I should use List.", ThoughtSignature: "sig-abc"}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_abc", Name: "List", Arguments: `{"path":"."}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	reasoning := "I should use List."
	sig := "sig-abc"
	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("list the files"),
		{
			Role:             wire.RoleAssistant,
			Content:          wire.TextContent(""),
			ReasoningContent: &reasoning,
			ThoughtSignature: &sig,
			ToolCalls: []wire.ToolCall{
				{ID: "call_00_abc", Type: "function", Function: wire.ToolCallFunc{Name: "List", Arguments: `{"path":"."}`}},
			},
		},
	}
	requireEqualMessages(t, got, want)
}

// TestFoldThoughtSignatureNoReasoning covers the common Gemini case
// docs/GEMINI-INTEGRATION.md §5.2 calls out explicitly: a signature is
// always present on a thought step even when the step carries no summary
// at all, so a sub-turn can have a signature and zero reasoning prose. The
// fold must not key ThoughtSignature off reasoning.Len() the way
// ReasoningContent is keyed, or this event would be dropped and a resumed
// session would replay history missing the thought step.
func TestFoldThoughtSignatureNoReasoning(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "list the files"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		// No Text at all — thinking_summaries: "none", or a non-text thought,
		// per docs/GEMINI-INTEGRATION.md §5.2.
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{ThoughtSignature: "sig-no-summary"}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "done"}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	sig := "sig-no-summary"
	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("list the files"),
		{Role: wire.RoleAssistant, Content: wire.TextContent("done"), ThoughtSignature: &sig},
	}
	requireEqualMessages(t, got, want)

	// ReasoningContent must stay nil on the rendered message, not a pointer
	// to an empty string — the same "absent, not empty" distinction the rest
	// of the fold observes, now made by wire.MessagesFromItems.
	if msgs := wire.MessagesFromItems(got); msgs[2].ReasoningContent != nil {
		t.Errorf("ReasoningContent = %v, want nil when no reasoning_delta event carried text", *msgs[2].ReasoningContent)
	}
}

// TestFoldThoughtSignaturePerSubTurn covers two sub-turns each carrying
// their own signature: the fold must reset the held signature between
// sub-turns (flushAssistant), or turn 2's message would still carry turn
// 1's signature after turn 1's is gone from the log.
func TestFoldThoughtSignaturePerSubTurn(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "add a test"}),

		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "turn one", ThoughtSignature: "sig-1"}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_x", Name: "Read", Arguments: `{"file_path":"a.go"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_x", Name: "Read", Content: "1\tpackage a"}),

		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 2}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "turn two", ThoughtSignature: "sig-2"}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "done"}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	r1, r2 := "turn one", "turn two"
	sig1, sig2 := "sig-1", "sig-2"
	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("add a test"),
		{
			Role: wire.RoleAssistant, Content: wire.TextContent(""),
			ReasoningContent: &r1, ThoughtSignature: &sig1,
			ToolCalls: []wire.ToolCall{
				{ID: "call_00_x", Type: "function", Function: wire.ToolCallFunc{Name: "Read", Arguments: `{"file_path":"a.go"}`}},
			},
		},
		{Role: wire.RoleTool, Content: wire.TextContent("1\tpackage a"), ToolCallID: "call_00_x"},
		{Role: wire.RoleAssistant, Content: wire.TextContent("done"), ReasoningContent: &r2, ThoughtSignature: &sig2},
	}
	requireEqualMessages(t, got, want)
}

// TestFoldDeepSeekUnaffectedByThoughtSignature pins the compatibility
// requirement: a DeepSeek or Kimi session's reasoning_delta events never
// carry ThoughtSignature, and old events stored before the field existed
// decode with it simply absent — both fold to the exact same message this
// package's fold produced before Gemini support existed, with
// ThoughtSignature staying nil.
func TestFoldDeepSeekUnaffectedByThoughtSignature(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "fix the bug"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		// Raw JSON with no thought_signature key at all — the shape every
		// event committed before this phase has on disk.
		{SessionID: "sess-1", Seq: 90, Kind: store.KindReasoningDelta, Payload: json.RawMessage(`{"text":"let me think."}`)},
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "fixed."}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	reasoning := "let me think."
	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("fix the bug"),
		{Role: wire.RoleAssistant, Content: wire.TextContent("fixed."), ReasoningContent: &reasoning},
	}
	requireEqualMessages(t, got, want)
	if msgs := wire.MessagesFromItems(got); msgs[2].ThoughtSignature != nil {
		t.Errorf("ThoughtSignature = %v, want nil for an event with no thought_signature field", *msgs[2].ThoughtSignature)
	}
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

// TestFoldWithdrawnSteer pins that a steer a host withdrew when its run ended
// never reaches the conversation: steer_withdrawn places nothing, and the
// steer_message it closes places nothing either. It fails if the fold starts
// treating a withdrawal as delivery, or errors on the kind.
func TestFoldWithdrawnSteer(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "do it"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "done"}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
		b.ev(store.KindSteerMessage, store.SteerMessagePayload{Text: "be terse", Source: "cli"}),
		b.ev(store.KindRunFinished, store.RunFinishedPayload{Reason: "no_tool_calls", Text: "done"}),
		b.ev(store.KindSteerWithdrawn, store.SteerWithdrawnPayload{SourceSeq: 5}),
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "next"}),
	}
	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}
	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("do it"),
		{Role: wire.RoleAssistant, Content: wire.TextContent("done")},
		wire.UserMessage("next"),
	}
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

// TestFoldImageToolResult covers the parts-in-tool-message shape every
// provider uses: a Read stored the image as ImageURL on the event, and the
// fold must rebuild the parts array the model sees — a text label part then
// the image_url part, exactly as the tool produced them
// (docs/KIMI-INTEGRATION.md §4.5). The bytes come entirely from the event
// payload, so replaying the log reproduces them identically regardless of
// what happened to the image file since.
func TestFoldImageToolResult(t *testing.T) {
	b := &eventBuilder{}
	uri := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "look at the screenshot"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_a", Name: "Read", Arguments: `{"file_path":"shot.png"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_a", Name: "Read", Content: "Image: shot.png", ImageURL: uri}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("look at the screenshot"),
		{
			Role:    wire.RoleAssistant,
			Content: wire.TextContent(""),
			ToolCalls: []wire.ToolCall{
				{ID: "call_00_a", Type: "function", Function: wire.ToolCallFunc{Name: "Read", Arguments: `{"file_path":"shot.png"}`}},
			},
		},
		{
			Role:       wire.RoleTool,
			ToolCallID: "call_00_a",
			Content: wire.Content{Parts: []wire.Part{
				{Type: wire.PartTypeText, Text: "Image: shot.png"},
				{Type: wire.PartTypeImageURL, ImageURL: &wire.ImageURL{URL: uri}},
			}},
		},
	}
	requireEqualMessages(t, got, want)

	// The parts array must serialise as the array form on the wire — the
	// shape a tool message carries an image in
	// (third_party/kimi-docs/openapi.json "Message"). Again a property of
	// the rendering: as an item the same image is an input_image part in the
	// function_call_output's own output.
	raw, err := json.Marshal(wire.MessagesFromItems(got)[3])
	if err != nil {
		t.Fatal(err)
	}
	if !jsonContains(raw, `"content":[`) {
		t.Fatalf("image tool result content is not the parts array: %s", raw)
	}
	if !jsonContains(raw, `"type":"image_url"`) || !jsonContains(raw, `"type":"text"`) {
		t.Fatalf("parts array missing the text or image_url part: %s", raw)
	}
	if !jsonContains(raw, uri) {
		t.Fatalf("parts array does not carry the data URI verbatim: %s", raw)
	}
}

// TestFoldImageToolResultAppendOnly pins the load-bearing property for the
// image shape: folding the log up to the tool_result event and past it must
// not disagree on the image message — the event payload is immutable, so
// the image part is identical on every replay (docs/DESIGN.md §4.1).
func TestFoldImageToolResultAppendOnly(t *testing.T) {
	b := &eventBuilder{}
	uri := "data:image/png;base64,iVBORw0KGgo="
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "look"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_a", Name: "Read", Arguments: `{"file_path":"shot.png"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_a", Name: "Read", Content: "Image: shot.png", ImageURL: uri}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 2}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "the button is misaligned"}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
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

// TestFoldThoughtSignatureAppendOnly pins the same load-bearing property
// TestFoldImageToolResultAppendOnly pins for the image shape, for thought
// signatures: folding the log up to and past the reasoning_delta event that
// carries a signature must never disagree about the assistant message once
// it has been emitted — the signature comes entirely from the event
// payload, so every prefix that includes it agrees with the full fold.
func TestFoldThoughtSignatureAppendOnly(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "check the weather"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		// Signature with no reasoning text — the case the fold must not
		// gate on reasoning.Len().
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{ThoughtSignature: "sig-1"}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_a", Name: "get_weather", Arguments: `{"location":"Hobart"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_a", Name: "get_weather", Content: "18C"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 2}),
		b.ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "it's cold", ThoughtSignature: "sig-2"}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "18C, bring a jacket"}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
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

// TestFoldInterruptedToolCallGetsStandInOutput is the fold half of the
// wedged-session fix. A session stopped while a tool was running leaves a
// tool_call in the log with no tool_result beside it (internal/session's
// TestToolResultsCommitAfterCancel is why that is now rare rather than
// routine, but logs written before it exist). Every provider rejects a
// request carrying a call with no output — DeepSeek answers
// "400 invalid_request_error: No tool output found for tool call <id>" — and
// since the fold is the only thing that builds a request from the log, a
// session in that state could never send anything again.
//
// The fold closes the call off with a stand-in output once the log shows the
// conversation moved past it. It would fail if the orphan produced no output
// item, or if the stand-in landed after the user message that follows it.
func TestFoldInterruptedToolCallGetsStandInOutput(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "run the suite"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_run", Name: "Bash", Arguments: `{"command":"pnpm test"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		// The stop landed here. No tool_result was ever written.
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "resume now"}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("run the suite"),
		{
			Role:    wire.RoleAssistant,
			Content: wire.TextContent(""),
			ToolCalls: []wire.ToolCall{
				{ID: "call_00_run", Type: "function", Function: wire.ToolCallFunc{Name: "Bash", Arguments: `{"command":"pnpm test"}`}},
			},
		},
		{Role: wire.RoleTool, Content: wire.TextContent(interruptedToolOutput), ToolCallID: "call_00_run"},
		wire.UserMessage("resume now"),
	}
	requireEqualMessages(t, got, want)
}

// TestFoldInterruptedToolCallStandInBeforeNextTurn covers the other place
// the conversation can demonstrably move past an orphaned call: the next
// sub-turn starting, with no user message in between. A resume that carries
// no prompt takes this path.
func TestFoldInterruptedToolCallStandInBeforeNextTurn(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "run the suite"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_a", Name: "Bash", Arguments: `{"command":"a"}`}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 1, ID: "call_01_b", Name: "Bash", Arguments: `{"command":"b"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		// One of the two landed before the stop; the other did not.
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_a", Name: "Bash", Content: "a done"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 2}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "carrying on"}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("run the suite"),
		{
			Role:    wire.RoleAssistant,
			Content: wire.TextContent(""),
			ToolCalls: []wire.ToolCall{
				{ID: "call_00_a", Type: "function", Function: wire.ToolCallFunc{Name: "Bash", Arguments: `{"command":"a"}`}},
				{ID: "call_01_b", Type: "function", Function: wire.ToolCallFunc{Name: "Bash", Arguments: `{"command":"b"}`}},
			},
		},
		{Role: wire.RoleTool, Content: wire.TextContent("a done"), ToolCallID: "call_00_a"},
		{Role: wire.RoleTool, Content: wire.TextContent(interruptedToolOutput), ToolCallID: "call_01_b"},
		{Role: wire.RoleAssistant, Content: wire.TextContent("carrying on")},
	}
	requireEqualMessages(t, got, want)
}

// TestFoldOutstandingToolCallGetsNoStandIn is the boundary the stand-in must
// not cross. A tool call whose result has simply not been committed yet —
// the ordinary window between a sub-turn's tool_call batch and its
// tool_result batch — is not orphaned, and emitting a stand-in for it would
// both lie to the model and break the fold's append-only property, because
// the real result would have to replace it. It fails if the fold emits
// anything after the assistant's tool-call message.
func TestFoldOutstandingToolCallGetsNoStandIn(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "run the suite"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_run", Name: "Bash", Arguments: `{"command":"pnpm test"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		b.ev(store.KindUsage, store.UsagePayload{PromptTokens: 100}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("run the suite"),
		{
			Role:    wire.RoleAssistant,
			Content: wire.TextContent(""),
			ToolCalls: []wire.ToolCall{
				{ID: "call_00_run", Type: "function", Function: wire.ToolCallFunc{Name: "Bash", Arguments: `{"command":"pnpm test"}`}},
			},
		},
	}
	requireEqualMessages(t, got, want)
}

// TestFoldUnfinishedSubTurnToolCallGetsNoStandIn covers the case that would
// make the stand-in address a call the model was never shown: a sub-turn
// that streamed tool_call events but never reached turn_finished, so the
// fold drops its calls entirely. The ids must be dropped with them.
func TestFoldUnfinishedSubTurnToolCallGetsNoStandIn(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "run the suite"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_never", Name: "Bash", Arguments: `{"command":"a"}`}),
		// No turn_finished: the request died mid-stream.
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "resume now"}),
	}

	got, err := Fold(testSession(), events)
	if err != nil {
		t.Fatal(err)
	}

	want := []wire.Message{
		wire.SystemMessage("you are a coding agent"),
		wire.UserMessage("run the suite"),
		wire.UserMessage("resume now"),
	}
	requireEqualMessages(t, got, want)
}

// TestInterruptedToolCallAppendOnly holds the stand-in to the property the
// prompt cache rests on (see the package comment): folding any prefix of the
// log must agree with the full fold on every item both include. The stand-in
// is the risky case, because it is an item the fold invents — emitting it
// one event too early would have the real tool_result overwrite it in a
// longer fold, which is exactly the rewrite TestAppendOnly forbids.
func TestInterruptedToolCallAppendOnly(t *testing.T) {
	b := &eventBuilder{}
	events := []store.Event{
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "run two things"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_a", Name: "Bash", Arguments: `{"command":"a"}`}),
		b.ev(store.KindToolCall, store.ToolCallPayload{Index: 1, ID: "call_01_b", Name: "Bash", Arguments: `{"command":"b"}`}),
		b.ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		b.ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_a", Name: "Bash", Content: "a done"}),
		// call_01_b never got one: the stop landed here.
		b.ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "resume now"}),
		b.ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 2}),
		b.ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "all done"}),
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
