package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/fold"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// TestFoldToAnthropicRequestReplaysRawBlocksVerbatim is the end-to-end check
// for the raw-block replay path: an event log built the way
// internal/session/turn.go actually commits one — a reasoning_delta event
// carrying provider_blocks, tool_call and tool_result events, across two
// sub-turns — folds through internal/fold.Fold into wire.Items, and
// requestFromIntent turns the raw-blocks sub-turn's assistant message back
// into exactly the bytes the API originally sent, untouched by the
// text/tool_use items fold.go also emits for the same sub-turn.
func TestFoldToAnthropicRequestReplaysRawBlocksVerbatim(t *testing.T) {
	sess := store.Session{
		ID:           "sess-1",
		Model:        ModelSonnet55,
		SystemPrompt: "you are a coding agent",
	}

	rawTurn1 := json.RawMessage(`[{"type":"thinking","thinking":"I should list files.","signature":"sig-1"},{"type":"tool_use","id":"call_1","name":"List","input":{"path":"."}}]`)

	var seq int64
	ev := func(kind store.EventKind, payload any) store.Event {
		seq++
		p, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		return store.Event{SessionID: sess.ID, Seq: seq, Kind: kind, Payload: p}
	}

	events := []store.Event{
		ev(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "list the files"}),

		ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 1}),
		ev(store.KindReasoningDelta, store.ReasoningDeltaPayload{Text: "I should list files.", ThoughtSignature: "sig-1", ProviderBlocks: rawTurn1}),
		ev(store.KindToolCall, store.ToolCallPayload{Index: 0, ID: "call_1", Name: "List", Arguments: `{"path":"."}`}),
		ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "tool_use"}),
		ev(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_1", Name: "List", Content: "a.go\nb.go"}),

		ev(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: 2}),
		ev(store.KindContentDelta, store.ContentDeltaPayload{Text: "Found a.go and b.go."}),
		ev(store.KindTurnFinished, store.TurnFinishedPayload{FinishReason: "end_turn"}),
	}

	items, err := fold.Fold(sess, events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}

	req := requestFromIntent(wire.ChatIntent{Model: sess.Model, Items: items, Effort: wire.EffortHigh})

	// user, assistant (raw turn 1), user tool_result, assistant (turn 2,
	// reconstructed since it carries no raw blocks).
	if len(req.Messages) != 4 {
		t.Fatalf("Messages = %d, want 4: %+v", len(req.Messages), req.Messages)
	}
	if req.Messages[1].Role != wire.RoleAssistant {
		t.Fatalf("Messages[1].Role = %q", req.Messages[1].Role)
	}
	if string(req.Messages[1].Content) != string(rawTurn1) {
		t.Errorf("Messages[1].Content = %s,\nwant verbatim %s", req.Messages[1].Content, rawTurn1)
	}

	if req.Messages[3].Role != wire.RoleAssistant {
		t.Fatalf("Messages[3].Role = %q", req.Messages[3].Role)
	}
	var turn2Blocks []ContentBlock
	if err := json.Unmarshal(req.Messages[3].Content, &turn2Blocks); err != nil {
		t.Fatalf("decode turn 2 content: %v", err)
	}
	if len(turn2Blocks) != 1 || turn2Blocks[0].Type != "text" || turn2Blocks[0].Text != "Found a.go and b.go." {
		t.Errorf("turn 2 blocks = %+v", turn2Blocks)
	}
}

// openServerToolBlocks is a sub-turn whose response called a dynamic-filtering
// web search — a server_tool_use named code_execution — in parallel with a
// client tool, so it ended stop_reason "tool_use" with the server call still
// open (docs/ANTHROPIC-INTEGRATION.md, "Server tools left open across a tool
// round").
const openServerToolBlocks = `[{"type":"thinking","thinking":"Search and list.","signature":"sig-1"},` +
	`{"type":"server_tool_use","id":"srvtoolu_017316q55G8RgWFKLYMthRtU","name":"code_execution","input":{"code":"search()"}},` +
	`{"type":"tool_use","id":"call_1","name":"List","input":{"path":"."}}]`

// closedServerToolBlocks is the response to the request after that tool
// round: it opens with the open call's result and answers in text.
const closedServerToolBlocks = `[{"type":"code_execution_tool_result","tool_use_id":"srvtoolu_017316q55G8RgWFKLYMthRtU","content":{"type":"code_execution_result","stdout":"ok","stderr":"","return_code":0,"content":[]}},` +
	`{"type":"text","text":"Found it."}]`

// eventLog builds a session's events with sequential seqs, the way
// TestFoldToAnthropicRequestReplaysRawBlocksVerbatim does inline.
type eventLog struct {
	t      *testing.T
	events []store.Event
}

func (l *eventLog) add(kind store.EventKind, payload any) {
	l.t.Helper()
	p, err := json.Marshal(payload)
	if err != nil {
		l.t.Fatal(err)
	}
	l.events = append(l.events, store.Event{SessionID: "sess-1", Seq: int64(len(l.events) + 1), Kind: kind, Payload: p})
}

func (l *eventLog) subTurn(n int, blocks string, calls ...store.ToolCallPayload) {
	l.add(store.KindTurnStarted, store.TurnStartedPayload{SubTurn: n})
	l.add(store.KindReasoningDelta, store.ReasoningDeltaPayload{ProviderBlocks: json.RawMessage(blocks)})
	for _, c := range calls {
		l.add(store.KindToolCall, c)
	}
	reason := "end_turn"
	if len(calls) > 0 {
		reason = "tool_use"
	}
	l.add(store.KindTurnFinished, store.TurnFinishedPayload{SubTurn: n, FinishReason: reason})
}

// request folds the log as it stands and returns the request's messages,
// each as the bytes the body carries.
func (l *eventLog) request() []json.RawMessage {
	l.t.Helper()
	sess := store.Session{ID: "sess-1", Model: ModelSonnet55, SystemPrompt: "you are a coding agent"}
	items, err := fold.Fold(sess, l.events)
	if err != nil {
		l.t.Fatalf("fold: %v", err)
	}
	req := requestFromIntent(wire.ChatIntent{Model: sess.Model, Items: items, Effort: wire.EffortHigh})
	out := make([]json.RawMessage, len(req.Messages))
	for i, m := range req.Messages {
		b, err := json.Marshal(m)
		if err != nil {
			l.t.Fatal(err)
		}
		out[i] = b
	}
	return out
}

// stripBreakpoint drops the cache_control requestFromIntent places on the
// last message of every request, which is the one thing about an earlier
// message that legitimately differs between two consecutive requests.
func stripBreakpoint(m json.RawMessage) string {
	return strings.ReplaceAll(string(m), `,"cache_control":{"type":"ephemeral"}`, "")
}

func requireToolResultsOnly(t *testing.T, m json.RawMessage) {
	t.Helper()
	var msg struct {
		Role    string         `json:"role"`
		Content []ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(m, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Role != wire.RoleUser || len(msg.Content) == 0 {
		t.Fatalf("message = %s, want a user message of tool results", m)
	}
	for _, b := range msg.Content {
		if b.Type != "tool_result" {
			t.Fatalf("message = %s, want tool_result blocks only", m)
		}
	}
}

// TestOpenServerToolHoldsLaterMessagesBack replays event logs through the
// fold and the request builder where something other than tool results
// follows a sub-turn that left a server tool open — the shape the API
// rejects with "`code_execution` tool use with id `srvtoolu_…` was found
// without a corresponding `code_execution_tool_result` block", on every
// request that replays it. Each case is a log already written in that shape:
// a steer, a reminder in either role, and the new message a resumed run
// appends after an interrupted one (whose unfinished tool call the fold
// closes with an interrupted stand-in). The request built while the call is
// still open ends on the tool results; the one built once the closing
// response is in the log carries the held message after it, and repeats the
// first request's messages unchanged ahead of it.
func TestOpenServerToolHoldsLaterMessagesBack(t *testing.T) {
	cases := []struct {
		name  string
		after func(l *eventLog)
		text  string
		role  string
	}{
		{"steer", func(l *eventLog) {
			l.add(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_1", Name: "List", Content: "a.go"})
			l.add(store.KindSteerApplied, store.SteerAppliedPayload{SourceSeq: 1, Text: "also check b.go", SubTurn: 2})
		}, "also check b.go", wire.RoleUser},
		{"system reminder", func(l *eventLog) {
			l.add(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_1", Name: "List", Content: "a.go"})
			l.add(store.KindSteerApplied, store.SteerAppliedPayload{Text: "remember the plan", SubTurn: 2, Role: wire.RoleSystem})
		}, "remember the plan", wire.RoleSystem},
		{"user reminder", func(l *eventLog) {
			l.add(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_1", Name: "List", Content: "a.go"})
			l.add(store.KindSteerApplied, store.SteerAppliedPayload{Text: "remember the plan", SubTurn: 2})
		}, "remember the plan", wire.RoleUser},
		{"interrupted run resumed with a new message", func(l *eventLog) {
			// No tool_result: the run was stopped while List ran, and the
			// fold closes the call with its interrupted stand-in.
			l.add(store.KindRunFinished, store.RunFinishedPayload{Reason: "cancelled"})
			l.add(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "try again"})
		}, "try again", wire.RoleUser},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &eventLog{t: t}
			l.add(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "find it"})
			l.subTurn(1, openServerToolBlocks, store.ToolCallPayload{ID: "call_1", Name: "List", Arguments: `{"path":"."}`})
			tc.after(l)

			open := l.request()
			// user, assistant (open), user tool results — and nothing else.
			if len(open) != 3 {
				t.Fatalf("request while the call is open has %d messages, want 3: %s", len(open), open)
			}
			requireToolResultsOnly(t, open[2])
			for i, m := range open {
				if strings.Contains(string(m), tc.text) {
					t.Fatalf("message %d carries %q behind the open server tool: %s", i, tc.text, m)
				}
			}

			l.subTurn(2, closedServerToolBlocks)
			closed := l.request()
			if len(closed) != 5 {
				t.Fatalf("request after the call closed has %d messages, want 5: %s", len(closed), closed)
			}
			for i := range open {
				if stripBreakpoint(open[i]) != stripBreakpoint(closed[i]) {
					t.Fatalf("message %d changed between consecutive requests:\nfirst  %s\nsecond %s", i, open[i], closed[i])
				}
			}
			if got := string(closed[3]); !strings.Contains(got, `"role":"assistant"`) || !strings.Contains(got, "code_execution_tool_result") {
				t.Fatalf("message 3 = %s, want the closing assistant turn", got)
			}
			var held Message
			if err := json.Unmarshal(closed[4], &held); err != nil {
				t.Fatal(err)
			}
			if held.Role != tc.role || !strings.Contains(string(held.Content), tc.text) {
				t.Fatalf("message 4 = %s, want the held %s message %q", closed[4], tc.role, tc.text)
			}
		})
	}
}

// TestOpenServerToolHeldAcrossAnotherToolRound covers the closing response
// itself leaving a new server tool open beside another client call: the
// held steer waits again, behind the second tool round, and every request
// still extends the one before it.
func TestOpenServerToolHeldAcrossAnotherToolRound(t *testing.T) {
	reopen := strings.Replace(openServerToolBlocks, "srvtoolu_017316q55G8RgWFKLYMthRtU", "srvtoolu_second", 1)
	reopen = strings.Replace(reopen, "call_1", "call_2", 1)
	closesAndReopens := `[` + strings.TrimSuffix(strings.TrimPrefix(closedServerToolBlocks, "["), "]") + `,` +
		strings.TrimPrefix(reopen, "[")

	l := &eventLog{t: t}
	l.add(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "find it"})
	l.subTurn(1, openServerToolBlocks, store.ToolCallPayload{ID: "call_1", Name: "List", Arguments: `{"path":"."}`})
	l.add(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_1", Name: "List", Content: "a.go"})
	l.add(store.KindSteerApplied, store.SteerAppliedPayload{SourceSeq: 1, Text: "also check b.go", SubTurn: 2})
	first := l.request()

	l.subTurn(2, closesAndReopens, store.ToolCallPayload{ID: "call_2", Name: "List", Arguments: `{"path":"."}`})
	l.add(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_2", Name: "List", Content: "b.go"})
	second := l.request()

	l.subTurn(3, strings.Replace(closedServerToolBlocks, "srvtoolu_017316q55G8RgWFKLYMthRtU", "srvtoolu_second", 1))
	third := l.request()

	for _, pair := range [][2][]json.RawMessage{{first, second}, {second, third}} {
		for i := range pair[0] {
			if stripBreakpoint(pair[0][i]) != stripBreakpoint(pair[1][i]) {
				t.Fatalf("message %d changed between consecutive requests:\n%s\n%s", i, pair[0][i], pair[1][i])
			}
		}
	}
	if len(second) != 5 {
		t.Fatalf("second request has %d messages, want 5", len(second))
	}
	requireToolResultsOnly(t, second[4])
	if len(third) != 7 || !strings.Contains(string(third[6]), "also check b.go") {
		t.Fatalf("third request = %s, want the steer last", third)
	}
}

// TestClosedServerToolLeavesMessagesInPlace is the ordinary case the hold
// must not touch: a server call whose result arrived in the same response is
// finished, and a steer after that sub-turn's tool results stays where the
// log put it.
func TestClosedServerToolLeavesMessagesInPlace(t *testing.T) {
	paired := `[{"type":"server_tool_use","id":"srvtoolu_a","name":"web_search","input":{"query":"q"}},` +
		`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_a","content":[]},` +
		`{"type":"tool_use","id":"call_1","name":"List","input":{}}]`
	l := &eventLog{t: t}
	l.add(store.KindSessionStarted, store.SessionStartedPayload{OpeningMessage: "find it"})
	l.subTurn(1, paired, store.ToolCallPayload{ID: "call_1", Name: "List", Arguments: `{}`})
	l.add(store.KindToolResult, store.ToolResultPayload{ToolCallID: "call_1", Name: "List", Content: "a.go"})
	l.add(store.KindSteerApplied, store.SteerAppliedPayload{SourceSeq: 1, Text: "also check b.go", SubTurn: 2})
	msgs := l.request()
	if len(msgs) != 4 || !strings.Contains(string(msgs[3]), "also check b.go") {
		t.Fatalf("messages = %s, want the steer last", msgs)
	}
}

func TestLeavesServerToolOpen(t *testing.T) {
	cases := []struct {
		name   string
		blocks string
		want   bool
	}{
		{"open beside a client call", openServerToolBlocks, true},
		{"paired in the same response", `[{"type":"server_tool_use","id":"srvtoolu_a","name":"web_fetch","input":{}},{"type":"web_fetch_tool_result","tool_use_id":"srvtoolu_a","content":{}},{"type":"tool_use","id":"c","name":"List","input":{}}]`, false},
		{"nested dynamic-filtering calls, outer still open", `[{"type":"server_tool_use","id":"srvtoolu_outer","name":"code_execution","input":{}},{"type":"server_tool_use","id":"srvtoolu_inner","name":"web_search","input":{},"caller":{"type":"code_execution_20260120","tool_id":"srvtoolu_outer"}},{"type":"web_search_tool_result","tool_use_id":"srvtoolu_inner","content":[],"caller":{"type":"code_execution_20260120","tool_id":"srvtoolu_outer"}},{"type":"tool_use","id":"c","name":"List","input":{}}]`, true},
		{"unpaired with no client call", `[{"type":"server_tool_use","id":"srvtoolu_a","name":"web_search","input":{}}]`, false},
		{"mcp_tool_use open", `[{"type":"mcp_tool_use","id":"mcptoolu_a","name":"x","server_name":"s","input":{}},{"type":"tool_use","id":"c","name":"List","input":{}}]`, true},
		{"no server tools", `[{"type":"tool_use","id":"c","name":"List","input":{}}]`, false},
		{"empty", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := leavesServerToolOpen(json.RawMessage(tc.blocks)); got != tc.want {
				t.Errorf("leavesServerToolOpen = %v, want %v", got, tc.want)
			}
		})
	}
}
