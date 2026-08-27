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

	"github.com/mrgeoffrich/agent-harness/internal/provider"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// toolResultMessage rebuilds the tool-role message a tool_result event
// stands for on Kimi and Gemini, the two providers whose tool message
// accepts a parts array. The ordinary shape is a bare text content. When
// the event carries ImageURL — a Read that returned an image on a vision
// provider — the content is the parts array the model must see: a text
// part carrying the label, then the image_url part with the data URI
// exactly as the tool produced it (docs/KIMI-INTEGRATION.md §4.5).
// DeepSeek's tool message takes a different shape entirely — see
// deepSeekSidecarShape and Fold's own handling of store.KindToolResult —
// because its tool message has no parts variant at all. Everything here
// comes from the event payload, never from the filesystem, so replaying the
// log reproduces the identical bytes no matter what happened to the file
// since — the property TestAppendOnly pins (docs/DESIGN.md §4.1).
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

// deepSeekSidecarShape reports whether model's tool-result images must ride
// in a user message that follows the whole tool-result batch, rather than
// inside the tool message itself. DeepSeek's Chat Completions tool-message
// content has no array variant
// (third_party/deepseek-docs/api/create-chat-completion.md), and its vision
// guide restricts images to user messages
// (third_party/deepseek-docs/guides/vision.md) — so a DeepSeek model takes
// this shape, built inline in Fold's store.KindToolResult case, and every
// other provider keeps the parts-in-tool-message shape toolResultMessage
// builds.
//
// The switch is provider identity, not provider.SeesImages. Kimi and
// Gemini already satisfy SeesImages == true and both want the
// parts-in-tool-message shape, so "sees images" cannot be what selects
// between the two shapes — the day a DeepSeek vision model's own
// SeesImages entry turns true, it would be indistinguishable from them by
// that measure alone. What actually decides the shape is which wire
// dialect a provider's Chat Completions endpoint accepts, a fact about the
// provider, not about whether this particular model happens to look at
// images today. An unrecognised model — which a session's frozen,
// already-validated Model should never be — falls back to false, the shape
// every message the harness has sent since Kimi support landed.
//
// internal/provider imports nothing internal (it is deliberately a leaf,
// so internal/queue and cmd/harness can both reach the model→provider
// table without importing the agent loop), so importing it here adds no
// cycle: internal/fold's other two dependencies, internal/store and
// internal/wire, are leaves in the same sense.
func deepSeekSidecarShape(model string) bool {
	p, err := provider.ModelFor(model)
	return err == nil && p == provider.DeepSeek
}

// sidecarLeadIn is the one leading text part every DeepSeek sidecar user
// message carries, ahead of the per-image parts. Without it, the sidecar
// message is structurally indistinguishable from one a human actually
// sent: wire.RoleUser carrying prose and an image is exactly the shape a
// real steer or continuation takes when someone pastes an image into the
// composer (internal/session/attachments.go materialiseAttachments,
// internal/session/turn.go steerText) — nothing in the message itself
// would tell the model which case it is looking at. The lead-in supplies
// that signal. Its own text stays a plain statement of where the images
// came from; the reason it has to exist at all lives in this comment, not
// in the sentence a model reads.
const sidecarLeadIn = "The harness placed these images here, from the tool results above, because a tool message cannot carry them directly."

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

	// sidecar, sidecarParts, expectedToolResults and receivedToolResults
	// are the DeepSeek image-batching state deepSeekSidecarShape's doc
	// comment explains the need for. sidecar is decided once, from sess,
	// the same way inTurn and the rest of this state track one fold call —
	// Fold stays a pure function of (sess, events), which is what makes two
	// folds of the same prefix agree (docs/DESIGN.md §4.1).
	//
	// expectedToolResults is set from the assistant message's own
	// tool_calls the moment it is flushed, and receivedToolResults counts
	// store.KindToolResult and store.KindToolDenied events since. The
	// sidecar user message is flushed the instant the count is reached,
	// not simply "whenever the next event isn't a tool result" or "at the
	// end of whatever events slice was passed in" — those are the two
	// flush points a first cut at this reads naturally from the shape of
	// the problem, but flushing at end-of-list unconditionally is wrong: a
	// prefix cut between the first and second tool_result of a two-call
	// batch is real input (Fold is called on every prefix, not just
	// complete logs — TestAppendOnly), and flushing there would emit a
	// one-image sidecar message that a longer fold, once the batch's
	// second result lands, has no way to agree with — messages already
	// emitted are never revisited (docs/DESIGN.md §4.1). Counting against
	// the batch's own known size means the sidecar is only ever emitted
	// once every tool result it could still gain has been seen, so a
	// shorter fold either has already emitted the exact same sidecar
	// message a longer fold would, or has emitted none yet — always a
	// strict prefix, never a contradiction. In production this coincides
	// exactly with "the batch is complete", because the runner commits a
	// whole sub-turn's tool results in one store.AppendEvents batch
	// (internal/session/turn.go) — a session's real event log never holds
	// a partially-recorded batch for this to matter for.
	sidecar := deepSeekSidecarShape(sess.Model)
	var sidecarParts []wire.Part
	var expectedToolResults, receivedToolResults int

	flushSidecarImages := func() {
		if len(sidecarParts) == 0 {
			return
		}
		parts := make([]wire.Part, 0, len(sidecarParts)+1)
		parts = append(parts, wire.Part{Type: wire.PartTypeText, Text: sidecarLeadIn})
		parts = append(parts, sidecarParts...)
		messages = append(messages, wire.Message{Role: wire.RoleUser, Content: wire.Content{Parts: parts}})
		sidecarParts = nil
	}

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
		if sidecar {
			// expectedToolResults must be the true size of the batch this
			// assistant message is about to open, never left at its zero
			// value while results for it arrive: a zero count would make
			// the very first store.KindToolResult satisfy
			// receivedToolResults >= expectedToolResults on its own,
			// flushing a one-image sidecar immediately after it — one
			// sidecar per tool result again, reproducing exactly the
			// tool/user/tool interleaving this whole design exists to
			// avoid.
			//
			// This can only be reached correctly, because every
			// fold.Fold call site passes a slice starting at the log's
			// first event (internal/session/turn.go runSubTurn,
			// internal/session/compact.go, and this function's own
			// exhaustive-prefix tests) with one exception,
			// internal/session/resume.go primeDetector's
			// events[:lastTurnIdx] — and that prefix is cut at the index
			// of the log's last store.KindTurnStarted, after every prior
			// sub-turn's tool_calls and their answering tool_results, so
			// it never lands inside a batch either. Given that, seq order
			// guarantees this flushAssistant call — which is what a
			// tool_calls-carrying store.KindTurnFinished always triggers
			// — has already run by the time any store.KindToolResult
			// event answering those tool_calls is reached, so
			// expectedToolResults is always the batch's real size before
			// receivedToolResults starts counting against it. A future
			// caller that folds a slice starting anywhere but the true
			// first event would break this; this comment is where that
			// requirement is written down for them to find.
			expectedToolResults = len(toolCalls)
			receivedToolResults = 0
		}
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
			if sidecar {
				// DeepSeek shape: the tool message carries text only —
				// never a parts array, per deepSeekSidecarShape — and any
				// image rides the sidecar buffer instead, keyed to this
				// tool result by carrying the same label text
				// (internal/tools/read.go readImage: "Image: <path>"),
				// which is what lets the model tell the sidecar images
				// apart when a batch carries more than one.
				messages = append(messages, wire.Message{
					Role:       wire.RoleTool,
					ToolCallID: p.ToolCallID,
					Content:    wire.TextContent(p.Content),
				})
				if p.ImageURL != "" {
					sidecarParts = append(sidecarParts,
						wire.Part{Type: wire.PartTypeText, Text: p.Content},
						wire.Part{Type: wire.PartTypeImageURL, ImageURL: &wire.ImageURL{URL: p.ImageURL}},
					)
				}
				receivedToolResults++
				if receivedToolResults >= expectedToolResults {
					flushSidecarImages()
				}
			} else {
				messages = append(messages, toolResultMessage(p))
			}

		case store.KindToolDenied:
			var p store.ToolDeniedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("fold: tool_denied at seq %d: %w", e.Seq, err)
			}
			// A denial is decided by permission policy before the tool
			// ever runs (docs/TOOLS.md), so store.ToolDeniedPayload has no
			// ImageURL field at all — a denied call can never carry an
			// image. It still resolves one of the batch's outstanding
			// tool_calls, so it counts toward expectedToolResults exactly
			// like a KindToolResult does, or a batch with a denial mixed
			// into it would never satisfy the count and its sidecar (if
			// any of the batch's other calls carried images) would never
			// flush.
			messages = append(messages, wire.Message{
				Role:       wire.RoleTool,
				Content:    wire.TextContent(p.Content),
				ToolCallID: p.ToolCallID,
			})
			if sidecar {
				receivedToolResults++
				if receivedToolResults >= expectedToolResults {
					flushSidecarImages()
				}
			}

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
