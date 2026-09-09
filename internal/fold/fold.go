// Package fold turns a session's event log into the loop's conversation
// items (docs/DESIGN.md §4.1, §3.2). The event log is the one source of
// truth; this is the one consumer that reconstructs the wire shape a request
// carries.
//
// The items are wire.Item, the Responses API's own input-item shape, which
// is what makes the Responses path a passthrough: internal/deepseek
// serialises what this produces without rebuilding it. The Chat Completions
// providers render it back into a messages array with
// wire.MessagesFromItems, and internal/gemini into Interactions steps.
//
// The fold is append-only: an item is only ever pushed onto the output slice
// once every event needed to build it has been seen, and no item, once
// emitted, is later revisited. Folding events[:n] and events[:n+1] for any n
// therefore never disagrees on an item both include — folding more events
// only appends, never rewrites (docs/CACHE.md). The prompt cache depends on
// that property; TestAppendOnly in fold_test.go asserts it.
package fold

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// toolResultItem rebuilds the function_call_output item a tool_result event
// stands for. The ordinary shape is bare text. When the event carries
// ImageURL — a Read that returned an image on a vision provider — the output
// is the parts array the model must see: a text part carrying the label,
// then the input_image part with the data URI exactly as the tool produced
// it. Everything comes from the event payload, never from the filesystem, so
// replaying the log reproduces the identical bytes no matter what happened
// to the file since — the property TestAppendOnly pins (docs/DESIGN.md §4.1).
func toolResultItem(p store.ToolResultPayload) wire.Item {
	return wire.FunctionCallOutputItem(p.ToolCallID, p.Content, p.ImageURL)
}

// Fold reconstructs the item list for sess from events: the frozen system
// prompt, the opening user message, and every completed sub-turn. Events
// past an incomplete sub-turn (deltas seen but no turn_finished yet)
// contribute nothing, which is what keeps the fold append-only.
//
// A sub-turn becomes up to three kinds of item, in the order the model
// produced them: its reasoning, its text, and one item per tool call. A
// messages array said the same thing in one assistant message, and
// wire.MessagesFromItems collapses them back for the providers that want it
// that way.
func Fold(sess store.Session, events []store.Event) ([]wire.Item, error) {
	items := []wire.Item{wire.SystemItem(sess.SystemPrompt)}

	var reasoning, content strings.Builder
	var toolCalls []wire.Item
	// signature is the sub-turn's thought-step receipt (Gemini only), held
	// separately from reasoning: it is always present on a real thought step
	// even when the step carries no summary at all, so it cannot be keyed
	// off reasoning.Len() (docs/GEMINI-INTEGRATION.md §5.2). Assigned, never
	// appended to — a signature is opaque and must survive replay
	// byte-for-byte, not accumulate like prose.
	var signature string
	inTurn := false

	flushAssistant := func() {
		// The reasoning item comes first, ahead of the text and the calls it
		// explains. DeepSeek requires it back on every later request that
		// carries tools and answers 400 without it
		// (third_party/deepseek-docs/guides/thinking_mode.md, "Tool Calls"),
		// and Gemini's thought signature rides on the same item.
		if reasoning.Len() > 0 || signature != "" {
			item := wire.ReasoningItem(reasoning.String())
			item.ThoughtSignature = signature
			items = append(items, item)
		}
		if content.Len() > 0 {
			items = append(items, wire.AssistantItem(content.String()))
		}
		items = append(items, toolCalls...)
		reasoning.Reset()
		content.Reset()
		toolCalls = nil
		signature = ""
		inTurn = false
	}

	for _, e := range events {
		switch e.Kind {
		case store.KindSessionStarted:
			var p store.SessionStartedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: session_started at seq %d: %w", e.Seq, err)
			}
			items = append(items, wire.UserItem(p.OpeningMessage))

		case store.KindTurnStarted:
			inTurn = true

		case store.KindReasoningDelta:
			var p store.ReasoningDeltaPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: reasoning_delta at seq %d: %w", e.Seq, err)
			}
			reasoning.WriteString(p.Text)
			if p.ThoughtSignature != "" {
				signature = p.ThoughtSignature
			}

		case store.KindContentDelta:
			var p store.ContentDeltaPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: content_delta at seq %d: %w", e.Seq, err)
			}
			content.WriteString(p.Text)

		case store.KindToolCall:
			var p store.ToolCallPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: tool_call at seq %d: %w", e.Seq, err)
			}
			toolCalls = append(toolCalls, wire.FunctionCallItem(p.ID, p.Name, p.Arguments))

		case store.KindTurnFinished:
			if inTurn {
				flushAssistant()
			}

		case store.KindToolResult:
			var p store.ToolResultPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: tool_result at seq %d: %w", e.Seq, err)
			}
			items = append(items, toolResultItem(p))

		case store.KindToolDenied:
			var p store.ToolDeniedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: tool_denied at seq %d: %w", e.Seq, err)
			}
			items = append(items, wire.FunctionCallOutputItem(p.ToolCallID, p.Content, ""))

		case store.KindSteerApplied:
			var p store.SteerAppliedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: steer_applied at seq %d: %w", e.Seq, err)
			}
			if p.Role == wire.RoleSystem {
				items = append(items, wire.SystemItem(p.Text))
			} else {
				items = append(items, wire.UserItem(p.Text))
			}

		case store.KindToolStdout, store.KindUsage, store.KindRunFinished, store.KindError, store.KindSteerMessage:
			// Carry no conversation content. Usage and errors are
			// diagnostics; run_finished is a terminal marker read by the
			// runner, not something the model replays. steer_message joins
			// them, unlike steer_applied: only the loop's later steer_applied
			// places the text as a user item, so a steer mid-tool-call cannot
			// move an item the fold had already placed (docs/RUN-CONTROL.md
			// "Two event kinds, not one").
		}
	}

	return items, nil
}
