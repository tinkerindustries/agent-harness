package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// TestDebugChurnAtSubTurnIsCaughtAndNamed drives RunOptions.DebugChurnAtSubTurn
// through the full loop against a fake server that reports usage the way the
// real API would for a genuinely broken prefix (near-total miss on the
// churned sub-turn). It is the session-level half of the guarantee that a
// deliberately churned prefix is caught by the diagnostic and named to the
// specific message. The live demonstration against the real
// API exercises the identical option through `harness run
// -debug-churn-at-subturn` (cmd/harness/run.go).
func TestDebugChurnAtSubTurnIsCaughtAndNamed(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		n := calls.Add(1)

		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("n/a")}, FinishReason: wire.FinishStop}},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		switch n {
		case 1:
			// Sub-turn 1: a tool call, healthy usage — nothing to compare
			// against yet, so this one can never be reported as churn.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_0", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000, CompletionTokens: 20},
			})
		case 2:
			// Sub-turn 2: this is the one turn.go mutates before sending
			// (DebugChurnAtSubTurn == 2). Report usage the way DeepSeek
			// would for a request whose common prefix collapsed: almost
			// nothing hits, far more than the 127-token slack over what an
			// honest append would predict.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_1", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 1100, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1100, CompletionTokens: 20},
			})
		default:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("done")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 1300, PromptCacheHitTokens: 1024, PromptCacheMissTokens: 276, CompletionTokens: 5},
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
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "a task with several sub-turns",
		DebugChurnAtSubTurn: 2,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s: %+v", res.Status, res)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var usages []store.UsagePayload
	for _, e := range events {
		if e.Kind != store.KindUsage {
			continue
		}
		var p store.UsagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		usages = append(usages, p)
	}
	if len(usages) != 3 {
		t.Fatalf("expected 3 usage events, got %d", len(usages))
	}

	if usages[0].ChurnPointIndex != nil {
		t.Fatalf("sub-turn 1 has nothing to compare against and must not be reported as churn: %+v", usages[0])
	}
	// This is the exit criterion: the deliberately churned sub-turn is
	// caught (actual miss far exceeds expected) and named to the specific
	// message the debug hook mutated — index 1, the opening user message.
	if usages[1].ChurnPointIndex == nil {
		t.Fatalf("expected sub-turn 2 to be reported as churned, got %+v", usages[1])
	}
	if *usages[1].ChurnPointIndex != 1 {
		t.Fatalf("expected the churn point to name message index 1 (the opening message cache.Mutate touched), got %d", *usages[1].ChurnPointIndex)
	}
	if usages[1].PromptCacheMissTokens-usages[1].ExpectedMissTokens <= 127 {
		t.Fatalf("expected the churned sub-turn's actual miss to exceed its expected miss by more than one cache block, got actual=%d expected=%d",
			usages[1].PromptCacheMissTokens, usages[1].ExpectedMissTokens)
	}

	// The mutation is on-the-wire only: the stored opening message is never
	// rewritten by the debug hook, which is what keeps the fold append-only
	// (docs/CACHE.md) even while deliberately churning one request.
	var started store.SessionStartedPayload
	if err := json.Unmarshal(events[0].Payload, &started); err != nil {
		t.Fatal(err)
	}
	resolvedWS, err := tools.ResolvePath(ws, ".")
	if err != nil {
		t.Fatal(err)
	}
	if started.OpeningMessage != RenderOpeningMessage(resolvedWS, "a task with several sub-turns", nil, "", "", nil) {
		t.Fatalf("expected the stored opening message to be untouched by the debug hook, got %q", started.OpeningMessage)
	}
}

// The debug hook is gated on a positive sub-turn number, not merely on
// equality with the current one. Sub-turns start at 1 today, so the zero
// value already cannot match; this keeps that true if numbering ever changes.
func TestDebugChurnIsOffByDefault(t *testing.T) {
	var opts RunOptions
	if opts.DebugChurnAtSubTurn != 0 {
		t.Fatalf("zero value of DebugChurnAtSubTurn = %d, want 0", opts.DebugChurnAtSubTurn)
	}
	for subTurn := 0; subTurn <= 3; subTurn++ {
		if opts.DebugChurnAtSubTurn > 0 && opts.DebugChurnAtSubTurn == subTurn {
			t.Fatalf("default RunOptions would churn sub-turn %d", subTurn)
		}
	}
}
