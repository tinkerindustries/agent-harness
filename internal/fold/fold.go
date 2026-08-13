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

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// Fold reconstructs the messages array for sess from events: the frozen
// system prompt, the opening user message, and every completed sub-turn.
// Events past an incomplete sub-turn (deltas seen but no turn_finished yet)
// contribute nothing, which is what keeps the fold append-only.
func Fold(sess store.Session, events []store.Event) ([]deepseek.Message, error) {
	messages := []deepseek.Message{deepseek.SystemMessage(sess.SystemPrompt)}

	var reasoning, content strings.Builder
	var toolCalls []deepseek.ToolCall
	inTurn := false

	flushAssistant := func() {
		msg := deepseek.Message{
			Role:      deepseek.RoleAssistant,
			Content:   content.String(),
			ToolCalls: toolCalls,
		}
		if reasoning.Len() > 0 {
			r := reasoning.String()
			msg.ReasoningContent = &r
		}
		messages = append(messages, msg)
		reasoning.Reset()
		content.Reset()
		toolCalls = nil
		inTurn = false
	}

	for _, e := range events {
		switch e.Kind {
		case store.KindSessionStarted:
			var p store.SessionStartedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: session_started at seq %d: %w", e.Seq, err)
			}
			messages = append(messages, deepseek.UserMessage(p.OpeningMessage))

		case store.KindTurnStarted:
			inTurn = true

		case store.KindReasoningDelta:
			var p store.ReasoningDeltaPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: reasoning_delta at seq %d: %w", e.Seq, err)
			}
			reasoning.WriteString(p.Text)

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
			toolCalls = append(toolCalls, deepseek.ToolCall{
				ID:   p.ID,
				Type: "function",
				Function: deepseek.ToolCallFunc{
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
			messages = append(messages, deepseek.Message{
				Role:       deepseek.RoleTool,
				Content:    p.Content,
				ToolCallID: p.ToolCallID,
			})

		case store.KindToolDenied:
			var p store.ToolDeniedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: tool_denied at seq %d: %w", e.Seq, err)
			}
			messages = append(messages, deepseek.Message{
				Role:       deepseek.RoleTool,
				Content:    p.Content,
				ToolCallID: p.ToolCallID,
			})

		case store.KindSteerApplied:
			var p store.SteerAppliedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: steer_applied at seq %d: %w", e.Seq, err)
			}
			if p.Role == deepseek.RoleSystem {
				messages = append(messages, deepseek.SystemMessage(p.Text))
			} else {
				messages = append(messages, deepseek.UserMessage(p.Text))
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
