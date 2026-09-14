package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// toolLoopServer answers every streaming request with a TaskList call until
// request answerAt, which gets a plain answer. An answerAt of zero never
// answers, so the run keeps calling tools until something outside the model
// stops it. onRequest, when set, sees each request's number before it is
// answered and may stop the handler by returning false.
func toolLoopServer(t *testing.T, answerAt int32, onRequest func(n int32, r *http.Request) bool) (*httptest.Server, *atomic.Int32) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if onRequest != nil && !onRequest(n, r) {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == answerAt {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("finished")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheHitTokens: 256, PromptCacheMissTokens: 44, CompletionTokens: 5},
			})
		} else {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: fmt.Sprintf("call_%d", n), Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheHitTokens: 256, PromptCacheMissTokens: 44, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func toolLoopOptions(t *testing.T) RunOptions {
	return RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "keep working",
	}
}

// TestRunHasNoSubTurnCeiling pins that nothing counts sub-turns against a
// run. The model calls a tool on 450 requests, past the 400 a run used to be
// capped at, and the run ends only when the model answers without one.
func TestRunHasNoSubTurnCeiling(t *testing.T) {
	const answerAt = 451
	srv, calls := toolLoopServer(t, answerAt, nil)
	r := newTestRunner(t, srv.URL)

	res, err := r.Run(t.Context(), toolLoopOptions(t))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK || res.Reason != "no_tool_calls" {
		t.Fatalf("expected the run to finish on the model's answer, got status %q reason %q", res.Status, res.Reason)
	}
	if res.SubTurns != answerAt || calls.Load() != answerAt {
		t.Fatalf("expected %d sub-turns and requests, got %d sub-turns and %d requests", answerAt, res.SubTurns, calls.Load())
	}
	if res.Text != "finished" {
		t.Fatalf("expected the final answer text, got %q", res.Text)
	}
}

// TestRunEndsAtItsContextDeadline pins the wall-clock budget: a model that
// never stops calling tools is ended by the deadline on the run's context,
// with the session marked timeout.
func TestRunEndsAtItsContextDeadline(t *testing.T) {
	srv, calls := toolLoopServer(t, 0, nil)
	r := newTestRunner(t, srv.URL)

	ctx, cancel := context.WithTimeout(t.Context(), 750*time.Millisecond)
	defer cancel()
	res, err := r.Run(ctx, toolLoopOptions(t))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run error = %v, want context.DeadlineExceeded", err)
	}
	if res == nil || res.Status != store.StatusTimeout {
		t.Fatalf("expected a timeout result, got %+v", res)
	}
	if calls.Load() < 2 {
		t.Fatalf("expected the run to loop before the deadline, got %d requests", calls.Load())
	}
	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusTimeout {
		t.Fatalf("stored status = %q, want %q", sess.Status, store.StatusTimeout)
	}
}

// TestRunEndsWhenTheCallerCancels pins the other way a looping run ends: the
// caller cancels its context partway through, and the session is marked
// cancelled after the sub-turns that completed.
func TestRunEndsWhenTheCallerCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	srv, _ := toolLoopServer(t, 0, func(n int32, r *http.Request) bool {
		if n <= 20 {
			return true
		}
		cancel()
		return false
	})
	r := newTestRunner(t, srv.URL)

	res, err := r.Run(ctx, toolLoopOptions(t))
	if err == nil {
		t.Fatal("Run: expected the cancellation as an error")
	}
	if res == nil || res.Status != store.StatusCancelled {
		t.Fatalf("expected a cancelled result, got %+v", res)
	}
	if res.SubTurns != 20 {
		t.Fatalf("expected 20 completed sub-turns before the cancel, got %d", res.SubTurns)
	}
}
