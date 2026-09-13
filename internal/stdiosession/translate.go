package stdiosession

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// translator turns one session's committed event log, and the live text
// deltas published alongside it, into the Responses API's semantic event
// vocabulary.
//
// The two sources say different things and both are needed. The event log is
// authoritative and ordered but arrives one batch per sub-turn, so a client
// fed only from it would see a sub-turn's whole answer appear at once. The
// live deltas (internal/hub, LiveDelta) carry the same text as it is
// produced, coalesced on an interval and always flushed at the end of a
// sub-turn, so they are complete. This translator therefore streams text
// from the live deltas and takes structure — tool calls, results, usage, the
// run's end — from the log.
//
// One consequence is a deviation from the HTTP surface's own stream,
// recorded in docs/STDIO-PROTOCOL.md: a reasoning item and a message item
// can be open at the same time. A client must key items by output_index and
// must not assume the highest index is the only open one.
type translator struct {
	responseID string
	model      string
	emit       func(method string, params any)

	nextIndex int
	// reasoning and message are the output indices of the open reasoning
	// and message items, or -1 when none is open.
	reasoning int
	message   int
	subTurn   int

	// seq is the monotonic sequence_number every frame carries, the way the
	// HTTP surface's own frames do. Written only on the goroutine draining
	// the pump, which is the one that emits.
	seq int

	// itemsMu guards items, itemAt and usage. Everything that writes them
	// runs on the one goroutine draining the pump; the lock is for
	// responses.get, which reads them from a client's own goroutine while
	// the run is still going.
	itemsMu sync.Mutex
	// items accumulates the assembled response, which is what responses.get
	// returns and what a stream:false create answers with.
	items []OutputItem
	// itemAt maps an output index to its position in items, so a delta can
	// be folded onto the item it belongs to.
	itemAt map[int]int

	usage Usage

	// idMu guards the message-id map, which the append handler writes from
	// its own goroutine while this translator reads it from the streaming
	// one.
	idMu           sync.Mutex
	firstMessageID string
	messageIDs     map[int64]string
}

func newTranslator(responseID, model string, emit func(method string, params any)) *translator {
	return &translator{
		responseID: responseID,
		model:      model,
		emit:       emit,
		reasoning:  -1,
		message:    -1,
		itemAt:     map[int]int{},
		messageIDs: map[int64]string{},
	}
}

// next returns the sequence number for the next frame.
func (t *translator) next() int {
	n := t.seq
	t.seq++
	return n
}

// itemID is the id an item is addressed by on its deltas. The surface mints
// opaque ids; these are derived from the output index, which is the only
// thing a client needs them for.
func itemID(index int) string { return fmt.Sprintf("item_%d", index) }

// terminal emits the last frame a run produces — response.completed, or
// response.failed for one the loop could not finish — numbered in the same
// sequence as every frame before it.
//
// It is here rather than in server.go because the sequence counter is here.
// Built there, the two terminal frames carried no number at all, so a client
// that had counted a stream up to N was handed a final frame claiming to be
// frame 0 — and the two failure paths disagreed with each other, since the
// one this file already emitted for store.KindError numbered itself.
func (t *translator) terminal(method string, resp Response) {
	t.emit(method, responseEnvelope{Type: method, SequenceNumber: t.next(), Response: resp})
}

// created emits response.created, the first frame of every stream.
func (t *translator) Created() {
	t.emit(NotifyResponseCreated, responseEnvelope{
		Type:           NotifyResponseCreated,
		SequenceNumber: t.next(),
		Response: Response{
			ID: t.responseID, Object: "response",
			Model: t.model, Status: StatusInProgress,
		},
	})
}

// addItem opens an output item, emits response.output_item.added, and
// returns its output index.
func (t *translator) addItem(item OutputItem) int {
	idx := t.nextIndex
	t.nextIndex++
	item.ID = itemID(idx)
	item.Status = StatusInProgress
	if item.Harness == nil && t.subTurn > 0 {
		item.Harness = &ItemHarness{SubTurn: t.subTurn}
	} else if item.Harness != nil && item.Harness.SubTurn == 0 {
		item.Harness.SubTurn = t.subTurn
	}
	t.itemsMu.Lock()
	t.itemAt[idx] = len(t.items)
	t.items = append(t.items, item)
	t.itemsMu.Unlock()
	t.emit(NotifyOutputItemAdded, itemEvent{
		Type: NotifyOutputItemAdded, SequenceNumber: t.next(),
		ResponseID: t.responseID, OutputIndex: idx, Item: item,
	})
	return idx
}

// doneItem closes an item by output index, emitting the assembled item the
// way the surface's own .done frame carries it.
func (t *translator) doneItem(idx int) {
	if idx < 0 {
		return
	}
	t.itemsMu.Lock()
	var item OutputItem
	if pos, ok := t.itemAt[idx]; ok {
		t.items[pos].Status = StatusCompleted
		item = t.items[pos]
	}
	t.itemsMu.Unlock()
	t.emit(NotifyOutputItemDone, itemEvent{
		Type: NotifyOutputItemDone, SequenceNumber: t.next(),
		ResponseID: t.responseID, OutputIndex: idx, Item: item,
	})
}

// delta emits one incremental string against an open item and folds it onto
// the assembled copy.
func (t *translator) delta(method string, idx int, text string) {
	t.emit(method, textDelta{
		Type: method, SequenceNumber: t.next(), ResponseID: t.responseID,
		ItemID: itemID(idx), OutputIndex: idx, Delta: text,
	})
	t.foldDelta(method, idx, text)
}

// foldDelta accumulates a delta onto the assembled item, so the response
// responses.get returns is the same document a client would have built by
// following the stream.
func (t *translator) foldDelta(method string, idx int, text string) {
	t.itemsMu.Lock()
	defer t.itemsMu.Unlock()
	pos, ok := t.itemAt[idx]
	if !ok {
		return
	}
	item := &t.items[pos]
	switch method {
	case NotifyOutputTextDelta:
		appendText(&item.Content, PartOutputText, text)
	case NotifyReasoningTextDelta:
		appendText(&item.Content, PartReasoningText, text)
	case NotifyFunctionCallArgsDelta:
		if len(item.Arguments) == 0 || string(item.Arguments) == "{}" {
			item.Arguments = json.RawMessage(text)
			return
		}
		item.Arguments = append(item.Arguments, []byte(text)...)
	}
}

func appendText(parts *[]ContentPart, kind, text string) {
	if text == "" {
		return
	}
	if n := len(*parts); n > 0 && (*parts)[n-1].Type == kind {
		(*parts)[n-1].Text += text
		return
	}
	*parts = append(*parts, ContentPart{Type: kind, Text: text})
}

// live translates one hub live delta: the answer text and the reasoning
// text as the model produces them.
func (t *translator) Live(d hub.LiveDelta) {
	switch d.Channel {
	case hub.ChannelReasoning:
		if t.reasoning < 0 {
			t.reasoning = t.addItem(OutputItem{Type: ItemReasoning})
		}
		t.delta(NotifyReasoningTextDelta, t.reasoning, d.Text)
	case hub.ChannelContent:
		if t.message < 0 {
			t.message = t.addItem(OutputItem{Type: ItemMessage, Role: "assistant"})
		}
		t.delta(NotifyOutputTextDelta, t.message, d.Text)
	}
}

// event translates one committed event.
func (t *translator) Event(e store.Event) {
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
		t.emit(NotifyResponseInProgress, inProgress{
			Type: NotifyResponseInProgress, SequenceNumber: t.next(),
			ResponseID: t.responseID,
			Harness:    &ItemHarness{SubTurn: p.SubTurn},
		})
	case store.KindReasoningDelta:
		var p store.ReasoningDeltaPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		if t.reasoning < 0 {
			// No live deltas reached this translator — the run had no hub,
			// or the sub-turn produced no reasoning text at all. Open the
			// item now and emit the whole text as one delta.
			t.reasoning = t.addItem(OutputItem{Type: ItemReasoning})
			if p.Text != "" {
				t.delta(NotifyReasoningTextDelta, t.reasoning, p.Text)
			}
		}
		t.doneItem(t.reasoning)
		t.reasoning = -1
	case store.KindContentDelta:
		var p store.ContentDeltaPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		if t.message < 0 {
			t.message = t.addItem(OutputItem{Type: ItemMessage, Role: "assistant"})
			if p.Text != "" {
				t.delta(NotifyOutputTextDelta, t.message, p.Text)
			}
		}
		t.doneItem(t.message)
		t.message = -1
	case store.KindToolCall:
		var p store.ToolCallPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.CloseText()
		idx := t.addItem(OutputItem{
			Type: ItemFunctionCall, CallID: p.ID, Name: p.Name,
			Arguments: json.RawMessage("{}"),
		})
		if p.Arguments != "" {
			t.delta(NotifyFunctionCallArgsDelta, idx, p.Arguments)
		}
		t.doneItem(idx)
	case store.KindToolResult:
		var p store.ToolResultPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		out := TextPart(PartInputText, p.Content)
		if p.ImageURL != "" {
			out = append(out, ContentPart{Type: PartInputImage, ImageURL: p.ImageURL})
		}
		idx := t.addItem(OutputItem{
			Type: ItemFunctionCallOutput, CallID: p.ToolCallID, Name: p.Name,
			Output: out,
			Harness: &ItemHarness{
				IsError: p.IsError, Truncated: p.Truncated,
				ChildResponseID: p.ChildSessionID,
			},
		})
		t.doneItem(idx)
	case store.KindToolDenied:
		var p store.ToolDeniedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		// A denial is a function_call_output carrying harness.is_error,
		// because that is what the model is shown and the surface's item
		// union has no third outcome. The rule that refused it rides in the
		// harness block, so a parent can render a denial differently from a
		// tool that failed.
		idx := t.addItem(OutputItem{
			Type: ItemFunctionCallOutput, CallID: p.ToolCallID, Name: p.Name,
			Output:  TextPart(PartInputText, p.Content),
			Harness: &ItemHarness{IsError: true, Rule: p.Rule},
		})
		t.doneItem(idx)
	case store.KindToolStdout:
		var p store.ToolStdoutPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.emit(NotifyToolOutput, toolOutput{
			Type: NotifyToolOutput, SequenceNumber: t.next(),
			ResponseID: t.responseID, CallID: p.ToolCallID, Text: p.Text,
		})
	case store.KindUsage:
		var p store.UsagePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		one := usageFrom(p)
		t.addUsage(one)
		t.emit(NotifyUsage, usageEvent{
			Type: NotifyUsage, SequenceNumber: t.next(),
			ResponseID: t.responseID, SubTurn: p.SubTurn, Usage: one,
		})
	case store.KindTurnFinished:
		t.CloseText()
	case store.KindError:
		var p store.ErrorPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.CloseText()
		t.emit(NotifyResponseFailed, responseEnvelope{
			Type: NotifyResponseFailed, SequenceNumber: t.next(),
			Response: Response{
				ID: t.responseID, Object: "response", Model: t.model,
				Status: StatusFailed,
				Error:  &Error{Code: "internal", Message: p.Message},
			},
		})
	}
}

// setFirstMessageID records the id the create body's own input was sent
// under, echoed on the user message item the run's opening message becomes.
func (t *translator) SetFirstMessageID(id string) {
	t.idMu.Lock()
	t.firstMessageID = id
	t.idMu.Unlock()
}

// setMessageID records the id an appended input was sent under, keyed by the
// steer_message sequence number the append landed at. The steer_applied
// event that eventually folds it into the conversation names that same
// sequence, which is what ties the echo back to what the client sent.
func (t *translator) SetMessageID(seq int64, id string) {
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

// userInput emits a user message item. It carries its whole content on
// output_item.added and emits no deltas, because the text was never streamed
// — it was already complete when the loop recorded it.
func (t *translator) UserInput(text, source, messageID string) {
	if text == "" {
		return
	}
	t.CloseText()
	idx := t.addItem(OutputItem{
		Type: ItemMessage, Role: "user",
		Content: TextPart(PartInputText, text),
		Harness: &ItemHarness{Source: source, MessageID: messageID},
	})
	t.doneItem(idx)
}

// closeText closes whichever of the two streamed items is still open. Called
// before anything that must not appear inside them.
func (t *translator) CloseText() {
	if t.reasoning >= 0 {
		t.doneItem(t.reasoning)
		t.reasoning = -1
	}
	if t.message >= 0 {
		t.doneItem(t.message)
		t.message = -1
	}
}

// snapshot returns the response as assembled so far: the output items and
// the running usage total. Safe to call while the run is still going, which
// is what responses.get on an in-progress response needs.
func (t *translator) snapshot() ([]OutputItem, Usage) {
	t.itemsMu.Lock()
	defer t.itemsMu.Unlock()
	items := make([]OutputItem, len(t.items))
	copy(items, t.items)
	return items, t.usage
}

func (t *translator) addUsage(u Usage) {
	t.itemsMu.Lock()
	defer t.itemsMu.Unlock()
	t.usage.InputTokens += u.InputTokens
	t.usage.InputTokensDetails.CachedTokens += u.InputTokensDetails.CachedTokens
	t.usage.OutputTokens += u.OutputTokens
	t.usage.OutputTokensDetails.ReasoningTokens += u.OutputTokensDetails.ReasoningTokens
	t.usage.TotalTokens += u.TotalTokens
	if t.usage.Harness == nil {
		t.usage.Harness = &UsageX{}
	}
	if u.Harness != nil {
		t.usage.Harness.CostUSD += u.Harness.CostUSD
	}
}

// usageFrom maps one stored usage payload onto the Responses usage object.
// The harness records cache hit and miss separately, where this surface
// reports one input total with the cached part nested inside it, so the two
// are added back together — the inverse of the derivation
// internal/deepseek's usageFromResponses does on the way in.
//
// output_tokens is the whole output including reasoning, which is what the
// surface means by it and what the harness bills: reasoning_tokens is a
// breakdown of that total, not an addition to it.
func usageFrom(p store.UsagePayload) Usage {
	input := p.PromptCacheHitTokens + p.PromptCacheMissTokens
	return Usage{
		InputTokens:         input,
		InputTokensDetails:  InputDetails{CachedTokens: p.PromptCacheHitTokens},
		OutputTokens:        p.CompletionTokens,
		OutputTokensDetails: OutputDetail{ReasoningTokens: p.ReasoningTokens},
		TotalTokens:         input + p.CompletionTokens,
		Harness:             &UsageX{CostUSD: p.CostUSD},
	}
}

// Resource builds the `response` resource. The items and the usage come from
// this translator whether the run is still going or has finished, so
// responses.get on an in-progress response answers with what has happened so
// far rather than with nothing.
func (t *translator) Resource(v RunView) any {
	items, usage := t.snapshot()
	out := Response{
		ID: v.ID, Object: "response", Model: v.Model, Status: v.Status,
		CreatedAt: v.Created.Unix(),
	}
	if v.Err != nil {
		out.Error = &Error{Code: v.Err.Code, Message: v.Err.Message}
	}
	if v.WithItems {
		out.Output = items
	}
	if usage.TotalTokens > 0 {
		if usage.Harness != nil && v.SubTurns > 0 {
			h := *usage.Harness
			h.SubTurns = v.SubTurns
			usage.Harness = &h
		}
		out.Usage = &usage
	}
	out.Harness = &ResponseHarness{
		SessionID: v.SessionID,
		Reason:    v.Reason,
		Text:      v.Text,
		Result:    v.Result,
		SubTurns:  v.SubTurns,
		UpdatedAt: v.Updated.Format(time.RFC3339),

		UnappliedMessageIDs: v.UnappliedMessageIDs,
	}
	return out
}

// Result is the body responses.create, .cancel and .get answer with.
func (t *translator) Result(v RunView) any {
	return CreateResult{Response: t.Resource(v).(Response)}
}

// Completed emits response.completed, carrying the whole assembled response
// the way the surface's own terminal frame does.
func (t *translator) Completed(v RunView) {
	v.WithItems = true
	t.terminal(NotifyResponseCompleted, t.Resource(v).(Response))
}

// Failed emits response.failed. It carries the response object too, so a
// client that reads only terminal frames still gets the run's status and
// usage with the message.
func (t *translator) Failed(v RunView) {
	v.WithItems = false
	t.terminal(NotifyResponseFailed, t.Resource(v).(Response))
}
