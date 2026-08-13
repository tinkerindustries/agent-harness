package wire

// EventType tags the payload carried by an Event.
type EventType int

const (
	EventReasoningDelta EventType = iota
	EventContentDelta
	EventToolCallDelta
	EventUsage
	EventFinish
	EventError
)

// Event is one item on the channel returned by a provider's streaming
// method. Only the field matching Type is meaningful.
type Event struct {
	Type         EventType
	Reasoning    string
	Content      string
	ToolCall     ToolCallDelta
	FinishReason string
	Usage        *Usage
	Err          error
}
