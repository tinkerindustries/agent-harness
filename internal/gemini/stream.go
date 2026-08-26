package gemini

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// The streaming half of the interactions surface. The request carries
// stream: true and the response is text/event-stream; frames are
// "event: <name>\ndata: <json>\n\n" and the stream ends with an event named
// done whose data is the literal [DONE].
//
// Streaming is how a review call stays inside its deadline. The unary form
// sends nothing until the model has finished thinking, which for a
// multi-image review reaches 29s
// (docs/reviews/sess-8df2a5f78847c4737b0e86e6e5d069b6.md); streamed, the
// first frame arrives immediately and the connection is never idle.
//
// The frame vocabulary below was measured against the live API on
// 2026-08-14, and it is wider than the published examples: an
// interaction.status_update event appears that the docs do not show, and the
// two delta shapes nest their text differently — a text delta carries
// delta.text, a thought_summary delta carries delta.content.text.
//
// decodeStream and applyDelta below are Interact's own consumer — the
// vision path, which sends no tools and asks for no thought signatures, so
// they read only the text and thought_summary deltas they always have. The
// agentic path's own consumer, pumpChatEvents, reads the same frames further
// down this file: it is what turns arguments_delta, thought_signature,
// step.stop and error into wire.Events (docs/GEMINI-INTEGRATION.md §5.4).
// streamFrame, streamStep and streamDelta are shared by both consumers —
// extended with the fields the agentic path needs, which decodeStream and
// applyDelta simply never read — rather than duplicated, since they decode
// the identical wire frames either way.

// Stream event names.
const (
	eventInteractionCreated   = "interaction.created"
	eventInteractionCompleted = "interaction.completed"
	eventStepStart            = "step.start"
	eventStepDelta            = "step.delta"
	eventStepStop             = "step.stop"
	eventError                = "error"
	eventDone                 = "done"
)

// Delta types inside a step.delta frame. text and thought_summary are the
// two decodeStream (the vision path) ever sees. arguments_delta,
// thought_signature and image are the three it used to ignore outright —
// pumpChatEvents, the agentic path's consumer, is what reads them now
// (docs/GEMINI-INTEGRATION.md §5.4, §7 "Phase 4"). image has no counterpart
// in wire.Event: this harness never asks Gemini to generate image output
// (no response_modalities is ever set), so pumpChatEvents skips it exactly
// as decodeStream always skipped every delta type it did not know, rather
// than guessing at a shape nothing here consumes.
const (
	deltaText             = "text"
	deltaThoughtSummary   = "thought_summary"
	deltaArguments        = "arguments_delta"
	deltaThoughtSignature = "thought_signature"
	deltaImage            = "image"
)

// streamFrame is one decoded SSE frame's data payload. The fields are the
// union of what the frames this client cares about carry; each event name
// populates a different subset. Error is set only on an error event
// (docs/OBSERVED.md, "The error body itself is a finding" — an SSE-framed
// error carries {"error":{...}} at the top level of the frame, the same
// Error shape errors.go's plain-JSON path also parses).
type streamFrame struct {
	EventType   string           `json:"event_type"`
	Index       int              `json:"index"`
	Step        *streamStep      `json:"step"`
	Delta       *streamDelta     `json:"delta"`
	Interaction *streamInteraion `json:"interaction"`
	Error       *apiErrorBody    `json:"error"`
}

// streamStep is a step.start frame's step object. ID and Name are set only
// on a function_call step's opening frame (openapi.json "FunctionCallStep");
// decodeStream's vision path never reads them, since Interact sends no
// tools and so never sees a function_call step.
type streamStep struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// streamDelta is one increment of a step. Text is set on a text delta;
// Content is set on a thought_summary delta, which nests its text one level
// deeper; Arguments is set on an arguments_delta, already the JSON-string
// fragment wire.ToolCallFuncDelta.Arguments wants (docs/OBSERVED.md,
// "arguments_delta never fragmented" — never observed split, but the shape
// is a string either way); Signature is set on a thought_signature delta.
type streamDelta struct {
	Type      string   `json:"type"`
	Text      string   `json:"text"`
	Content   *Content `json:"content"`
	Arguments string   `json:"arguments"`
	Signature string   `json:"signature"`
}

// streamInteraion is the interaction object carried by the created and
// completed frames.
type streamInteraion struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Usage  *Usage `json:"usage"`
}

// maxStreamLine bounds one SSE data line. A thought signature runs to about
// 1.5KB and a text delta is smaller, so this is headroom rather than a
// working limit.
const maxStreamLine = 1 << 20

// decodeStream reads an event stream and assembles the same
// InteractionResponse the unary form returns, so nothing downstream of
// Interact can tell which transport produced it. Steps are indexed by the
// frames' own index, and a stream that ends without an
// interaction.completed frame is an error: a truncated answer must not be
// mistaken for a short one.
func decodeStream(body io.Reader) (*InteractionResponse, error) {
	out := &InteractionResponse{}
	steps := map[int]*Step{}
	highest := -1
	completed := false

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), maxStreamLine)

	var event, data string
	flush := func() error {
		if event == "" && data == "" {
			return nil
		}
		defer func() { event, data = "", "" }()
		if event == eventDone || strings.TrimSpace(data) == "[DONE]" {
			return nil
		}
		var f streamFrame
		if err := json.Unmarshal([]byte(data), &f); err != nil {
			return fmt.Errorf("gemini: decode %s frame: %w", event, err)
		}
		switch event {
		case eventInteractionCreated:
			if f.Interaction != nil {
				out.ID = f.Interaction.ID
				out.Status = f.Interaction.Status
			}
		case eventStepStart:
			s := &Step{}
			if f.Step != nil {
				s.Type = f.Step.Type
			}
			steps[f.Index] = s
			if f.Index > highest {
				highest = f.Index
			}
		case eventStepDelta:
			s, ok := steps[f.Index]
			if !ok {
				// A delta before its step.start: keep the content rather than
				// drop it, and let the step's type stay empty.
				s = &Step{}
				steps[f.Index] = s
				if f.Index > highest {
					highest = f.Index
				}
			}
			applyDelta(s, f.Delta)
		case eventInteractionCompleted:
			completed = true
			if f.Interaction != nil {
				if f.Interaction.ID != "" {
					out.ID = f.Interaction.ID
				}
				out.Status = f.Interaction.Status
				out.Usage = f.Interaction.Usage
			}
		}
		return nil
	}

	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return nil, err
			}
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("gemini: read stream: %w", err)
	}
	if err := flush(); err != nil {
		return nil, err
	}
	if !completed {
		return nil, fmt.Errorf("gemini: stream ended after %d step(s) without an %s frame", highest+1, eventInteractionCompleted)
	}

	for i := 0; i <= highest; i++ {
		if s, ok := steps[i]; ok {
			out.Steps = append(out.Steps, *s)
		}
	}
	return out, nil
}

// applyDelta folds one step.delta onto its step, concatenating consecutive
// text so the assembled step holds one part per kind rather than one per
// frame.
func applyDelta(s *Step, d *streamDelta) {
	if d == nil {
		return
	}
	switch d.Type {
	case deltaText:
		appendText(&s.Content, d.Text)
	case deltaThoughtSummary:
		if d.Content != nil {
			appendText(&s.Summary, d.Content.Text)
		}
	}
}

func appendText(parts *[]Content, text string) {
	if text == "" {
		return
	}
	if n := len(*parts); n > 0 && (*parts)[n-1].Type == ContentTypeText {
		(*parts)[n-1].Text += text
		return
	}
	*parts = append(*parts, Content{Type: ContentTypeText, Text: text})
}

// ErrIdleTimeout is sent when no SSE frame, including a keep-alive comment,
// arrives within the idle window — the same watchdog internal/deepseek and
// internal/kimi run on their own streams (docs/DESIGN.md §4.3), reimplemented
// here rather than shared because internal/providerhttp.Transport.PumpStream
// decodes wire.ChatCompletionChunk, DeepSeek's and Kimi's OpenAI-format
// shape, and has no notion of Gemini's step-typed frames — arguments as a
// delta string, thought signatures, step.start/step.stop — so pumpChatEvents
// below stays this package's own reader even though request retry now comes
// from that Transport (client.go's chatTransport, docs/GEMINI-INTEGRATION.md
// §8: the fix was a SetAuth seam on Transport.Do, not a rewrite of
// PumpStream).
var ErrIdleTimeout = errors.New("gemini: stream idle timeout")

// defaultIdleTimeout is pumpChatEvents' watchdog window when none is set.
// Interact has no equivalent: it has no response-header timeout either
// (client.go, "A response header timeout cannot be set here"), on the
// reasoning that the tool-level context is the only deadline that should
// bound a slow-to-think call. The agentic path keeps a watchdog regardless,
// matching DeepSeek's and Kimi's streams, because a session mid-run with no
// idle bound would hang the whole loop rather than one tool call.
const defaultIdleTimeout = 120 * time.Second

// idleTimeout is the watchdog window pumpChatEvents uses; overridable for
// tests via a field client_test.go / chatstream_test.go can set directly, in
// the same spirit as WithIdleTimeout on internal/deepseek and internal/kimi.
func (c *Client) idleTimeout() time.Duration {
	if c.chatIdleTimeout > 0 {
		return c.chatIdleTimeout
	}
	return defaultIdleTimeout
}

// interactionStatusIncomplete is the status the API reports when a
// generation was cut short by generation_config.max_output_tokens — a live
// measurement against gemini-3.7-flash: max_output_tokens=50 answered 200
// with status "incomplete" and 46 output tokens, against 678 uncapped. This
// is a narrower claim than docs/OBSERVED.md's "status is always completed"
// finding, which was measured only on uncapped requests — a *pending tool
// call* is still found by scanning steps, never by branching on status
// (chatStreamState.sawFunctionCall below still takes precedence over this),
// but a token-capped generation really does report a different status, and
// finishReasonFor needs to read it to give IsReasoningStarved anything to
// key off.
const interactionStatusIncomplete = "incomplete"

// chatStreamState is what pumpChatEvents carries across frames of one
// stream: whether any function_call step was seen, and the interaction
// status off the interaction.completed frame — together what
// finishReasonFor needs to synthesise a finish reason, since this surface
// has no finish_reason field of its own.
type chatStreamState struct {
	sawFunctionCall bool
	status          string
}

// finishReasonFor turns what pumpChatEvents observed into the finish-reason
// vocabulary wire.ChatCompletionRequest's OpenAI-format siblings already
// use. A pending function_call step takes precedence — the plan's own
// "status is always completed, even mid-turn with an unanswered
// function_call pending" finding, so status must never override it — then
// an "incomplete" status (a max_output_tokens cap cutting generation short,
// interactionStatusIncomplete's own comment) maps to FinishLength, the same
// value DeepSeek's finish_reason carries for the same condition, which is
// what lets IsReasoningStarved below reuse DeepSeek's exact predicate.
// Anything else is FinishStop.
func finishReasonFor(st chatStreamState) string {
	switch {
	case st.sawFunctionCall:
		return wire.FinishToolCalls
	case st.status == interactionStatusIncomplete:
		return wire.FinishLength
	default:
		return wire.FinishStop
	}
}

// pumpChatEvents reads body as the agentic stream and translates each frame
// into zero or more wire.Events on events, closing events when the stream
// ends — normally, on a decode error, on the idle watchdog firing, or when
// ctx is cancelled. It owns body and closes it.
//
// The shape mirrors internal/providerhttp.Transport.PumpStream (a goroutine
// reading lines behind a channel so the idle watchdog can still fire while a
// read blocks) without importing it, for the reason ErrIdleTimeout's comment
// gives. What differs from decodeStream above, this package's other stream
// reader, is not the frame vocabulary — both read the same
// streamFrame/streamStep/streamDelta — but what happens to it: decodeStream
// assembles a complete InteractionResponse for Interact to return once;
// this emits wire.Events as frames arrive, for the agent loop's
// StreamChatCompletion.
func (c *Client) pumpChatEvents(ctx context.Context, body io.ReadCloser, events chan<- wire.Event) {
	defer close(events)
	defer body.Close()

	send := func(e wire.Event) bool {
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
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64*1024), maxStreamLine)
		for sc.Scan() {
			select {
			case lines <- sc.Text():
			case <-ctx.Done():
				return
			}
		}
		if err := sc.Err(); err != nil {
			lineErrs <- err
		}
	}()

	idle := time.NewTimer(c.idleTimeout())
	defer idle.Stop()

	var event, data string
	var st chatStreamState

	// flushFrame decodes one accumulated event/data pair and dispatches it.
	// It returns (terminal, ok): terminal is true once the frame legitimately
	// ends the stream (interaction.completed or an error event), ok is false
	// only when a send could not be delivered because ctx was cancelled — in
	// both cases the caller stops reading.
	flushFrame := func() (terminal, ok bool) {
		if event == "" && data == "" {
			return false, true
		}
		e, d := event, data
		event, data = "", ""
		if e == eventDone || strings.TrimSpace(d) == "[DONE]" {
			return false, true
		}
		var f streamFrame
		if err := json.Unmarshal([]byte(d), &f); err != nil {
			return true, send(wire.Event{Type: wire.EventError, Err: fmt.Errorf("gemini: decode %s frame: %w", e, err)})
		}
		switch e {
		case eventError:
			// SSE-framed even when the HTTP status was already a plain 400
			// (docs/OBSERVED.md, "The error body itself is a finding"); this
			// is the mid-stream case, arriving inside a body pumpChatEvents
			// is already reading. errors.go's parseAgenticAPIError handles
			// the same shape for a non-200 response before a stream starts.
			return true, send(wire.Event{Type: wire.EventError, Err: apiErrorFrom(f.Error)})
		case eventStepStart:
			if f.Step != nil && f.Step.Type == StepTypeFunctionCall {
				st.sawFunctionCall = true
				if !send(wire.Event{Type: wire.EventToolCallDelta, ToolCall: wire.ToolCallDelta{
					Index: f.Index, ID: f.Step.ID, Type: "function",
					Function: wire.ToolCallFuncDelta{Name: f.Step.Name},
				}}) {
					return false, false
				}
			}
			return false, true
		case eventStepDelta:
			return false, sendChatDelta(send, f)
		case eventStepStop:
			// No event: nothing this client tracks changes at step.stop.
			// step_usage (openapi.json "StepStop") would let a caller cost
			// one tool call rather than one turn, which nothing here does
			// yet (docs/GEMINI-INTEGRATION.md §5.5, "future opportunity, out
			// of scope here").
			return false, true
		case eventInteractionCompleted:
			var usage *wire.Usage
			if f.Interaction != nil {
				usage = usageToWire(f.Interaction.Usage)
				st.status = f.Interaction.Status
			}
			if usage != nil {
				if !send(wire.Event{Type: wire.EventUsage, Usage: usage}) {
					return false, false
				}
			}
			return true, send(wire.Event{Type: wire.EventFinish, FinishReason: finishReasonFor(st)})
		default:
			// interaction.created and interaction.status_update carry
			// nothing this client needs yet; any future event name is
			// skipped the same way decodeStream skips one, rather than
			// erroring the stream over a frame it does not understand.
			return false, true
		}
	}

	for {
		select {
		case <-ctx.Done():
			send(wire.Event{Type: wire.EventError, Err: ctx.Err()})
			return
		case err := <-lineErrs:
			send(wire.Event{Type: wire.EventError, Err: fmt.Errorf("gemini: read stream: %w", err)})
			return
		case <-idle.C:
			send(wire.Event{Type: wire.EventError, Err: ErrIdleTimeout})
			return
		case line, ok := <-lines:
			if !ok {
				// EOF with no interaction.completed and no error frame ever
				// seen: a truncated answer must not be mistaken for a short
				// one, the same rule decodeStream enforces for Interact.
				send(wire.Event{Type: wire.EventError, Err: fmt.Errorf("gemini: stream ended without an %s or %s frame", eventInteractionCompleted, eventError)})
				return
			}
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(c.idleTimeout())
			switch {
			case line == "":
				terminal, ok := flushFrame()
				if !ok {
					return
				}
				if terminal {
					return
				}
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data += strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")
			}
		}
	}
}

// sendChatDelta translates one step.delta frame into zero or one
// wire.Event, per the delta-type vocabulary this file's const block names.
// A frame this client does not recognise (image, or anything the surface
// grows later) is skipped rather than erroring the stream, the same
// tolerance decodeStream already gives an unknown frame.
func sendChatDelta(send func(wire.Event) bool, f streamFrame) bool {
	if f.Delta == nil {
		return true
	}
	switch f.Delta.Type {
	case deltaText:
		if f.Delta.Text == "" {
			return true
		}
		return send(wire.Event{Type: wire.EventContentDelta, Content: f.Delta.Text})
	case deltaThoughtSummary:
		if f.Delta.Content == nil || f.Delta.Content.Text == "" {
			return true
		}
		return send(wire.Event{Type: wire.EventReasoningDelta, Reasoning: f.Delta.Content.Text})
	case deltaArguments:
		return send(wire.Event{Type: wire.EventToolCallDelta, ToolCall: wire.ToolCallDelta{
			Index:    f.Index,
			Function: wire.ToolCallFuncDelta{Arguments: f.Delta.Arguments},
		}})
	case deltaThoughtSignature:
		// A signature is one complete value, not a fragment to accumulate —
		// unlike EventReasoningDelta's payload, and unlike arguments, which
		// happen to have never been observed fragmenting either
		// (docs/OBSERVED.md, "arguments_delta never fragmented") but are
		// still run through the assembler defensively. A signature has no
		// assembler: it arrives once, as the final delta on a thought step
		// (docs/GEMINI-INTEGRATION.md §5.2).
		if f.Delta.Signature == "" {
			return true
		}
		return send(wire.Event{Type: wire.EventThoughtSignatureDelta, ThoughtSignature: f.Delta.Signature})
	default:
		return true
	}
}

// IsReasoningStarved reports whether a completion hit its
// max_output_tokens ceiling before producing any answer text — the same
// DeepSeek quirk internal/deepseek/stream.go's own IsReasoningStarved
// checks, and the same predicate: a finish reason of FinishLength with no
// content at all. Phase 2 recorded this as a no-op on the theory that no
// generation_config field caps total output tokens; that was a failed
// search, not an absence, corrected once max_output_tokens=50 was tried
// live and answered 200 with status "incomplete" and 46 output tokens
// (against 678 uncapped) — finishReasonFor above maps that status to
// FinishLength precisely so this predicate has something to key off, the
// same way DeepSeek's finish_reason does.
func (c *Client) IsReasoningStarved(finishReason, content string) bool {
	return finishReason == wire.FinishLength && content == ""
}
