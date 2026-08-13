package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// TestParallelToolResultsAppendInCallOrder gives the model two Bash calls
// where the second (index 1) finishes well before the first (index 0), and
// asserts the stored tool_result events are still in call order — index
// order, not completion order (docs/TOOLS.md, docs/CACHE.md). Execution is
// concurrent; only the append must be ordered.
func TestParallelToolResultsAppendInCallOrder(t *testing.T) {
	var streamCall int32Counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: "n/a"}, FinishReason: wire.FinishStop}},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := streamCall.next()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role: "assistant",
					ToolCalls: []wire.ToolCallDelta{
						{Index: 0, ID: "call_00_slow", Type: "function", Function: wire.ToolCallFuncDelta{Name: "Bash", Arguments: `{"command":"sleep 0.2 && echo slow"}`}},
						{Index: 1, ID: "call_01_fast", Type: "function", Function: wire.ToolCallFuncDelta{Name: "Bash", Arguments: `{"command":"echo fast"}`}},
					},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheMissTokens: 300, CompletionTokens: 5},
			})
		} else {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("done")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 400, PromptCacheHitTokens: 300, PromptCacheMissTokens: 100, CompletionTokens: 5},
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
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "run two commands",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("unexpected status: %s", res.Status)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var resultIDs []string
	for _, e := range events {
		if e.Kind != store.KindToolResult {
			continue
		}
		var p store.ToolResultPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		resultIDs = append(resultIDs, p.ToolCallID)
		if p.ToolCallID == "call_00_slow" && !strings.Contains(p.Content, "slow") {
			t.Fatalf("expected the slow call's own output, got: %s", p.Content)
		}
	}
	want := []string{"call_00_slow", "call_01_fast"}
	if len(resultIDs) != 2 || resultIDs[0] != want[0] || resultIDs[1] != want[1] {
		t.Fatalf("tool results out of order: got %v, want %v", resultIDs, want)
	}
}
