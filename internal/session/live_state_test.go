package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// TestTaskCallsPersistPlanToSessionRow drives a run whose first sub-turn
// calls TaskCreate (with a two-item plan) alongside a Bash call, whose
// second sub-turn patches one task with TaskUpdate, and whose third finishes
// with Complete. It asserts the session row carries the plan — the current
// full checklist, minted ids included and the patch applied, not the raw
// arguments of any one call — the filtered recent-tool-call roll, and the
// model's own summary: the data the session list renders the in-flight card
// and the finished table's subtitle from.
func TestTaskCallsPersistPlanToSessionRow(t *testing.T) {
	var streamCall int32Counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("n/a")}, FinishReason: wire.FinishStop}},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := streamCall.next()
		w.Header().Set("Content-Type", "text/event-stream")
		switch idx {
		case 0:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role: "assistant",
					ToolCalls: []wire.ToolCallDelta{
						{Index: 0, ID: "call_00_create", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskCreate", Arguments: `{"tasks":[{"subject":"Read the spec","description":"Read it","activeForm":"Reading"},{"subject":"Wire it up","description":"Wire it","activeForm":"Wiring"}]}`}},
						{Index: 1, ID: "call_01_bash", Type: "function", Function: wire.ToolCallFuncDelta{Name: "Bash", Arguments: `{"command":"echo hi"}`}},
					},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheMissTokens: 300, CompletionTokens: 5},
			})
		case 1:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_10_update", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskUpdate", Arguments: `{"taskId":"1","status":"completed"}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		default:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_20_complete", Type: "function", Function: wire.ToolCallFuncDelta{Name: "Complete", Arguments: `{"summary":"wired it up","status":"done"}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "wire it up",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("unexpected status: %s", res.Status)
	}

	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	// The plan is what the handlers built — minted ids included, with the
	// TaskUpdate patch applied — not the raw arguments of any one call.
	var todos []store.StatusTodo
	if err := json.Unmarshal([]byte(sess.Plan), &todos); err != nil {
		t.Fatalf("decode stored plan %q: %v", sess.Plan, err)
	}
	if len(todos) != 2 || todos[0].TaskID != "1" || todos[1].TaskID != "2" {
		t.Fatalf("expected two tasks with ids 1 and 2, got %+v", todos)
	}
	if todos[0].Subject != "Read the spec" || todos[0].Status != "completed" || todos[0].ActiveForm != "Reading" {
		t.Fatalf("unexpected first task: %+v", todos[0])
	}
	if todos[1].Subject != "Wire it up" || todos[1].Status != "pending" || todos[1].ActiveForm != "Wiring" {
		t.Fatalf("unexpected second task: %+v", todos[1])
	}

	// The roll keeps the Bash call and the finishing Complete call, and
	// drops TaskCreate/TaskUpdate, whose effect the plan panel already shows.
	if len(sess.RecentToolCalls) != 2 || sess.RecentToolCalls[0].Name != "Bash" || sess.RecentToolCalls[1].Name != "Complete" {
		t.Fatalf("unexpected recent tool call roll: %+v", sess.RecentToolCalls)
	}

	if sess.Summary != "wired it up" {
		t.Fatalf("expected the Complete summary on the row, got %q", sess.Summary)
	}
}

// TestTaskReadsDoNotWritePlan drives a run whose only tool calls are the
// non-mutating task tools — TaskList and TaskGet — and asserts the session
// row's plan column stays empty: persistTaskState only writes when the
// sub-turn carries a TaskCreate or TaskUpdate. The read calls themselves do
// land in the recent-tool-call roll like any other tool.
func TestTaskReadsDoNotWritePlan(t *testing.T) {
	var streamCall int32Counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("n/a")}, FinishReason: wire.FinishStop}},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := streamCall.next()
		w.Header().Set("Content-Type", "text/event-stream")
		switch idx {
		case 0:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role: "assistant",
					ToolCalls: []wire.ToolCallDelta{
						{Index: 0, ID: "call_00_list", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}},
						{Index: 1, ID: "call_01_get", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskGet", Arguments: `{"taskId":"1"}`}},
					},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheMissTokens: 300, CompletionTokens: 5},
			})
		default:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_10_complete", Type: "function", Function: wire.ToolCallFuncDelta{Name: "Complete", Arguments: `{"summary":"nothing to do","status":"done"}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "just read the plan",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("unexpected status: %s", res.Status)
	}

	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	// No TaskCreate/TaskUpdate ever ran, so persistTaskState never wrote the
	// plan column.
	if sess.Plan != "" {
		t.Fatalf("expected no plan write for read-only task calls, got %q", sess.Plan)
	}

	// The read calls roll like any other tool, alongside the Complete call.
	if len(sess.RecentToolCalls) != 3 ||
		sess.RecentToolCalls[0].Name != "TaskList" ||
		sess.RecentToolCalls[1].Name != "TaskGet" ||
		sess.RecentToolCalls[2].Name != "Complete" {
		t.Fatalf("unexpected recent tool call roll: %+v", sess.RecentToolCalls)
	}
}

// TestSubTurnPublishesFreshPlanAndRecentToolCalls proves every session_state
// event runSubTurn publishes after a sub-turn carries that same sub-turn's
// plan and tool-call roll, not the stale value sess held when runSubTurn was
// called. persistLiveState and persistTaskState both write fresh state to
// the store; publishState used to publish the sess parameter as received,
// which never picked either write up in memory, so a live push to the
// browser mid-run could silently revert the in-flight card's plan and "Last
// calls" panel until the next full reconnect re-read the row from the store.
// The invariant: the state publish that follows a sub-turn carries that same
// sub-turn's new plan. This covers both
// publish points in runSubTurn: the reload before persistLiveState's
// publishState, and persistTaskState's own local sess.Plan update before its
// publishState.
func TestSubTurnPublishesFreshPlanAndRecentToolCalls(t *testing.T) {
	var streamCall int32Counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("n/a")}, FinishReason: wire.FinishStop}},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := streamCall.next()
		w.Header().Set("Content-Type", "text/event-stream")
		switch idx {
		case 0:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role: "assistant",
					ToolCalls: []wire.ToolCallDelta{
						{Index: 0, ID: "call_00_create", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskCreate", Arguments: `{"tasks":[{"subject":"Wire it up","description":"Wire it","activeForm":"Wiring"}]}`}},
						{Index: 1, ID: "call_01_bash", Type: "function", Function: wire.ToolCallFuncDelta{Name: "Bash", Arguments: `{"command":"echo hi"}`}},
					},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheMissTokens: 300, CompletionTokens: 5},
			})
		default:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_10_complete", Type: "function", Function: wire.ToolCallFuncDelta{Name: "Complete", Arguments: `{"summary":"wired it up","status":"done"}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.Hub = hub.New()

	// Both audiences of a state publish, watched at once: the list feed,
	// which gets the ListRow projection, and this session's own transcript
	// stream, which gets the whole row. The session id is generated up front
	// (RunOptions.SessionID) so the session subscription can exist before the
	// first sub-turn publishes — the roll is only asserted where it is
	// actually sent, and ListRow has no field for it to arrive in.
	sessionID := NewSessionID()
	statesCh, cancelList := r.Hub.SubscribeList()
	framesCh, cancelSession := r.Hub.Subscribe(sessionID)

	var listRows []hub.ListRow
	var fullRows []hub.SessionState
	drained := make(chan struct{})
	go func() {
		for s := range statesCh {
			listRows = append(listRows, s)
		}
		close(drained)
	}()
	framesDrained := make(chan struct{})
	go func() {
		for f := range framesCh {
			if f.State != nil {
				fullRows = append(fullRows, *f.State)
			}
		}
		close(framesDrained)
	}()

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "wire it up",
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("unexpected status: %s", res.Status)
	}
	// Run only returns once every sub-turn's publishState call for this
	// session has already happened, so every frame is in both channels by now.
	cancelList()
	cancelSession()
	<-drained
	<-framesDrained

	// The first sub-turn's publish (right after the TaskCreate+Bash sub-turn
	// committed, before Complete's sub-turn) must already carry the plan
	// and the Bash call — not empty, and not the finished row's own later
	// state, which this asserts against by checking the still-running one.
	// The plan is on both feeds; the roll is on the session's alone.
	var sawFullWithPlanAndRoll bool
	for _, s := range fullRows {
		if s.ID != res.SessionID || s.Status != store.StatusRunning {
			continue
		}
		if len(s.Plan) == 0 || len(s.RecentToolCalls) == 0 {
			continue
		}
		if !planHasSubject(t, s.Plan, "Wire it up") {
			continue
		}
		if len(s.RecentToolCalls) == 1 && s.RecentToolCalls[0].Name == "Bash" {
			sawFullWithPlanAndRoll = true
		}
	}
	if !sawFullWithPlanAndRoll {
		t.Fatalf("no running-status state frame on the session stream carried the sub-turn's plan and tool-call roll: %+v", fullRows)
	}

	var sawListWithPlan bool
	for _, s := range listRows {
		if s.ID != res.SessionID || s.Status != store.StatusRunning || len(s.Plan) == 0 {
			continue
		}
		if planHasSubject(t, s.Plan, "Wire it up") {
			sawListWithPlan = true
		}
	}
	if !sawListWithPlan {
		t.Fatalf("no running-status list row carried the sub-turn's plan: %+v", listRows)
	}
}

// planHasSubject reports whether a published plan is the single-item plan
// this test's TaskCreate wrote. Shared by the two assertions above so the
// list feed and the session stream are held to the same plan.
func planHasSubject(t *testing.T, plan json.RawMessage, subject string) bool {
	t.Helper()
	var todos []store.StatusTodo
	if err := json.Unmarshal(plan, &todos); err != nil {
		t.Fatalf("decode published plan %q: %v", plan, err)
	}
	return len(todos) == 1 && todos[0].Subject == subject
}
