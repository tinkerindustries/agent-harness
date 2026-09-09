package deepseek

import (
	"encoding/json"
	"fmt"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// decodeResponsesFrame turns one semantic SSE payload into the loop's own
// events. It is the providerhttp.FrameDecoder for this surface, and the
// whole of what makes the Responses stream readable by a loop written
// against chat completion chunks.
//
// It holds no state, which is the property worth keeping. A chat completion
// chunk carries its tool call's index on every fragment; here the call is
// announced once, by an output_item.added naming its call_id and function,
// and its arguments arrive later on separate events. Both carry
// output_index, so the tool-call assembler's own keying by index is enough
// to tie them together (wire.ToolCallAssembler) and nothing has to remember
// which item was opened.
//
// Unknown event types are skipped rather than erroring. The surface defines
// around twenty and this reads six; the rest describe structure the loop
// rebuilds for itself (content_part boundaries, item completion) or
// features it does not use (web search). A type added upstream must not
// break a session mid-run.
func decodeResponsesFrame(data string) ([]wire.Event, bool, error) {
	var ev streamEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return nil, false, fmt.Errorf("deepseek: decode stream event: %w", err)
	}

	switch ev.Type {
	case eventReasoningTextDelta:
		if ev.Delta == "" {
			return nil, false, nil
		}
		return []wire.Event{{Type: wire.EventReasoningDelta, Reasoning: ev.Delta}}, false, nil

	case eventOutputTextDelta:
		if ev.Delta == "" {
			return nil, false, nil
		}
		return []wire.Event{{Type: wire.EventContentDelta, Content: ev.Delta}}, false, nil

	case eventOutputItemAdded:
		// Only a function call opens anything the loop tracks. A reasoning
		// or message item's text arrives as deltas, which need no opening
		// event to be accumulated.
		if ev.Item == nil || ev.Item.Type != itemTypeFunctionCall {
			return nil, false, nil
		}
		return []wire.Event{{Type: wire.EventToolCallDelta, ToolCall: wire.ToolCallDelta{
			Index:    ev.OutputIndex,
			ID:       ev.Item.CallID,
			Type:     "function",
			Function: wire.ToolCallFuncDelta{Name: ev.Item.Name},
		}}}, false, nil

	case eventFunctionCallArgsDelta:
		if ev.Delta == "" {
			return nil, false, nil
		}
		return []wire.Event{{Type: wire.EventToolCallDelta, ToolCall: wire.ToolCallDelta{
			Index:    ev.OutputIndex,
			Function: wire.ToolCallFuncDelta{Arguments: ev.Delta},
		}}}, false, nil

	case eventResponseCompleted, eventResponseIncomplete:
		// The terminal event carries the whole response object, so usage
		// and the finish reason both come off it. Usage first, then finish,
		// matching the order the chat completion pump emits them in — the
		// loop accumulates rather than switching on order, but two streams
		// that differ for no reason are two streams to reason about.
		var out []wire.Event
		if ev.Response != nil && ev.Response.Usage != nil {
			out = append(out, wire.Event{Type: wire.EventUsage, Usage: usageFromResponses(ev.Response.Usage)})
		}
		return append(out, wire.Event{
			Type:         wire.EventFinish,
			FinishReason: finishReasonFor(ev.Response),
		}), true, nil

	case eventResponseFailed:
		// A failure mid-stream is the provider's own message, carried up as
		// the stream's terminal error the same way a transport failure
		// would be. Usage is emitted first when the failed response carried
		// any: tokens spent before a failure are still billed, and a run
		// that dropped them would under-report its own cost.
		var out []wire.Event
		if ev.Response != nil && ev.Response.Usage != nil {
			out = append(out, wire.Event{Type: wire.EventUsage, Usage: usageFromResponses(ev.Response.Usage)})
		}
		return append(out, wire.Event{Type: wire.EventError, Err: responseFailure(ev.Response)}), true, nil

	default:
		return nil, false, nil
	}
}

// responseFailure is the error a response.failed event carries, naming the
// provider's own code and message when it gave one.
func responseFailure(r *responseObject) error {
	if r == nil || r.Error == nil {
		return ErrResponseFailed
	}
	if r.Error.Code == "" {
		return fmt.Errorf("%w: %s", ErrResponseFailed, r.Error.Message)
	}
	return fmt.Errorf("%w: %s (%s)", ErrResponseFailed, r.Error.Message, r.Error.Code)
}
