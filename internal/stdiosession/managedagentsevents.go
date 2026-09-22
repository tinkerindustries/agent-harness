package stdiosession

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// sessions.events, ManagedAgents' one wire method for steering, interrupting
// and answering a pending custom tool call, plus the pending-custom-tool
// registry hostTools.Call blocks on under this dialect
// (docs/STDIO-MANAGED-AGENTS.md, "The seam"). Nothing here is reached by the
// Responses or Interactions dialects: Server.handle checks
// MethodSessionsEvents ahead of the generic method switch, and neither of
// the other two dialects' clients ever sends it.

// customToolResult is what a pending custom tool call resolves to.
type customToolResult struct {
	content tools.MCPContent
}

// registerCustomTool opens a wait for id, returning the channel
// callCustomTool blocks on. The KindToolCall event that names id is
// committed and streamed to the client (as agent.custom_tool_use) strictly
// before the tool-dispatch goroutine that will call this ever runs
// (internal/session/turn.go commits the whole sub-turn's tool_call batch
// before executeToolCalls starts it), so a fast client can answer before
// this side has registered the wait at all — a real race on a local pipe,
// not just a theoretical one. resolveCustomTool below stashes an early
// answer for exactly that reason; this reads it back out first.
func (s *Server) registerCustomTool(id string) chan customToolResult {
	s.customMu.Lock()
	if res, ok := s.customReady[id]; ok {
		delete(s.customReady, id)
		s.customMu.Unlock()
		ch := make(chan customToolResult, 1)
		ch <- res
		return ch
	}
	ch := make(chan customToolResult, 1)
	s.customPending[id] = ch
	s.customMu.Unlock()
	return ch
}

func (s *Server) unregisterCustomTool(id string) {
	s.customMu.Lock()
	delete(s.customPending, id)
	delete(s.customReady, id)
	s.customMu.Unlock()
}

// resolveCustomTool answers a pending wait, and reports whether id was
// accepted. An id already waiting is signalled immediately; one not yet
// waiting is stashed for registerCustomTool to pick up (the race described
// there), which is indistinguishable here from one this process never
// declared at all — a client-supplied id this harness never minted is
// vanishingly unlikely to collide with a real one, so this is a deliberate
// simplification rather than a full three-state registry, noted in this
// phase's report. A second answer for an id already resolved once is
// refused (docs/STDIO-MANAGED-AGENTS.md, "user.custom_tool_result").
func (s *Server) resolveCustomTool(id string, content tools.MCPContent) bool {
	s.customMu.Lock()
	defer s.customMu.Unlock()
	if ch, ok := s.customPending[id]; ok {
		delete(s.customPending, id)
		ch <- customToolResult{content: content}
		return true
	}
	if _, already := s.customReady[id]; already {
		return false
	}
	s.customReady[id] = customToolResult{content: content}
	return true
}

// callCustomTool is hostTools.async under ManagedAgents. It blocks the
// tool-dispatch goroutine exactly as the blocking harness.function_call path
// does on the other two dialects, but on this registry rather than on
// conn.Call — the answer arrives as an event on a different call
// (sessions.events) instead of a reply to a request this process sent.
//
// ctx ending unblocks it either way: the per-tool timeout
// (tools.Timeouts.HostTool, set for this dialect alone to a figure with no
// natural ceiling) resolves it as an ordinary tool error the sub-turn
// continues past, the same way any other tool's timeout does; the session's
// own context ending — user.interrupt, or the process shutting down —
// cancels it along with whatever else is running, ending the whole turn
// (docs/STDIO-MANAGED-AGENTS.md, "user.interrupt", "the one escape hatch out
// of a stuck requires_action wait").
func (s *Server) callCustomTool(ctx context.Context, id, name string, args json.RawMessage) (tools.MCPContent, error) {
	ch := s.registerCustomTool(id)
	defer s.unregisterCustomTool(id)
	select {
	case <-ctx.Done():
		return tools.MCPContent{}, ctx.Err()
	case res := <-ch:
		return res.content, nil
	}
}

// events is the sessions.events handler.
func (s *Server) events(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p maEventsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "sessions.events params: %v", err)
	}
	if p.SessionID == "" {
		return nil, errorf(CodeInvalidParams, "session_id is required")
	}
	it, ok := s.lookupBySession(p.SessionID)
	if !ok {
		return nil, errorf(CodeRunNotFound, s.m.NotFound, p.SessionID)
	}

	var (
		sawMessageOrInterrupt bool
		results               []maEventResultRow
	)
	for _, raw := range p.Events {
		var e maWireEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, errorf(CodeInvalidParams, "events: %v", err)
		}
		switch e.Type {
		case maEventUserMessage, maEventUserInterrupt:
			if sawMessageOrInterrupt {
				return nil, errorf(CodeInvalidParams, "a user.message and a user.interrupt cannot both be posted in one call; send the interrupt first, wait for it to be acknowledged, then the message")
			}
			sawMessageOrInterrupt = true
			var (
				row  maEventResultRow
				rerr *rpcError
			)
			if e.Type == maEventUserMessage {
				row, rerr = s.eventsMessage(ctx, it, e)
			} else {
				row, rerr = s.eventsInterrupt(it, e)
			}
			if rerr != nil {
				return nil, rerr
			}
			results = append(results, row)
		case maEventUserCustomToolResult:
			row, rerr := s.eventsCustomToolResult(it, e)
			if rerr != nil {
				return nil, rerr
			}
			results = append(results, row)
		case "system.message":
			return nil, errorf(CodeUnsupported, "system.message: this harness's own system prompt is frozen for the session's life")
		case "user.tool_result", "user.tool_confirmation", "user.define_outcome":
			return nil, errorf(CodeUnsupported, "%s is not built (docs/STDIO-MANAGED-AGENTS.md, \"Not built\")", e.Type)
		default:
			return nil, errorf(CodeUnsupported, "event type %q is not accepted; see docs/STDIO-MANAGED-AGENTS.md, \"sessions.events\"", e.Type)
		}
	}
	return maEventsResult{SessionID: p.SessionID, Results: results}, nil
}

// eventsMessage handles one posted user.message: a steer on a running
// session, or the next turn's start on an idle one
// (docs/STDIO-MANAGED-AGENTS.md, "user.message").
func (s *Server) eventsMessage(ctx context.Context, it *run, e maWireEvent) (maEventResultRow, *rpcError) {
	text, rerr := maContentText(e.Content)
	if rerr != nil {
		return maEventResultRow{}, rerr
	}
	messageID := ""
	if e.Harness != nil {
		messageID = e.Harness.MessageID
	}

	it.mu.Lock()
	status, tr, host := it.status, it.tr, it.host
	model, cwd := it.model, it.cwd
	it.mu.Unlock()

	if status == StatusInProgress {
		if ma, ok := tr.(*maTranslator); ok {
			if pending := ma.pendingCustomToolIDs(); len(pending) > 0 {
				return maEventResultRow{}, errorf(CodeRunNotRunning,
					"session %s is idle awaiting a custom tool result for %s; answer with user.custom_tool_result, or send user.interrupt to abandon it, before sending a message",
					it.sessionID, strings.Join(pending, ", "))
			}
		}
		res, rerr := s.appendInput(ctx, it, &AppendRequest{RunID: it.id, Prompt: text, MessageID: messageID})
		if rerr != nil {
			return maEventResultRow{}, rerr
		}
		row, _ := res.(maEventResultRow)
		return row, nil
	}

	if text == "" {
		return maEventResultRow{}, errorf(CodeInvalidParams, "%s", s.m.AppendNeedsInput)
	}
	s.mu.Lock()
	if s.running != nil {
		id := s.running.id
		s.mu.Unlock()
		return maEventResultRow{}, errorf(CodeInvalidRequest, s.m.AlreadyRunning, id)
	}
	s.mu.Unlock()
	if host == nil {
		return maEventResultRow{}, errorf(CodeInternalError, "session %s has no tool provider to continue with", it.sessionID)
	}

	res, rerr := s.beginRun(ctx, beginRunParams{
		model: model, sessionID: it.sessionID, cwd: cwd,
		prompt: text, prevRunID: it.id, resume: true,
		maxOutputTokens: maDefaultMaxOutputTokens,
		messageID:       messageID, host: host,
	})
	if rerr != nil {
		return maEventResultRow{}, rerr
	}
	turnID := ""
	if created, ok := res.(maCreateResult); ok && created.Session.Harness != nil {
		turnID = created.Session.Harness.TurnID
	}
	return maEventResultRow{Type: maEventUserMessage, Harness: &maEventHarness{TurnID: turnID, MessageID: messageID}}, nil
}

// eventsInterrupt handles one posted user.interrupt: cancels the running
// turn, or the one named by harness.turn_id — refusing a name that is not
// the turn actually running, since this process only ever has one in
// flight (docs/STDIO-MANAGED-AGENTS.md, "user.interrupt"). It is also the
// escape hatch out of a session idle awaiting a custom tool result:
// cancelRunItem cancels the run's context regardless of what its
// tool-dispatch goroutines are doing, unblocking callCustomTool's own
// ctx.Done() the same way it unblocks any other in-flight tool call.
func (s *Server) eventsInterrupt(it *run, e maWireEvent) (maEventResultRow, *rpcError) {
	wantTurn := ""
	if e.Harness != nil {
		wantTurn = e.Harness.TurnID
	}
	it.mu.Lock()
	curID, status := it.id, it.status
	it.mu.Unlock()
	if wantTurn != "" && wantTurn != curID {
		return maEventResultRow{}, errorf(CodeRunNotFound, "turn %q is not the one running now (%s is)", wantTurn, curID)
	}
	if status == StatusInProgress {
		s.cancelRunItem(it)
	}
	// Cancelling something already idle is not an error: it is the state
	// the caller asked for. Either way, an interrupt's own result entry
	// carries no seq — nothing was appended to the log.
	return maEventResultRow{Type: maEventUserInterrupt, Harness: &maEventHarness{TurnID: curID}}, nil
}

// eventsCustomToolResult handles one posted user.custom_tool_result,
// resolving the matching pending call in the registry callCustomTool
// blocks on.
func (s *Server) eventsCustomToolResult(it *run, e maWireEvent) (maEventResultRow, *rpcError) {
	if e.CustomToolUseID == "" {
		return maEventResultRow{}, errorf(CodeInvalidParams, "user.custom_tool_result needs custom_tool_use_id")
	}
	content := maContentFromEvent(e)
	if !s.resolveCustomTool(e.CustomToolUseID, content) {
		return maEventResultRow{}, errorf(CodeRunNotRunning, "%q is not a pending custom tool call for this session", e.CustomToolUseID)
	}
	it.mu.Lock()
	turnID := it.id
	it.mu.Unlock()
	return maEventResultRow{Type: maEventUserCustomToolResult, CustomToolUseID: e.CustomToolUseID, Harness: &maEventHarness{TurnID: turnID}}, nil
}

// maContentFromEvent reads a user.custom_tool_result's content array the
// same way ManagedAgents.CallContent reads a raw reply — the same content
// union, decoded directly here rather than round-tripped through JSON since
// the event is already decoded.
func maContentFromEvent(e maWireEvent) tools.MCPContent {
	out := tools.MCPContent{IsError: e.IsError}
	for _, c := range e.Content {
		switch c.Type {
		case "text":
			if out.Text != "" {
				out.Text += "\n"
			}
			out.Text += c.Text
		case "image":
			data, err := decodeBase64(c.Data)
			if err != nil {
				continue
			}
			out.Images = append(out.Images, tools.MCPImage{MIMEType: c.MIMEType, Data: data})
		}
	}
	return out
}
