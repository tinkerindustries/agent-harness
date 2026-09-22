package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/fold"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// TestFoldToAnthropicRequestReplaysRawBlocksVerbatim is the end-to-end check
// for the raw-block replay path: an event log built the way
// internal/session/turn.go actually commits one — a reasoning_delta event
// carrying provider_blocks, tool_call and tool_result events, across two
// sub-turns — folds through internal/fold.Fold into wire.Items, and
// requestFromIntent turns the raw-blocks sub-turn's assistant message back
// into exactly the bytes the API originally sent, untouched by the
// text/tool_use items fold.go also emits for the same sub-turn.
func TestFoldToAnthropicRequestReplaysRawBlocksVerbatim(t *testing.T) {
	sess := store.Session{
		ID:           "sess-1",
		Model:        ModelSonnet5,
		SystemPrompt: "you are a coding agent",
	}

	rawTurn1 := json.RawMessage(`[{"type":"thinking","thinking":"I should list files.","signature":"sig-1"},{"type":"tool_use","id":"call_1","name":"List","input":{"path":"."}}]`)

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
		ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "list the files"}),

		ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "I should list files.", ThoughtSignature: "sig-1", ProviderBlocks: rawTurn1}),
		ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_1", Name: "List", Arguments: `{"path":"."}`}),
		ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_use"}),
		ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_1", Name: "List", Content: "a.go\nb.go"}),

		ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 2}),
		ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "Found a.go and b.go."}),
		ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "end_turn"}),
	}

	items, err := fold.Fold(sess, events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}

	req := requestFromIntent(wire.ChatIntent{Model: sess.Model, Items: items, Effort: wire.EffortHigh})

	// user, assistant (raw turn 1), user tool_result, assistant (turn 2,
	// reconstructed since it carries no raw blocks).
	if len(req.Messages) != 4 {
		t.Fatalf("Messages = %d, want 4: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Role != wire.RoleAssistant {
		t.Fatalf("Messages[1].Role = %q", req.Messages[1].Role)
	}
	if string(req.Messages[1].Content) != string(rawTurn1) {
		t.Errorf("Messages[1].Content = %s,\nwant verbatim %s", req.Messages[1].Content, rawTurn1)
	}

	if req.Messages[3].Role != wire.RoleAssistant {
		t.Fatalf("Messages[3].Role = %q", req.Messages[3].Role)
	}
	var turn2Blocks []ContentBlock
	if err := json.Unmarshal(req.Messages[3].Content, &turn2Blocks); err != nil {
		t.Fatalf("decode turn 2 content: %v", err)
	}
	if len(turn2Blocks) != 1 || turn2Blocks[0].Type != "text" || turn2Blocks[0].Text != "Found a.go and b.go." {
		t.Errorf("turn 2 blocks = %+v", turn2Blocks)
	}
}
