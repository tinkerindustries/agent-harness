package stdiosession

import (
	"encoding/json"
	"sync"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// iactTranslator turns one session's committed Event log, and the Live text
// deltas published alongside it, into Google's step-Event vocabulary.
//
// The two sources say different things and both are needed. The Event log is
// authoritative and ordered but arrives one batch per sub-turn, so a client
// fed only from it would see a sub-turn's whole answer appear at once. The
// Live deltas (internal/hub, LiveDelta) carry the same text as it is
// produced, coalesced on an interval and always flushed at the end of a
// sub-turn, so they are complete. This iactTranslator therefore streams text from
// the Live deltas and takes structure — tool calls, results, thought
// signatures, usage, the run's end — from the log.
//
// One consequence is a deviation from Google's own stream, recorded in
// docs/STDIO-PROTOCOL.md: a thought step and a model_output step can be open
// at the same time. The harness learns a sub-turn's thought signature only
// when the sub-turn commits, which is after the answer text has already
// streamed, so the thought step cannot be closed before the model_output step
// opens without losing the signature's place. A client must key steps by
// their index and must not assume the highest index is the only open one.
type iactTranslator struct {
	interactionID string
	model         string
	emit          func(method string, params any)

	nextIndex int
	// thought and output are the indices of the open thought and
	// model_output steps, or -1 when none is open.
	thought int
	output  int
	subTurn int

	// stepsMu guards steps, stepAt and usage. Everything that writes them
	// runs on the one goroutine draining the pump; the lock is for
	// interactions.get, which reads them from a client's own goroutine while
	// the run is still going.
	stepsMu sync.Mutex
	// steps accumulates the assembled interaction, which is what
	// interactions.get returns and what a stream:false create answers with.
	steps []iactStep
	// stepAt maps a step index to its position in steps, so a delta can be
	// folded onto the step it belongs to.
	stepAt map[int]int

	usage iactUsage

	// idMu guards the message-id map, which the append handler writes from
	// its own goroutine while this iactTranslator reads it from the streaming
	// one.
	idMu           sync.Mutex
	firstMessageID string
	messageIDs     map[int64]string
}

func newIactTranslator(interactionID, model string, emit func(method string, params any)) *iactTranslator {
	return &iactTranslator{
		interactionID: interactionID,
		model:         model,
		emit:          emit,
		thought:       -1,
		output:        -1,
		stepAt:        map[int]int{},
		messageIDs:    map[int64]string{},
	}
}

// Created emits interaction.Created, the first frame of every stream.
func (t *iactTranslator) Created() {
	t.emit(notifyIactCreated, iactEnvelope{
		Interaction: iactInteraction{
			ID: t.interactionID, Object: "interaction",
			Model: t.model, Status: StatusInProgress,
		},
		EventType: notifyIactCreated,
	})
}

// startStep opens a step, emits step.start, and returns its index.
func (t *iactTranslator) startStep(s iactStep) int {
	idx := t.nextIndex
	t.nextIndex++
	if s.Harness == nil && t.subTurn > 0 {
		s.Harness = &iactStepHarness{SubTurn: t.subTurn}
	} else if s.Harness != nil && s.Harness.SubTurn == 0 {
		s.Harness.SubTurn = t.subTurn
	}
	t.stepsMu.Lock()
	t.stepAt[idx] = len(t.steps)
	t.steps = append(t.steps, s)
	t.stepsMu.Unlock()
	t.emit(notifyIactStepStart, iactStepStart{
		InteractionID: t.interactionID, Index: idx, Step: s, EventType: notifyIactStepStart,
	})
	return idx
}

// stopStep closes a step by index.
func (t *iactTranslator) stopStep(idx int) {
	if idx < 0 {
		return
	}
	t.emit(notifyIactStepStop, iactStepStop{
		InteractionID: t.interactionID, Index: idx, EventType: notifyIactStepStop,
	})
}

func (t *iactTranslator) delta(idx int, d iactDelta) {
	t.emit(notifyIactStepDelta, iactStepDelta{
		InteractionID: t.interactionID, Index: idx, Delta: d, EventType: notifyIactStepDelta,
	})
	t.foldDelta(idx, d)
}

// foldDelta accumulates a delta onto the assembled step, so the interaction
// interactions.get returns is the same document a client would have built by
// following the stream.
func (t *iactTranslator) foldDelta(idx int, d iactDelta) {
	t.stepsMu.Lock()
	defer t.stepsMu.Unlock()
	pos, ok := t.stepAt[idx]
	if !ok {
		return
	}
	s := &t.steps[pos]
	switch d.Type {
	case iactDeltaText:
		iactAppendText(&s.Content, d.Text)
	case iactDeltaThoughtSummary:
		if d.Content != nil {
			iactAppendText(&s.Summary, d.Content.Text)
		}
	case iactDeltaThoughtSignature:
		s.Signature = d.Signature
	case iactDeltaArguments:
		if len(s.Arguments) == 0 || string(s.Arguments) == "{}" {
			s.Arguments = json.RawMessage(d.Arguments)
			return
		}
		s.Arguments = append(s.Arguments, []byte(d.Arguments)...)
	}
}

func iactAppendText(parts *[]iactContent, text string) {
	if text == "" {
		return
	}
	if n := len(*parts); n > 0 && (*parts)[n-1].Type == "text" {
		(*parts)[n-1].Text += text
		return
	}
	*parts = append(*parts, iactContent{Type: "text", Text: text})
}

// Live translates one hub Live delta: the answer text and the reasoning
// summary as the model produces them.
func (t *iactTranslator) Live(d hub.LiveDelta) {
	switch d.Channel {
	case hub.ChannelReasoning:
		if t.thought < 0 {
			t.thought = t.startStep(iactStep{Type: iactStepThought})
		}
		t.delta(t.thought, iactDelta{Type: iactDeltaThoughtSummary, Content: &iactContent{Type: "text", Text: d.Text}})
	case hub.ChannelContent:
		if t.output < 0 {
			t.output = t.startStep(iactStep{Type: iactStepModelOutput})
		}
		t.delta(t.output, iactDelta{Type: iactDeltaText, Text: d.Text})
	}
}

// Event translates one committed Event.
func (t *iactTranslator) Event(e store.Event) {
	switch e.Kind {
	case store.KindSessionStarted:
		var p store.SessionStartedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		// The opening message is what the model was actually given: the
		// task, the workspace listing, the skills catalogue, the repository
		// instructions. A parent that wants to render only what its user
		// typed reads harness.source and the task it sent, not this text.
		t.UserInput(p.OpeningMessage, "input", t.messageIDFor(0))
	case store.KindSteerApplied:
		var p store.SteerAppliedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		source := "append"
		if p.SourceSeq == 0 {
			source = "reminder"
		}
		t.UserInput(p.Text, source, t.messageIDFor(p.SourceSeq))
	case store.KindTurnStarted:
		var p store.TurnStartedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.CloseText()
		t.subTurn = p.SubTurn
		t.emit(notifyIactStatusUpdate, iactStatusUpdate{
			InteractionID: t.interactionID, Status: StatusInProgress,
			EventType: notifyIactStatusUpdate,
			Harness:   &iactStepHarness{SubTurn: p.SubTurn},
		})
	case store.KindReasoningDelta:
		var p store.ReasoningDeltaPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		if t.thought < 0 {
			// No Live deltas reached this iactTranslator — the run had no hub,
			// or the sub-turn produced a signature and no summary at all,
			// which is the common Gemini case. Open the step now and emit
			// the whole summary as one delta.
			t.thought = t.startStep(iactStep{Type: iactStepThought})
			if p.Text != "" {
				t.delta(t.thought, iactDelta{Type: iactDeltaThoughtSummary, Content: &iactContent{Type: "text", Text: p.Text}})
			}
		}
		if p.ThoughtSignature != "" {
			t.delta(t.thought, iactDelta{Type: iactDeltaThoughtSignature, Signature: p.ThoughtSignature})
		}
		t.stopStep(t.thought)
		t.thought = -1
	case store.KindContentDelta:
		var p store.ContentDeltaPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		if t.output < 0 {
			t.output = t.startStep(iactStep{Type: iactStepModelOutput})
			if p.Text != "" {
				t.delta(t.output, iactDelta{Type: iactDeltaText, Text: p.Text})
			}
		}
		t.stopStep(t.output)
		t.output = -1
	case store.KindToolCall:
		var p store.ToolCallPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.CloseText()
		idx := t.startStep(iactStep{
			Type: iactStepFunctionCall, ID: p.ID, Name: p.Name,
			Arguments: json.RawMessage("{}"),
		})
		if p.Arguments != "" {
			t.delta(idx, iactDelta{Type: iactDeltaArguments, Arguments: p.Arguments})
		}
		t.stopStep(idx)
	case store.KindToolResult:
		var p store.ToolResultPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		result := iactTextContent(p.Content)
		if p.ImageURL != "" {
			if mime, data, ok := iactDecodeDataURI(p.ImageURL); ok {
				result = append(result, iactContent{Type: "image", MIMEType: mime, Data: data})
			}
		}
		h := &iactStepHarness{Truncated: p.Truncated, ChildInteractionID: p.ChildSessionID}
		idx := t.startStep(iactStep{
			Type: iactStepFunctionResult, CallID: p.ToolCallID, Name: p.Name,
			Result: result, IsError: p.IsError, Harness: h,
		})
		t.stopStep(idx)
	case store.KindToolDenied:
		var p store.ToolDeniedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		// A denial is a function_result carrying is_error, because that is
		// what the model is shown and Google's step union has no third
		// outcome. The rule that refused it rides in the harness block, so
		// a parent can render a denial differently from a tool that failed.
		idx := t.startStep(iactStep{
			Type: iactStepFunctionResult, CallID: p.ToolCallID, Name: p.Name,
			Result: iactTextContent(p.Content), IsError: true,
			Harness: &iactStepHarness{Rule: p.Rule},
		})
		t.stopStep(idx)
	case store.KindToolStdout:
		var p store.ToolStdoutPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.emit(notifyIactToolOutput, iactToolOutput{
			InteractionID: t.interactionID, CallID: p.ToolCallID,
			Text: p.Text, EventType: notifyIactToolOutput,
		})
	case store.KindUsage:
		var p store.UsagePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		one := iactUsageFrom(p)
		t.addUsage(one)
		t.emit(notifyIactUsage, iactUsageEvent{
			InteractionID: t.interactionID, SubTurn: p.SubTurn,
			Usage: one, EventType: notifyIactUsage,
		})
	case store.KindTurnFinished:
		t.CloseText()
	case store.KindError:
		var p store.ErrorPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.CloseText()
		t.emit(notifyIactError, iactErrorEvent{
			InteractionID: t.interactionID,
			Error:         iactError{Code: "internal", Message: p.Message},
			EventType:     notifyIactError,
		})
	}
}

// SetFirstMessageID records the id the create body's own input was sent
// under, echoed on the user_input step the run's opening message becomes.
func (t *iactTranslator) SetFirstMessageID(id string) {
	t.idMu.Lock()
	t.firstMessageID = id
	t.idMu.Unlock()
}

// SetMessageID records the id an appended input was sent under, keyed by the
// steer_message sequence number the append landed at. The steer_applied
// Event that eventually folds it into the conversation names that same
// sequence, which is what ties the echo back to what the client sent.
func (t *iactTranslator) SetMessageID(seq int64, id string) {
	t.idMu.Lock()
	t.messageIDs[seq] = id
	t.idMu.Unlock()
}

func (t *iactTranslator) messageIDFor(seq int64) string {
	t.idMu.Lock()
	defer t.idMu.Unlock()
	if seq == 0 {
		return t.firstMessageID
	}
	return t.messageIDs[seq]
}

// UserInput emits a user_input step. It carries its whole content on
// step.start and emits no deltas, because the text was never streamed — it
// was already complete when the loop recorded it.
func (t *iactTranslator) UserInput(text, source, messageID string) {
	if text == "" {
		return
	}
	t.CloseText()
	idx := t.startStep(iactStep{
		Type: iactStepUserInput, Content: iactTextContent(text),
		Harness: &iactStepHarness{Source: source, MessageID: messageID},
	})
	t.stopStep(idx)
}

// CloseText closes whichever of the two text steps is still open. Called
// before anything that must not appear inside them.
func (t *iactTranslator) CloseText() {
	if t.thought >= 0 {
		t.stopStep(t.thought)
		t.thought = -1
	}
	if t.output >= 0 {
		t.stopStep(t.output)
		t.output = -1
	}
}

// snapshot returns the interaction as assembled so far: the steps and the
// running usage total. Safe to call while the run is still going, which is
// what interactions.get on an in-progress interaction needs.
func (t *iactTranslator) snapshot() ([]iactStep, iactUsage) {
	t.stepsMu.Lock()
	defer t.stepsMu.Unlock()
	steps := make([]iactStep, len(t.steps))
	copy(steps, t.steps)
	return steps, t.usage
}

func (t *iactTranslator) addUsage(u iactUsage) {
	t.stepsMu.Lock()
	defer t.stepsMu.Unlock()
	t.usage.TotalTokens += u.TotalTokens
	t.usage.TotalInputTokens += u.TotalInputTokens
	t.usage.TotalCachedTokens += u.TotalCachedTokens
	t.usage.TotalOutputTokens += u.TotalOutputTokens
	t.usage.TotalThoughtTokens += u.TotalThoughtTokens
	if t.usage.Harness == nil {
		t.usage.Harness = &iactUsageX{}
	}
	if u.Harness != nil {
		t.usage.Harness.CostUSD += u.Harness.CostUSD
	}
}

// iactUsageFrom maps one stored usage payload onto Google's usage object. The
// harness records cache hit and miss separately, where Google reports total
// input with the cached part as a subset of it, so the two are added back
// together (internal/gemini/types.go, TokenSplit, does the same mapping in
// the other direction).
func iactUsageFrom(p store.UsagePayload) iactUsage {
	return iactUsage{
		TotalTokens:        p.PromptCacheHitTokens + p.PromptCacheMissTokens + p.CompletionTokens,
		TotalInputTokens:   p.PromptCacheHitTokens + p.PromptCacheMissTokens,
		TotalCachedTokens:  p.PromptCacheHitTokens,
		TotalOutputTokens:  p.CompletionTokens - p.ReasoningTokens,
		TotalThoughtTokens: p.ReasoningTokens,
		Harness:            &iactUsageX{CostUSD: p.CostUSD},
	}
}

// iactDecodeDataURI splits a "data:<mime>;base64,<data>" URL into its mime type
// and payload, which is already the encoding a Google image iactContent wants.
func iactDecodeDataURI(uri string) (mime, data string, ok bool) {
	const prefix = "data:"
	if len(uri) < len(prefix) || uri[:len(prefix)] != prefix {
		return "", "", false
	}
	rest := uri[len(prefix):]
	comma := -1
	for i := 0; i < len(rest); i++ {
		if rest[i] == ',' {
			comma = i
			break
		}
	}
	if comma < 0 {
		return "", "", false
	}
	header, payload := rest[:comma], rest[comma+1:]
	semi := -1
	for i := 0; i < len(header); i++ {
		if header[i] == ';' {
			semi = i
			break
		}
	}
	if semi < 0 || header[semi+1:] != "base64" {
		return "", "", false
	}
	return header[:semi], payload, true
}

// Resource builds Google's Interaction resource. The steps and the usage
// come from this translator whether the run is still going or has finished,
// so interactions.get on an in-progress interaction answers with what has
// happened so far rather than with nothing.
func (t *iactTranslator) Resource(v RunView) any {
	steps, usage := t.snapshot()
	out := iactInteraction{
		ID: v.ID, Object: "interaction", Model: v.Model, Status: v.Status,
		Created: v.Created.Format(time.RFC3339),
		Updated: v.Updated.Format(time.RFC3339),
	}
	if v.Err != nil {
		// Google's resource carries an array where the Responses one
		// carries a single error. A run fails once, so the array has one
		// member, and it is still an array because Google's schema says so.
		out.Errors = []iactError{{Code: v.Err.Code, Message: v.Err.Message}}
	}
	if v.WithItems {
		out.Steps = steps
	}
	if usage.TotalTokens > 0 {
		if usage.Harness != nil && v.SubTurns > 0 {
			h := *usage.Harness
			h.SubTurns = v.SubTurns
			usage.Harness = &h
		}
		out.Usage = &usage
	}
	out.Harness = &iactInteractionHarness{
		SessionID: v.SessionID,
		Reason:    v.Reason,
		Text:      v.Text,
		Result:    v.Result,
		SubTurns:  v.SubTurns,
	}
	return out
}

// Result is the body interactions.create, .cancel and .get answer with.
func (t *iactTranslator) Result(v RunView) any {
	return iactCreateResult{Interaction: t.Resource(v).(iactInteraction)}
}

// Completed emits interaction.completed, which ends the interaction.
//
// It does not carry the steps. That is Google's own shape and it is what the
// clients built against this dialect expect: the steps were streamed as
// step.start/step.delta/step.stop, and a client that wants the assembled
// document asks for it with interactions.get. The Responses dialect answers
// this question the other way, which is why the frame is built here rather
// than in the server.
func (t *iactTranslator) Completed(v RunView) {
	v.WithItems = false
	t.emit(notifyIactCompleted, iactEnvelope{
		Interaction: t.Resource(v).(iactInteraction),
		EventType:   notifyIactCompleted,
	})
}

// Failed emits the `error` notification for a run the loop could not finish.
//
// Google's error frame is the interaction id and the error, and nothing
// else — not the resource the Responses surface's response.failed carries.
// interaction.completed still follows it, so a client that reads only
// terminal frames is not left waiting.
func (t *iactTranslator) Failed(v RunView) {
	e := iactError{Code: "internal", Message: ""}
	if v.Err != nil {
		e = iactError{Code: v.Err.Code, Message: v.Err.Message}
	}
	t.emit(notifyIactError, iactErrorEvent{
		InteractionID: t.interactionID,
		Error:         e,
		EventType:     notifyIactError,
	})
}
