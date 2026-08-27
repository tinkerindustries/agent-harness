// Package fold turns a session's event log into the DeepSeek messages array
// (docs/DESIGN.md §4.1, §3.2). The event log is the one source of truth;
// this is the one consumer that reconstructs the wire shape the API expects.
//
// The fold is append-only: a message is only ever pushed onto the output
// slice once every event needed to build it has been seen, and no message,
// once emitted, is later revisited. Folding events[:n] and events[:n+1] for
// any n therefore never disagrees on a message both include — folding more
// events only appends, never rewrites (docs/CACHE.md). The prompt cache
// depends on that property; TestAppendOnly in fold_test.go asserts it.
package fold

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// toolResultMessage rebuilds the tool-role message a tool_result event
// stands for. The ordinary shape is a bare text content. When the event
// carries ImageURL — a Read that returned an image on a vision provider —
// the content is the parts array the model must see: a text part carrying
// the label, then the image_url part with the data URI exactly as the tool
// produced it (docs/KIMI-INTEGRATION.md §4.5). Everything comes from the
// event payload, never from the filesystem, so replaying the log reproduces
// the identical bytes no matter what happened to the file since — the
// property TestAppendOnly pins (docs/DESIGN.md §4.1).
func toolResultMessage(p store.ToolResultPayload) wire.Message {
	msg := wire.Message{
		Role:       wire.RoleTool,
		ToolCallID: p.ToolCallID,
	}
	if p.ImageURL == "" {
		msg.Content = wire.TextContent(p.Content)
		return msg
	}
	parts := make([]wire.Part, 0, 2)
	if p.Content != "" {
		parts = append(parts, wire.Part{Type: wire.PartTypeText, Text: p.Content})
	}
	parts = append(parts, wire.Part{Type: wire.PartTypeImageURL, ImageURL: &wire.ImageURL{URL: p.ImageURL}})
	msg.Content = wire.Content{Parts: parts}
	return msg
}

// Fold reconstructs the messages array for sess from events: the frozen
// system prompt, the opening user message, and every completed sub-turn.
// Events past an incomplete sub-turn (deltas seen but no turn_finished yet)
// contribute nothing, which is what keeps the fold append-only.
func Fold(sess store.Session, events []store.Event) ([]wire.Message, error) {
	messages := []wire.Message{wire.SystemMessage(sess.SystemPrompt)}

	var reasoning, content strings.Builder
	var toolCalls []wire.ToolCall
	// signature is the sub-turn's thought-step receipt (Gemini only), held
	// separately from reasoning: it is always present on a real thought
	// step even when the step carries no summary at all, so it cannot be
	// keyed off reasoning.Len() the way ReasoningContent is
	// (docs/GEMINI-INTEGRATION.md §5.2). Assigned, never appended to — a
	// signature is opaque and must survive replay byte-for-byte, not
	// accumulate like prose.
	var signature string
	inTurn := false

	flushAssistant := func() {
		msg := wire.Message{
			Role:      wire.RoleAssistant,
			Content:   wire.TextContent(content.String()),
			ToolCalls: toolCalls,
		}
		if reasoning.Len() > 0 {
			r := reasoning.String()
			msg.ReasoningContent = &r
		}
		if signature != "" {
			s := signature
			msg.ThoughtSignature = &s
		}
		messages = append(messages, msg)
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
			messages = append(messages, wire.UserMessage(p.OpeningMessage))

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
			toolCalls = append(toolCalls, wire.ToolCall{
				ID:   p.ID,
				Type: "function",
				Function: wire.ToolCallFunc{
					Name:      p.Name,
					Arguments: p.Arguments,
				},
			})

		case store.KindTurnFinished:
			if inTurn {
				flushAssistant()
			}

		case store.KindToolResult:
			var p store.ToolResultPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: tool_result at seq %d: %w", e.Seq, err)
			}
			messages = append(messages, toolResultMessage(p))

		case store.KindToolDenied:
			var p store.ToolDeniedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: tool_denied at seq %d: %w", e.Seq, err)
			}
			messages = append(messages, wire.Message{
				Role:       wire.RoleTool,
				Content:    wire.TextContent(p.Content),
				ToolCallID: p.ToolCallID,
			})

		case store.KindSteerApplied:
			var p store.SteerAppliedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: steer_applied at seq %d: %w", e.Seq, err)
			}
			if p.Role == wire.RoleSystem {
				messages = append(messages, wire.SystemMessage(p.Text))
			} else {
				messages = append(messages, wire.UserMessage(p.Text))
			}

		case store.KindToolStdout, store.KindUsage, store.KindRunFinished, store.KindError, store.KindSteerMessage:
			// Carry no messages-array content. Usage and errors are
			// diagnostics; run_finished is a terminal marker read by the
			// runner, not something the model replays. steer_message joins
			// them, unlike steer_applied: only the loop's later steer_applied
			// places the text as a user message, so a steer mid-tool-call
			// cannot move a message the fold had already placed
			// (docs/RUN-CONTROL.md "Two event kinds, not one").
		}
	}

	return messages, nil
}
