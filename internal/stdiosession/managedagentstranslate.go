package stdiosession

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// maTranslator turns one session's committed event log, and the live text
// deltas published alongside it, into Anthropic's Managed Agents event
// vocabulary — session_id on every frame, harness.turn_id nested under it,
// the way interactionstranslate.go's iactTranslator does for Google's own
// vocabulary. Two things are genuinely new here, beyond a renaming of that
// pattern:
//
//   - agent.custom_tool_use and session.status_idle{requires_action}. A
//     client's declared tool renders as agent.custom_tool_use rather than
//     agent.tool_use, and this translator tracks which of a sub-turn's calls
//     are still pending one: every KindToolCall event for the reserved
//     "host" namespace adds its id to that set, and KindTurnFinished — which
//     always follows a sub-turn's whole batch of KindToolCall events,
//     before any of them have run (internal/session/turn.go) — is where the
//     set, if non-empty, is announced as the reason the session is about to
//     go idle. No result of any kind commits until every call in the batch
//     resolves (tooldispatch.go runs the batch to completion with one
//     wg.Wait() before appending any KindToolResult), so this is the whole
//     of what "every ordinary call already finished" needs: if a custom
//     call is still open when the batch's own KindToolCall events run out,
//     nothing else is going to happen until it answers.
//   - event_start/event_delta bracketing agent.message and agent.thinking.
//     Anthropic's real preview never streams agent.thinking's text
//     (docs/STDIO-MANAGED-AGENTS.md, deviation 11); this one does, the same
//     departure the other two dialects' own reasoning summary already
//     makes.
type maTranslator struct {
	turnID    string
	sessionID string
	model     string
	emit      func(method string, params any)

	nextID int

	messageEventID  string
	thinkingEventID string
	subTurn         int
	spanID          string

	// pendingCustom is every custom_tool_use_id opened in the sub-turn now
	// in flight that has not yet resolved. Reset at the start of each
	// sub-turn (KindTurnStarted); read by Server.events to decide whether a
	// posted user.message must be refused.
	pendingCustom []string

	stepsMu sync.Mutex
	events  []maEventOut
	usage   maUsage

	idMu           sync.Mutex
	firstMessageID string
	messageIDs     map[int64]string
}

func newMATranslator(turnID, sessionID, model string, emit func(method string, params any)) *maTranslator {
	return &maTranslator{
		turnID: turnID, sessionID: sessionID, model: model, emit: emit,
		messageIDs: map[int64]string{},
	}
}

func (t *maTranslator) mintID() string {
	t.nextID++
	return "evt_" + strconv.Itoa(t.nextID)
}

func (t *maTranslator) harness() *maStepHarness {
	return &maStepHarness{TurnID: t.turnID, SubTurn: t.subTurn}
}

func (t *maTranslator) record(ev maEventOut) {
	t.stepsMu.Lock()
	t.events = append(t.events, ev)
	t.stepsMu.Unlock()
}

// Created emits session.status_running, the first frame of every stream —
// this dialect's own opening bracket, matching interaction.created's role.
func (t *maTranslator) Created() {
	t.emit(notifyMASessionStatusRunning, maStatusRunning{SessionID: t.sessionID, Harness: t.harness()})
}

func (t *maTranslator) openMessage() {
	if t.messageEventID != "" {
		return
	}
	t.messageEventID = t.mintID()
	t.emit(notifyMAEventStart, maEventStartPayload{SessionID: t.sessionID, Event: maEventStart{Type: maEventKindMessage, ID: t.messageEventID}})
}

func (t *maTranslator) openThinking() {
	if t.thinkingEventID != "" {
		return
	}
	t.thinkingEventID = t.mintID()
	t.emit(notifyMAEventStart, maEventStartPayload{SessionID: t.sessionID, Event: maEventStart{Type: maEventKindThinking, ID: t.thinkingEventID}})
}

func (t *maTranslator) deltaText(id, text string) {
	if text == "" || id == "" {
		return
	}
	t.emit(notifyMAEventDelta, maEventDeltaPayload{SessionID: t.sessionID, EventID: id, Delta: maEventText{Text: text}})
}

func (t *maTranslator) closeMessage(text string) {
	if t.messageEventID == "" {
		return
	}
	id := t.messageEventID
	t.messageEventID = ""
	content := maTextContent(text)
	h := t.harness()
	t.record(maEventOut{Type: notifyMAAgentMessage, ID: id, Content: content, Harness: h})
	t.emit(notifyMAAgentMessage, maMessage{SessionID: t.sessionID, ID: id, Content: content, Harness: h})
}

func (t *maTranslator) closeThinking(text string) {
	if t.thinkingEventID == "" {
		return
	}
	id := t.thinkingEventID
	t.thinkingEventID = ""
	content := maTextContent(text)
	h := t.harness()
	t.record(maEventOut{Type: notifyMAAgentThinking, ID: id, Thinking: content, Harness: h})
	t.emit(notifyMAAgentThinking, maThinking{SessionID: t.sessionID, ID: id, Thinking: content, Harness: h})
}

// Live folds in one coalesced live delta.
func (t *maTranslator) Live(d hub.LiveDelta) {
	switch d.Channel {
	case hub.ChannelReasoning:
		t.openThinking()
		t.deltaText(t.thinkingEventID, d.Text)
	case hub.ChannelContent:
		t.openMessage()
		t.deltaText(t.messageEventID, d.Text)
	}
}

// Event translates one committed event.
func (t *maTranslator) Event(e store.Event) {
	switch e.Kind {
	case store.KindSessionStarted:
		var p store.SessionStartedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
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
		t.stepsMu.Lock()
		t.pendingCustom = nil
		t.stepsMu.Unlock()
		t.spanID = t.mintID()
		t.emit(notifyMASpanRequestStart, maSpanStart{SessionID: t.sessionID, ID: t.spanID, Model: t.model, Harness: t.harness()})
		t.emit(notifyMASessionStatusRunning, maStatusRunning{SessionID: t.sessionID, Harness: t.harness()})
	case store.KindReasoningDelta:
		var p store.ReasoningDeltaPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		if t.thinkingEventID == "" && p.Text != "" {
			t.openThinking()
			t.deltaText(t.thinkingEventID, p.Text)
		}
		t.closeThinking(p.Text)
	case store.KindContentDelta:
		var p store.ContentDeltaPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		if t.messageEventID == "" && p.Text != "" {
			t.openMessage()
			t.deltaText(t.messageEventID, p.Text)
		}
		t.closeMessage(p.Text)
	case store.KindToolCall:
		var p store.ToolCallPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.CloseText()
		t.emitToolCall(p)
	case store.KindToolResult:
		var p store.ToolResultPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.emitToolResult(p.ToolCallID, p.Content, p.ImageURL, p.IsError, p.Truncated, p.ChildSessionID, "")
	case store.KindToolDenied:
		var p store.ToolDeniedPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.emitToolResult(p.ToolCallID, p.Content, "", true, false, "", p.Rule)
	case store.KindToolStdout:
		var p store.ToolStdoutPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.emit(notifyMAToolOutput, maToolOutput{SessionID: t.sessionID, CallID: p.ToolCallID, Text: p.Text})
	case store.KindUsage:
		var p store.UsagePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		one := maUsageFrom(p)
		t.addUsage(one)
		t.emit(notifyMASessionUsage, maUsageEvent{SessionID: t.sessionID, Usage: one, Harness: t.harness()})
	case store.KindTurnFinished:
		t.CloseText()
		if t.spanID != "" {
			t.emit(notifyMASpanRequestEnd, maSpanEnd{SessionID: t.sessionID, ID: t.spanID, Harness: t.harness()})
			t.spanID = ""
		}
		pending := t.pendingCustomToolIDs()
		if len(pending) > 0 {
			t.emit(notifyMASessionStatusIdle, maStatusIdle{
				SessionID:  t.sessionID,
				StopReason: maStopReason{Type: "requires_action", EventIDs: pending},
				Harness:    t.harness(),
			})
		}
	case store.KindError:
		var p store.ErrorPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		t.CloseText()
		t.emit(notifyMASessionError, maErrorEvent{SessionID: t.sessionID, Error: maError{Type: "internal", Message: p.Message}, Harness: t.harness()})
	}
}

// emitToolCall renders one tool call as agent.tool_use, agent.mcp_tool_use
// or agent.custom_tool_use, by which namespace its qualified name belongs
// to (docs/STDIO-MANAGED-AGENTS.md, "agent.tool_use vs agent.mcp_tool_use vs
// agent.custom_tool_use"). evaluated_permission is left unset rather than
// guessed "allow": permission is checked inside Execute, after this event
// already committed, so a call about to be denied cannot be told apart from
// one that will succeed at this point — the eventual agent.tool_result
// carries is_error and harness.rule instead.
func (t *maTranslator) emitToolCall(p store.ToolCallPayload) {
	server, isMCP := tools.MCPServerOf(p.Name)
	input := toolArguments(p.Arguments)
	if len(input) == 0 {
		input = toolArguments("{}")
	}
	h := t.harness()
	switch {
	case isMCP && server == HostServerName:
		name := strings.TrimPrefix(p.Name, tools.MCPToolPrefix+HostServerName+"__")
		t.record(maEventOut{Type: notifyMAAgentCustomToolUse, ID: p.ID, Name: name, Input: input, Harness: h})
		t.emit(notifyMAAgentCustomToolUse, maCustomToolUse{SessionID: t.sessionID, ID: p.ID, Name: name, Input: input, Harness: h})
		t.stepsMu.Lock()
		t.pendingCustom = append(t.pendingCustom, p.ID)
		t.stepsMu.Unlock()
	case isMCP:
		name := strings.TrimPrefix(p.Name, tools.MCPToolPrefix+server+"__")
		t.record(maEventOut{Type: notifyMAAgentMCPToolUse, ID: p.ID, Name: name, Input: input, Harness: h})
		t.emit(notifyMAAgentMCPToolUse, maToolUse{SessionID: t.sessionID, ID: p.ID, Name: name, Input: input, Harness: h})
	default:
		t.record(maEventOut{Type: notifyMAAgentToolUse, ID: p.ID, Name: p.Name, Input: input, Harness: h})
		t.emit(notifyMAAgentToolUse, maToolUse{SessionID: t.sessionID, ID: p.ID, Name: p.Name, Input: input, Harness: h})
	}
}

func (t *maTranslator) emitToolResult(callID, content, imageURL string, isError, truncated bool, childSessionID, rule string) {
	result := maTextContent(content)
	if imageURL != "" {
		if mime, data, ok := iactDecodeDataURI(imageURL); ok {
			result = append(result, maContent{Type: "image", MIMEType: mime, Data: data})
		}
	}
	h := t.harness()
	h.Truncated = truncated
	h.ChildTurnID = childSessionID
	h.Rule = rule
	id := t.mintID()
	t.record(maEventOut{Type: notifyMAAgentToolResult, ID: id, ToolUseID: callID, Content: result, IsError: isError, Harness: h})
	t.emit(notifyMAAgentToolResult, maToolResult{SessionID: t.sessionID, ID: id, ToolUseID: callID, Content: result, IsError: isError, Harness: h})

	t.stepsMu.Lock()
	for i, pid := range t.pendingCustom {
		if pid == callID {
			t.pendingCustom = append(t.pendingCustom[:i], t.pendingCustom[i+1:]...)
			break
		}
	}
	t.stepsMu.Unlock()
}

// pendingCustomToolIDs is what Server.events consults to decide whether a
// posted user.message must be refused with -32002 (a custom tool result is
// still pending) and to validate a user.custom_tool_result names an id this
// translator actually still considers open.
func (t *maTranslator) pendingCustomToolIDs() []string {
	t.stepsMu.Lock()
	defer t.stepsMu.Unlock()
	if len(t.pendingCustom) == 0 {
		return nil
	}
	out := append([]string(nil), t.pendingCustom...)
	sort.Strings(out)
	return out
}

func (t *maTranslator) SetFirstMessageID(id string) {
	t.idMu.Lock()
	t.firstMessageID = id
	t.idMu.Unlock()
}

func (t *maTranslator) SetMessageID(seq int64, id string) {
	t.idMu.Lock()
	t.messageIDs[seq] = id
	t.idMu.Unlock()
}

func (t *maTranslator) messageIDFor(seq int64) string {
	t.idMu.Lock()
	defer t.idMu.Unlock()
	if seq == 0 {
		return t.firstMessageID
	}
	return t.messageIDs[seq]
}

// UserInput records the run's opening message or a steer into the assembled
// document. It is not re-emitted live: this vocabulary has no notification
// for the client's own words coming back to it, unlike the other two
// dialects' user_input/message item, so a client that wants a transcript
// entry for it reads sessions.get.
func (t *maTranslator) UserInput(text, source, messageID string) {
	if text == "" {
		return
	}
	t.CloseText()
	h := t.harness()
	h.Source = source
	h.MessageID = messageID
	t.record(maEventOut{Type: maEventUserMessage, Content: maTextContent(text), Harness: h})
}

// CloseText closes whichever streamed spans are still open, with no further
// text than what already streamed live — the same defensive-only path the
// other two dialects' CloseText is: by the time anything else calls it, the
// authoritative KindReasoningDelta/KindContentDelta path has already closed
// both, in the ordinary case.
func (t *maTranslator) CloseText() {
	t.closeThinking("")
	t.closeMessage("")
}

func (t *maTranslator) snapshot() ([]maEventOut, maUsage) {
	t.stepsMu.Lock()
	defer t.stepsMu.Unlock()
	events := make([]maEventOut, len(t.events))
	copy(events, t.events)
	return events, t.usage
}

func (t *maTranslator) addUsage(u maUsage) {
	t.stepsMu.Lock()
	defer t.stepsMu.Unlock()
	t.usage.InputTokens += u.InputTokens
	t.usage.CacheReadInputTokens += u.CacheReadInputTokens
	t.usage.OutputTokens += u.OutputTokens
	t.usage.TotalTokens += u.TotalTokens
	if t.usage.Harness == nil {
		t.usage.Harness = &maUsageX{}
	}
	if u.Harness != nil {
		t.usage.Harness.CostUSD += u.Harness.CostUSD
	}
}

// maUsageFrom maps one stored usage payload onto this dialect's usage
// object, the same hit/miss-to-total mapping iactUsageFrom makes for
// Google's own shape.
func maUsageFrom(p store.UsagePayload) maUsage {
	return maUsage{
		InputTokens:          p.PromptCacheHitTokens + p.PromptCacheMissTokens,
		CacheReadInputTokens: p.PromptCacheHitTokens,
		OutputTokens:         p.CompletionTokens,
		TotalTokens:          p.PromptCacheHitTokens + p.PromptCacheMissTokens + p.CompletionTokens,
		Harness:              &maUsageX{CostUSD: p.CostUSD},
	}
}

// maStatusFor maps a run's neutral status onto this dialect's session
// status vocabulary. A run this process still calls "failed" or
// "cancelled" is idle here too (docs/STDIO-MANAGED-AGENTS.md, "Session
// statuses" — idle "includes ... one that failed"); this build does not yet
// distinguish an unrecoverable failure from an ordinary one; "terminated" is
// reachable only through sessions.delete's own bookkeeping in Server, never
// rendered by this translator (a documented simplification — see this
// phase's report).
func maStatusFor(status string) string {
	if status == StatusInProgress {
		return "running"
	}
	return "idle"
}

// maStopReasonType maps the run's finish reason onto stop_reason.type.
func maStopReasonType(reason string) string {
	switch reason {
	case "cancelled":
		return "user_interrupt"
	case "complete_rejected":
		return "complete_rejected"
	default:
		return "end_turn"
	}
}

// Resource builds this dialect's Session resource. Events and usage come
// from this translator whether the run is still going or has finished, so
// sessions.get on an in-progress session answers with what has happened so
// far.
func (t *maTranslator) Resource(v RunView) any {
	events, usage := t.snapshot()
	pending := t.pendingCustomToolIDs()

	status := maStatusFor(v.Status)
	reason := v.Reason
	if v.Status == StatusInProgress && len(pending) > 0 {
		status = "idle"
		reason = "requires_action"
	}

	out := maSession{
		ID: v.SessionID, Status: status,
		CreatedAt: v.Created.Format(time.RFC3339),
		UpdatedAt: v.Updated.Format(time.RFC3339),
		Agent:     &maAgentEcho{Model: maModelEcho{ID: v.Model}},
	}
	if v.Err != nil {
		out.Error = &maError{Type: v.Err.Code, Message: v.Err.Message}
	}
	if v.WithItems {
		out.Events = events
	}
	if usage.TotalTokens > 0 {
		if usage.Harness != nil && v.SubTurns > 0 {
			h := *usage.Harness
			h.SubTurns = v.SubTurns
			usage.Harness = &h
		}
		out.Usage = &usage
	}
	out.Harness = &maSessionHarness{
		TurnID: t.turnID, Reason: reason, Text: v.Text, Result: v.Result,
		SubTurns: v.SubTurns, UnappliedMessageIDs: v.UnappliedMessageIDs,
	}
	return out
}

// Result is the body sessions.create and sessions.get answer with.
func (t *maTranslator) Result(v RunView) any {
	return maCreateResult{Session: t.Resource(v).(maSession)}
}

// Completed emits session.status_idle, ending the turn.
func (t *maTranslator) Completed(v RunView) {
	reason := v.Reason
	if v.Status == StatusFailed && reason == "" {
		// status_idle has no status field, so this is the only place a
		// client reading terminal frames alone can tell a failure.
		reason = "failed"
	}
	t.emit(notifyMASessionStatusIdle, maStatusIdle{
		SessionID:  t.sessionID,
		StopReason: maStopReason{Type: maStopReasonType(v.Reason)},
		Harness:    &maStepHarness{TurnID: t.turnID, Reason: reason},
	})
}

// Failed emits session.error for a run the loop could not finish.
// session.status_idle still follows it (Completed runs after this, from
// Server.complete), so a client reading only terminal frames is not left
// waiting.
func (t *maTranslator) Failed(v RunView) {
	msg := ""
	if v.Err != nil {
		msg = v.Err.Message
	}
	t.emit(notifyMASessionError, maErrorEvent{
		SessionID: t.sessionID,
		Error:     maError{Type: "internal", Message: msg},
		Harness:   &maStepHarness{TurnID: t.turnID},
	})
}
