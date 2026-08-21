package wire

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
	FinishReason     string
	Usage            *Usage
	Err              error
}
