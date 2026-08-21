package gemini

import (
	"encoding/json"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/fold"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// TestFoldToGeminiRequestCarriesSignatures is the end-to-end check
// docs/GEMINI-INTEGRATION.md §7 "Phase 6" exists for: an event log built the
// way internal/session/turn.go actually commits one — a reasoning_delta
// event carrying a thought signature, tool_call and tool_result events,
// across several sub-turns including a parallel-call turn and a
// signature-with-no-summary turn — folds through internal/fold.Fold into
// wire.Messages, and requestFromIntent turns those into Gemini's Input
// array with every signature present as its own ThoughtStep, in the right
// order, exactly where docs/OBSERVED.md's captured round trip puts it:
// ahead of whatever step(s) it produced. This is the whole round trip the
// phase is about — capture (Phase 4, already covered by chatstream_test.go)
// feeding storage (internal/store) feeding replay (internal/fold) feeding
// the request builder (this package) — so it belongs here rather than in
// internal/fold, which cannot see requestFromIntent, an unexported symbol
// of this package.
func TestFoldToGeminiRequestCarriesSignatures(t *testing.T) {
	sess := store.Session{
		ID:           "sess-1",
		Model:        "gemini-3.7-flash",
		SystemPrompt: "you are a coding agent",
	}

	var seq int64
	ev := func(kind store.EventKind, payload any) store.Event {
		seq++
		p, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return store.Event{SessionID: sess.ID, Seq: seq, Kind: kind, Payload: p}
	}

	events := []store.Event{
		ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "check the weather and list files"}),

		// Sub-turn 1: reasoning plus a signature, one function call.
		ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "I should list the files first.", ThoughtSignature: "sig-1"}),
		ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_00_list", Name: "List", Arguments: `{"path":"."}`}),
		ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_00_list", Name: "List", Content: "a.go\nb.go"}),

		// Sub-turn 2: parallel calls — one thought step carrying the only
		// signature, then two signature-less function calls
		// (docs/GEMINI-INTEGRATION.md §5.2 and §6, "signatures never ride on
		// function calls").
		ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 2}),
		ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "now check the weather in two cities.", ThoughtSignature: "sig-2"}),
		ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_01_a", Name: "get_weather", Arguments: `{"location":"Hobart"}`}),
		ev(store.KindToolCall, store.ToolCallPayload{Index: 1, ID: "call_01_b", Name: "get_weather", Arguments: `{"location":"Perth"}`}),
		ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_calls"}),
		ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_01_a", Name: "get_weather", Content: "18C"}),
		ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_01_b", Name: "get_weather", Content: "22C"}),

		// Sub-turn 3: a signature with no reasoning summary at all — the
		// common Gemini case (docs/GEMINI-INTEGRATION.md §5.2) — ending the
		// turn with plain content and no tool calls.
		ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 3}),
		ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{ThoughtSignature: "sig-3"}),
		ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "Files: a.go, b.go. Hobart 18C, Perth 22C."}),
		ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "stop"}),
	}

	messages, err := fold.Fold(sess, events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}

	req := requestFromIntent(wire.ChatIntent{Model: sess.Model, Messages: messages, Effort: wire.EffortHigh})

	wantTypes := []string{
		StepTypeUserInput,
		StepTypeThought, StepTypeFunctionCall, StepTypeFunctionResult,
		StepTypeThought, StepTypeFunctionCall, StepTypeFunctionCall, StepTypeFunctionResult, StepTypeFunctionResult,
		StepTypeThought, StepTypeModelOutput,
	}
	if len(req.Input) != len(wantTypes) {
		t.Fatalf("input has %d steps, want %d, got %+v", len(req.Input), len(wantTypes), req.Input)
	}

	stepType := func(i int) string {
		switch s := req.Input[i].(type) {
		case UserInputStep:
			return s.Type
		case ModelOutputStep:
			return s.Type
		case ThoughtStep:
			return s.Type
		case FunctionCallStep:
			return s.Type
		case FunctionResultStep:
			return s.Type
		default:
			t.Fatalf("input[%d] is an unrecognised step type %T", i, s)
			return ""
		}
	}
	for i, want := range wantTypes {
		if got := stepType(i); got != want {
			t.Errorf("input[%d].Type = %q, want %q (full input: %+v)", i, got, want, req.Input)
		}
	}

	// The three thought steps, in order, each carrying exactly the
	// signature its sub-turn's reasoning_delta event recorded — never
	// truncated, never concatenated with another sub-turn's.
	wantSignatures := map[int]string{1: "sig-1", 4: "sig-2", 9: "sig-3"}
	for i, want := range wantSignatures {
		step, ok := req.Input[i].(ThoughtStep)
		if !ok {
			t.Fatalf("input[%d] = %T, want ThoughtStep", i, req.Input[i])
		}
		if step.Signature != want {
			t.Errorf("input[%d].Signature = %q, want %q", i, step.Signature, want)
		}
	}

	// The parallel-call turn's two function_call steps carry no signature of
	// their own — the rule that makes the single preceding ThoughtStep the
	// only carrier for the whole turn (docs/GEMINI-INTEGRATION.md §5.2).
	for _, i := range []int{5, 6} {
		call, ok := req.Input[i].(FunctionCallStep)
		if !ok {
			t.Fatalf("input[%d] = %T, want FunctionCallStep", i, req.Input[i])
		}
		_ = call // FunctionCallStep has no Signature field at all: the type
		// itself is the assertion that a function call can never carry one.
	}

	// The final sub-turn's ModelOutputStep still carries the answer text
	// even though its ThoughtStep carried no summary — the signature and the
	// content are independent, exactly as docs/GEMINI-INTEGRATION.md §5.2
	// says they must be treated.
	output, ok := req.Input[10].(ModelOutputStep)
	if !ok {
		t.Fatalf("input[10] = %T, want ModelOutputStep", req.Input[10])
	}
	if len(output.Content) != 1 || output.Content[0].Text != "Files: a.go, b.go. Hobart 18C, Perth 22C." {
		t.Errorf("model_output step = %+v, want the sub-turn 3 answer text", output)
	}
}
