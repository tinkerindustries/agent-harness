package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

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

// Event is one item on the channel returned by StreamChatCompletion. Only
// the field matching Type is meaningful.
type Event struct {
	Type         EventType
	Reasoning    string
	Content      string
	ToolCall     ToolCallDelta
	FinishReason string
	Usage        *Usage
	Err          error
}

// ErrIdleTimeout is sent when no SSE frame, including a keep-alive comment,
// arrives within the idle window. It is not a request timeout: DeepSeek can
// hold a request up to ten minutes before inference starts, and streaming
// keep-alives are expected during that wait.
var ErrIdleTimeout = errors.New("deepseek: stream idle timeout")

// StreamChatCompletion sends req with streaming enabled and returns a
// channel of typed deltas. The channel closes when the stream ends,
// normally or by error; a terminal Event with Type EventError is always
// the last event sent before it closes.
func (c *Client) StreamChatCompletion(ctx context.Context, req ChatCompletionRequest) (<-chan Event, error) {
	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("deepseek: encode request: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, wrapClientError("stream request", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}

	events := make(chan Event)
	go c.pumpStream(ctx, resp.Body, events)
	return events, nil
}

func (c *Client) pumpStream(ctx context.Context, body io.ReadCloser, events chan<- Event) {
	defer close(events)
	defer body.Close()

	// Every send is guarded by ctx so a caller that cancels and stops reading
	// cannot strand this goroutine and the response body on an unbuffered
	// send. send reports whether the value was delivered.
	send := func(e Event) bool {
		select {
		case events <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}

	lines := make(chan string)
	lineErrs := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := newSSEScanner(body)
		for {
			line, err := scanner.Scan()
			if err != nil {
				if err != io.EOF {
					lineErrs <- err
				}
				return
			}
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()

	idle := time.NewTimer(c.idleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			send(Event{Type: EventError, Err: ctx.Err()})
			return

		case err := <-lineErrs:
			send(Event{Type: EventError, Err: fmt.Errorf("deepseek: stream read: %w", err)})
			return

		case <-idle.C:
			send(Event{Type: EventError, Err: ErrIdleTimeout})
			return

		case line, ok := <-lines:
			if !ok {
				return
			}
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(c.idleTimeout)

			f := classifySSELine(line)
			switch f.kind {
			case frameComment, frameBlank, frameOther:
				continue
			case frameData:
				if f.data == "[DONE]" {
					return
				}
				var chunk ChatCompletionChunk
				if err := json.Unmarshal([]byte(f.data), &chunk); err != nil {
					send(Event{Type: EventError, Err: fmt.Errorf("deepseek: decode chunk: %w", err)})
					return
				}
				for _, e := range chunkToEvents(chunk) {
					if !send(e) {
						return
					}
				}
			}
		}
	}
}

// chunkToEvents translates one decoded SSE frame into zero or more typed
// events. Reasoning-only frames carry a nil or empty Content, per
// docs/OBSERVED.md, and never produce an EventContentDelta.
func chunkToEvents(chunk ChatCompletionChunk) []Event {
	var events []Event
	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]
		d := choice.Delta
		if d.ReasoningContent != nil && *d.ReasoningContent != "" {
			events = append(events, Event{Type: EventReasoningDelta, Reasoning: *d.ReasoningContent})
		}
		if d.Content != nil && *d.Content != "" {
			events = append(events, Event{Type: EventContentDelta, Content: *d.Content})
		}
		for _, tc := range d.ToolCalls {
			events = append(events, Event{Type: EventToolCallDelta, ToolCall: tc})
		}
		if choice.FinishReason != nil {
			events = append(events, Event{Type: EventFinish, FinishReason: *choice.FinishReason})
		}
	}
	if chunk.Usage != nil {
		events = append(events, Event{Type: EventUsage, Usage: chunk.Usage})
	}
	return events
}

// IsReasoningStarved reports whether a completion hit its max_tokens
// ceiling before producing any answer text. Reasoning is generated before
// content, so an undersized budget is spent entirely on reasoning and bills
// in full while returning nothing usable; this is a distinct condition from
// an answer that was truncated mid-sentence (docs/OBSERVED.md).
func IsReasoningStarved(finishReason, content string) bool {
	return finishReason == FinishLength && content == ""
}
