package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// TestTodoWritePersistsPlanToSessionRow drives a run whose first sub-turn
// calls TodoWrite (with a two-item plan) alongside a Bash call, then a
// second sub-turn that finishes with Complete, and asserts the session row
// carries the plan, the filtered recent-tool-call roll, and the model's own
// summary — the data the session list renders the in-flight card and the
// finished table's subtitle from (docs/WEB-REDESIGN.md phase 3).
func TestTodoWritePersistsPlanToSessionRow(t *testing.T) {
	const planArgs = `{"todos":[` +
		`{"content":"Read the spec","status":"completed","activeForm":""},` +
		`{"content":"Wire it up","status":"in_progress","activeForm":"Wiring"}]}`
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
						{Index: 0, ID: "call_00_todo", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "TodoWrite", Arguments: planArgs}},
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

	// The plan is the todos array of the TodoWrite call, verbatim.
	var todos []store.StatusTodo
	if err := json.Unmarshal([]byte(sess.Plan), &todos); err != nil {
		t.Fatalf("decode stored plan %q: %v", sess.Plan, err)
	}
	if len(todos) != 2 || todos[0].Content != "Read the spec" || todos[1].Status != "in_progress" || todos[1].ActiveForm != "Wiring" {
		t.Fatalf("unexpected stored plan: %+v", todos)
	}

	// The roll keeps the Bash call and the finishing Complete call, and
	// drops TodoWrite, whose content the plan panel already shows.
	if len(sess.RecentToolCalls) != 2 || sess.RecentToolCalls[0].Name != "Bash" || sess.RecentToolCalls[1].Name != "Complete" {
		t.Fatalf("unexpected recent tool call roll: %+v", sess.RecentToolCalls)
	}

	if sess.Summary != "wired it up" {
		t.Fatalf("expected the Complete summary on the row, got %q", sess.Summary)
	}
}
