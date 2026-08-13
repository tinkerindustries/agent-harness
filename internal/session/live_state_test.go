package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
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
			resp := deepseek.ChatCompletionResponse{
				Choices: []deepseek.Choice{{Message: deepseek.Message{Role: deepseek.RoleAssistant, Content: "n/a"}, FinishReason: deepseek.FinishStop}},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := streamCall.next()
		w.Header().Set("Content-Type", "text/event-stream")
		switch idx {
		case 0:
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
					Role: "assistant",
					ToolCalls: []deepseek.ToolCallDelta{
						{Index: 0, ID: "call_00_create", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "TaskCreate", Arguments: `{"tasks":[{"subject":"Read the spec","description":"Read it","activeForm":"Reading"},{"subject":"Wire it up","description":"Wire it","activeForm":"Wiring"}]}`}},
						{Index: 1, ID: "call_01_bash", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "Bash", Arguments: `{"command":"echo hi"}`}},
					},
				}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishToolCalls)}},
				Usage:   &deepseek.Usage{PromptTokens: 300, PromptCacheMissTokens: 300, CompletionTokens: 5},
			})
		case 1:
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []deepseek.ToolCallDelta{{Index: 0, ID: "call_10_update", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "TaskUpdate", Arguments: `{"taskId":"1","status":"completed"}`}}},
				}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishToolCalls)}},
				Usage:   &deepseek.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		default:
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []deepseek.ToolCallDelta{{Index: 0, ID: "call_20_complete", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "Complete", Arguments: `{"summary":"wired it up","status":"done"}`}}},
				}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishToolCalls)}},
				Usage:   &deepseek.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
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
			resp := deepseek.ChatCompletionResponse{
				Choices: []deepseek.Choice{{Message: deepseek.Message{Role: deepseek.RoleAssistant, Content: "n/a"}, FinishReason: deepseek.FinishStop}},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := streamCall.next()
		w.Header().Set("Content-Type", "text/event-stream")
		switch idx {
		case 0:
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
					Role: "assistant",
					ToolCalls: []deepseek.ToolCallDelta{
						{Index: 0, ID: "call_00_list", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}},
						{Index: 1, ID: "call_01_get", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "TaskGet", Arguments: `{"taskId":"1"}`}},
					},
				}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishToolCalls)}},
				Usage:   &deepseek.Usage{PromptTokens: 300, PromptCacheMissTokens: 300, CompletionTokens: 5},
			})
		default:
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []deepseek.ToolCallDelta{{Index: 0, ID: "call_10_complete", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "Complete", Arguments: `{"summary":"nothing to do","status":"done"}`}}},
				}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishToolCalls)}},
				Usage:   &deepseek.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
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
			resp := deepseek.ChatCompletionResponse{
				Choices: []deepseek.Choice{{Message: deepseek.Message{Role: deepseek.RoleAssistant, Content: "n/a"}, FinishReason: deepseek.FinishStop}},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := streamCall.next()
		w.Header().Set("Content-Type", "text/event-stream")
		switch idx {
		case 0:
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
					Role: "assistant",
					ToolCalls: []deepseek.ToolCallDelta{
						{Index: 0, ID: "call_00_create", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "TaskCreate", Arguments: `{"tasks":[{"subject":"Wire it up","description":"Wire it","activeForm":"Wiring"}]}`}},
						{Index: 1, ID: "call_01_bash", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "Bash", Arguments: `{"command":"echo hi"}`}},
					},
				}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishToolCalls)}},
				Usage:   &deepseek.Usage{PromptTokens: 300, PromptCacheMissTokens: 300, CompletionTokens: 5},
			})
		default:
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []deepseek.ToolCallDelta{{Index: 0, ID: "call_10_complete", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "Complete", Arguments: `{"summary":"wired it up","status":"done"}`}}},
				}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishToolCalls)}},
				Usage:   &deepseek.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.Hub = hub.New()
	statesCh, cancel := r.Hub.SubscribeList()

	var published []hub.SessionState
	drained := make(chan struct{})
	go func() {
		for s := range statesCh {
			published = append(published, s)
		}
		close(drained)
	}()

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "wire it up",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("unexpected status: %s", res.Status)
	}
	// Run only returns once every sub-turn's publishState call for this
	// session has already happened, so every event is in statesCh by now.
	cancel()
	<-drained

	// The first sub-turn's publish (right after the TaskCreate+Bash sub-turn
	// committed, before Complete's sub-turn) must already carry the plan
	// and the Bash call — not empty, and not the finished row's own later
	// state, which this asserts against by checking the still-running one.
	var sawLiveWithPlan bool
	for _, s := range published {
		if s.ID != res.SessionID || s.Status != store.StatusRunning {
			continue
		}
		if len(s.Plan) == 0 || len(s.RecentToolCalls) == 0 {
			continue
		}
		var todos []store.StatusTodo
		if err := json.Unmarshal(s.Plan, &todos); err != nil {
			t.Fatalf("decode published plan %q: %v", s.Plan, err)
		}
		if len(todos) == 1 && todos[0].Subject == "Wire it up" &&
			len(s.RecentToolCalls) == 1 && s.RecentToolCalls[0].Name == "Bash" {
			sawLiveWithPlan = true
		}
	}
	if !sawLiveWithPlan {
		t.Fatalf("no running-status session_state publish carried the sub-turn's plan and tool-call roll: %+v", published)
	}
}
