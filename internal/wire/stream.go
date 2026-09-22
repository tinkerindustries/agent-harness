package wire

import "encoding/json"

// EventType tags the payload carried by an Event.
type EventType int

const (
	EventReasoningDelta EventType = iota
	EventContentDelta
	EventToolCallDelta
	// EventThoughtSignatureDelta carries a Gemini thought step's opaque
	// signature, which arrives as the final delta on that step, just before
	// step.stop (docs/GEMINI-INTEGRATION.md §5.2, docs/OBSERVED.md "Gemini
	// 3.7 Flash — Interactions API"). Unlike EventReasoningDelta's payload,
	// a signature is not prose to accumulate: a client emits it as one
	// complete value in Event.ThoughtSignature.
	EventThoughtSignatureDelta
	// EventProviderBlocks carries one response's assistant content array
	// exactly as Anthropic returned it — the replay unit
	// docs/ANTHROPIC-INTEGRATION.md describes. internal/anthropic emits
	// exactly one of these per response, after the last content delta and
	// before EventUsage/EventFinish, so the caller has the complete array in
	// hand before the sub-turn commits. No other provider emits it.
	EventProviderBlocks
	EventUsage
	EventFinish
	EventError
)

// Event is one item on the channel returned by a provider's streaming
// method. Only the field matching Type is meaningful.
type Event struct {
	Type             EventType
	Reasoning        string
	Content          string
	ToolCall         ToolCallDelta
	ThoughtSignature string
	// ProviderBlocks is EventProviderBlocks' payload: one complete raw JSON
	// value, never a fragment to accumulate.
	ProviderBlocks json.RawMessage
	FinishReason   string
	Usage          *Usage
	Err            error
}
