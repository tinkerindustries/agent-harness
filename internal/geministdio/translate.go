package geministdio

import (
	"encoding/json"
	"sync"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// translator turns one session's committed event log, and the live text
// deltas published alongside it, into Google's step-event vocabulary.
//
// The two sources say different things and both are needed. The event log is
// authoritative and ordered but arrives one batch per sub-turn, so a client
// fed only from it would see a sub-turn's whole answer appear at once. The
// live deltas (internal/hub, LiveDelta) carry the same text as it is
// produced, coalesced on an interval and always flushed at the end of a
// sub-turn, so they are complete. This translator therefore streams text from
// the live deltas and takes structure — tool calls, results, thought
// signatures, usage, the run's end — from the log.
//
// One consequence is a deviation from Google's own stream, recorded in
// docs/STDIO-PROTOCOL.md: a thought step and a model_output step can be open
// at the same time. The harness learns a sub-turn's thought signature only
// when the sub-turn commits, which is after the answer text has already
// streamed, so the thought step cannot be closed before the model_output step
// opens without losing the signature's place. A client must key steps by
// their index and must not assume the highest index is the only open one.
type translator struct {
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
	steps []Step
	// stepAt maps a step index to its position in steps, so a delta can be
	// folded onto the step it belongs to.
	stepAt map[int]int

	usage Usage

	// idMu guards the message-id map, which the append handler writes from
	// its own goroutine while this translator reads it from the streaming
	// one.
	idMu           sync.Mutex
	firstMessageID string
	messageIDs     map[int64]string

	// callNames remembers which function a call id belonged to, so a result
	// step can carry the name Google's FunctionResultStep has an optional
	// field for.
	callNames map[string]string
}

func newTranslator(interactionID, model string, emit func(method string, params any)) *translator {
	return &translator{
		interactionID: interactionID,
		model:         model,
		emit:          emit,
		thought:       -1,
		output:        -1,
		stepAt:        map[int]int{},
		messageIDs:    map[int64]string{},
		callNames:     map[string]string{},
	}
}

// created emits interaction.created, the first frame of every stream.
func (t *translator) created() {
	t.emit(NotifyInteractionCreated, interactionEnvelope{
		Interaction: Interaction{
			ID: t.interactionID, Object: "interaction",
			Model: t.model, Status: StatusInProgress,
		},
		EventType: NotifyInteractionCreated,
	})
}

// startStep opens a step, emits step.start, and returns its index.
func (t *translator) startStep(s Step) int {
	idx := t.nextIndex
	t.nextIndex++
	if s.Harness == nil && t.subTurn > 0 {
		s.Harness = &StepHarness{SubTurn: t.subTurn}
	} else if s.Harness != nil && s.Harness.SubTurn == 0 {
		s.Harness.SubTurn = t.subTurn
	}
	t.stepsMu.Lock()
	t.stepAt[idx] = len(t.steps)
	t.steps = append(t.steps, s)
	t.stepsMu.Unlock()
	t.emit(NotifyStepStart, stepStart{
		InteractionID: t.interactionID, Index: idx, Step: s, EventType: NotifyStepStart,
	})
	return idx
}

// stopStep closes a step by index.
func (t *translator) stopStep(idx int) {
	if idx < 0 {
		return
	}
	t.emit(NotifyStepStop, stepStop{
		InteractionID: t.interactionID, Index: idx, EventType: NotifyStepStop,
	})
}

func (t *translator) delta(idx int, d Delta) {
	t.emit(NotifyStepDelta, stepDelta{
		InteractionID: t.interactionID, Index: idx, Delta: d, EventType: NotifyStepDelta,
	})
	t.foldDelta(idx, d)
}

// foldDelta accumulates a delta onto the assembled step, so the interaction
// interactions.get returns is the same document a client would have built by
// following the stream.
func (t *translator) foldDelta(idx int, d Delta) {
	t.stepsMu.Lock()
	defer t.stepsMu.Unlock()
	pos, ok := t.stepAt[idx]
	if !ok {
		return
	}
	s := &t.steps[pos]
	switch d.Type {
	case DeltaText:
		appendText(&s.Content, d.Text)
	case DeltaThoughtSummary:
		if d.Content != nil {
			appendText(&s.Summary, d.Content.Text)
		}
	case DeltaThoughtSignature:
		s.Signature = d.Signature
	case DeltaArguments:
		if len(s.Arguments) == 0 || string(s.Arguments) == "{}" {
			s.Arguments = json.RawMessage(d.Arguments)
			return
		}
		s.Arguments = append(s.Arguments, []byte(d.Arguments)...)
	}
}

func appendText(parts *[]Content, text string) {
	if text == "" {
		return
	}
	if n := len(*parts); n > 0 && (*parts)[n-1].Type == "text" {
		(*parts)[n-1].Text += text
		return
	}
	*parts = append(*parts, Content{Type: "text", Text: text})
}

// live translates one hub live delta: the answer text and the reasoning
// summary as the model produces them.
func (t *translator) live(d hub.LiveDelta) {
	switch d.Channel {
	case hub.ChannelReasoning:
		if t.thought < 0 {
			t.thought = t.startStep(Step{Type: StepThought})
		}
		t.delta(t.thought, Delta{Type: DeltaThoughtSummary, Content: &Content{Type: "text", Text: d.Text}})
	case hub.ChannelContent:
		if t.output < 0 {
			t.output = t.startStep(Step{Type: StepModelOutput})
		}
		t.delta(t.output, Delta{Type: DeltaText, Text: d.Text})
	}
}

// event translates one committed event.
func (t *translator) event(e store.Event) {
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
		t.userInput(p.OpeningMessage, "input", t.messageIDFor(0))
	case store.KindSteerApplied:
		var p store.SteerAppliedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		source := "append"
		if p.SourceSeq == 0 {
			source = "reminder"
		}
		t.userInput(p.Text, source, t.messageIDFor(p.SourceSeq))
	case store.KindTurnStarted:
		var p store.TurnStartedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.closeText()
		t.subTurn = p.SubTurn
		t.emit(NotifyInteractionStatusUpdate, statusUpdate{
			InteractionID: t.interactionID, Status: StatusInProgress,
			EventType: NotifyInteractionStatusUpdate,
			Harness:   &StepHarness{SubTurn: p.SubTurn},
		})
	case store.KindReasoningDelta:
		var p store.ReasoningDeltaPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		if t.thought < 0 {
			// No live deltas reached this translator — the run had no hub,
			// or the sub-turn produced a signature and no summary at all,
			// which is the common Gemini case. Open the step now and emit
			// the whole summary as one delta.
			t.thought = t.startStep(Step{Type: StepThought})
			if p.Text != "" {
				t.delta(t.thought, Delta{Type: DeltaThoughtSummary, Content: &Content{Type: "text", Text: p.Text}})
			}
		}
		if p.ThoughtSignature != "" {
			t.delta(t.thought, Delta{Type: DeltaThoughtSignature, Signature: p.ThoughtSignature})
		}
		t.stopStep(t.thought)
		t.thought = -1
	case store.KindContentDelta:
		var p store.ContentDeltaPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		if t.output < 0 {
			t.output = t.startStep(Step{Type: StepModelOutput})
			if p.Text != "" {
				t.delta(t.output, Delta{Type: DeltaText, Text: p.Text})
			}
		}
		t.stopStep(t.output)
		t.output = -1
	case store.KindToolCall:
		var p store.ToolCallPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.closeText()
		t.callNames[p.ID] = p.Name
		idx := t.startStep(Step{
			Type: StepFunctionCall, ID: p.ID, Name: p.Name,
			Arguments: json.RawMessage("{}"),
		})
		if p.Arguments != "" {
			t.delta(idx, Delta{Type: DeltaArguments, Arguments: p.Arguments})
		}
		t.stopStep(idx)
	case store.KindToolResult:
		var p store.ToolResultPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		result := TextContent(p.Content)
		if p.ImageURL != "" {
			if mime, data, ok := decodeDataURI(p.ImageURL); ok {
				result = append(result, Content{Type: "image", MIMEType: mime, Data: data})
			}
		}
		h := &StepHarness{Truncated: p.Truncated, ChildInteractionID: p.ChildSessionID}
		idx := t.startStep(Step{
			Type: StepFunctionResult, CallID: p.ToolCallID, Name: p.Name,
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
		idx := t.startStep(Step{
			Type: StepFunctionResult, CallID: p.ToolCallID, Name: p.Name,
			Result: TextContent(p.Content), IsError: true,
			Harness: &StepHarness{Rule: p.Rule},
		})
		t.stopStep(idx)
	case store.KindToolStdout:
		var p store.ToolStdoutPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.emit(NotifyToolOutput, toolOutput{
			InteractionID: t.interactionID, CallID: p.ToolCallID,
			Text: p.Text, EventType: NotifyToolOutput,
		})
	case store.KindUsage:
		var p store.UsagePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		one := usageFrom(p)
		t.addUsage(one)
		t.emit(NotifyUsage, usageEvent{
			InteractionID: t.interactionID, SubTurn: p.SubTurn,
			Usage: one, EventType: NotifyUsage,
		})
	case store.KindTurnFinished:
		t.closeText()
	case store.KindError:
		var p store.ErrorPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.closeText()
		t.emit(NotifyError, errorEvent{
			InteractionID: t.interactionID,
			Error:         Error{Code: "internal", Message: p.Message},
			EventType:     NotifyError,
		})
	}
}

// setFirstMessageID records the id the create body's own input was sent
// under, echoed on the user_input step the run's opening message becomes.
func (t *translator) setFirstMessageID(id string) {
	t.idMu.Lock()
	t.firstMessageID = id
	t.idMu.Unlock()
}

// setMessageID records the id an appended input was sent under, keyed by the
// steer_message sequence number the append landed at. The steer_applied
// event that eventually folds it into the conversation names that same
// sequence, which is what ties the echo back to what the client sent.
func (t *translator) setMessageID(seq int64, id string) {
	t.idMu.Lock()
	t.messageIDs[seq] = id
	t.idMu.Unlock()
}

func (t *translator) messageIDFor(seq int64) string {
	t.idMu.Lock()
	defer t.idMu.Unlock()
	if seq == 0 {
		return t.firstMessageID
	}
	return t.messageIDs[seq]
}

// userInput emits a user_input step. It carries its whole content on
// step.start and emits no deltas, because the text was never streamed — it
// was already complete when the loop recorded it.
func (t *translator) userInput(text, source, messageID string) {
	if text == "" {
		return
	}
	t.closeText()
	idx := t.startStep(Step{
		Type: StepUserInput, Content: TextContent(text),
		Harness: &StepHarness{Source: source, MessageID: messageID},
	})
	t.stopStep(idx)
}

// closeText closes whichever of the two text steps is still open. Called
// before anything that must not appear inside them.
func (t *translator) closeText() {
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
func (t *translator) snapshot() ([]Step, Usage) {
	t.stepsMu.Lock()
	defer t.stepsMu.Unlock()
	steps := make([]Step, len(t.steps))
	copy(steps, t.steps)
	return steps, t.usage
}

func (t *translator) addUsage(u Usage) {
	t.stepsMu.Lock()
	defer t.stepsMu.Unlock()
	t.usage.TotalTokens += u.TotalTokens
	t.usage.TotalInputTokens += u.TotalInputTokens
	t.usage.TotalCachedTokens += u.TotalCachedTokens
	t.usage.TotalOutputTokens += u.TotalOutputTokens
	t.usage.TotalThoughtTokens += u.TotalThoughtTokens
	if t.usage.Harness == nil {
		t.usage.Harness = &UsageX{}
	}
	if u.Harness != nil {
		t.usage.Harness.CostUSD += u.Harness.CostUSD
	}
}

// usageFrom maps one stored usage payload onto Google's usage object. The
// harness records cache hit and miss separately, where Google reports total
// input with the cached part as a subset of it, so the two are added back
// together (internal/gemini/types.go, TokenSplit, does the same mapping in
// the other direction).
func usageFrom(p store.UsagePayload) Usage {
	return Usage{
		TotalTokens:        p.PromptCacheHitTokens + p.PromptCacheMissTokens + p.CompletionTokens,
		TotalInputTokens:   p.PromptCacheHitTokens + p.PromptCacheMissTokens,
		TotalCachedTokens:  p.PromptCacheHitTokens,
		TotalOutputTokens:  p.CompletionTokens - p.ReasoningTokens,
		TotalThoughtTokens: p.ReasoningTokens,
		Harness:            &UsageX{CostUSD: p.CostUSD},
	}
}

// decodeDataURI splits a "data:<mime>;base64,<data>" URL into its mime type
// and payload, which is already the encoding a Google image Content wants.
func decodeDataURI(uri string) (mime, data string, ok bool) {
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
