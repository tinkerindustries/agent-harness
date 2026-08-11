package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// TestResumeContinuesSubTurnNumbering runs a session to completion, resumes
// it with a follow-up prompt, and asserts the resumed run picks up sub-turn
// numbering where the first run left off rather than restarting at 1 — the
// numbering store.SessionUsageSummaries and the browser both read
// cumulatively across the whole log.
func TestResumeContinuesSubTurnNumbering(t *testing.T) {
	srv := plainAnswerServer(t, "first answer")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	first, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "first task",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.SubTurns != 1 {
		t.Fatalf("expected 1 sub-turn from the first run, got %d", first.SubTurns)
	}

	resumed, err := r.Resume(t.Context(), ResumeOptions{
		SessionID: first.SessionID, Prompt: "one more thing", MaxTokens: 4000,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", resumed.Status)
	}
	if resumed.SubTurns != 2 {
		t.Fatalf("expected resume to continue at sub-turn 2, got %d", resumed.SubTurns)
	}

	events, err := r.Store.GetEvents(t.Context(), first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var starts []store.SessionStartedPayload
	for _, e := range events {
		kinds = append(kinds, string(e.Kind))
		if e.Kind == store.KindSessionStarted {
			var p store.SessionStartedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			starts = append(starts, p)
		}
	}
	// Two full sub-turns, each session_started/turn_started/... pair, with
	// the resume's continuation appended in between as its own
	// session_started event rather than rewriting the first one.
	want := []string{
		"session_started", "turn_started", "content_delta", "turn_finished", "usage", "run_finished",
		"session_started", "turn_started", "content_delta", "turn_finished", "usage", "run_finished",
	}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected event sequence: %v", kinds)
	}
	if len(starts) != 2 {
		t.Fatalf("expected 2 session_started events, got %d", len(starts))
	}
	if starts[1].OpeningMessage != "one more thing" {
		t.Fatalf("expected the resume prompt as the second opening message, got %q", starts[1].OpeningMessage)
	}

	sess, err := r.Store.GetSession(t.Context(), first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusOK {
		t.Fatalf("expected the session to finish ok again after resume, got %s", sess.Status)
	}

	// The primed Detector (primeDetector) is what makes this meaningful: a
	// fresh Detector would also report no churn on its first Observe, but
	// only because it has nothing to compare against yet. This is a real
	// check against the first run's actual last request.
	usageEvents := 0
	for _, e := range events {
		if e.Kind != store.KindUsage {
			continue
		}
		usageEvents++
		if usageEvents != 2 {
			continue
		}
		var p store.UsagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.ChurnPointIndex != nil {
			t.Fatalf("expected no churn on the first sub-turn after a clean resume, got churn point %d", *p.ChurnPointIndex)
		}
	}
}

// TestResumeRefusesRunningOrCompacted checks the two states Resume must not
// touch: a session a live goroutine still owns, and one compaction already
// retired in favour of a child.
func TestResumeRefusesRunningOrCompacted(t *testing.T) {
	srv := plainAnswerServer(t, "n/a")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)
	ctx := t.Context()

	if err := r.Store.CreateSession(ctx, store.Session{
		ID: "sess-running", Model: "test-model", Effort: deepseek.EffortHigh, Workspace: t.TempDir(),
		PermissionMode: string(tools.ModeFull), SystemPrompt: RenderSystemPrompt(),
		ToolSchema: json.RawMessage(`[]`), Status: store.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resume(ctx, ResumeOptions{SessionID: "sess-running"}); err == nil {
		t.Fatal("expected Resume to refuse a running session")
	}

	if err := r.Store.CreateSession(ctx, store.Session{
		ID: "sess-compacted", Model: "test-model", Effort: deepseek.EffortHigh, Workspace: t.TempDir(),
		PermissionMode: string(tools.ModeFull), SystemPrompt: RenderSystemPrompt(),
		ToolSchema: json.RawMessage(`[]`), Status: store.StatusCompacted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resume(ctx, ResumeOptions{SessionID: "sess-compacted"}); err == nil {
		t.Fatal("expected Resume to refuse a compacted session")
	}
}

// TestResumeWithoutPromptContinuesTheExistingTask exercises Resume with an
// empty Prompt — the shape a run that stopped at MaxSubTurns without
// finishing needs: pick the loop back up with no new user message appended.
func TestResumeWithoutPromptContinuesTheExistingTask(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		n := calls.Add(1)

		if !probe.Stream {
			resp := deepseek.ChatCompletionResponse{
				Choices: []deepseek.Choice{{Message: deepseek.Message{Role: deepseek.RoleAssistant, Content: "n/a"}, FinishReason: deepseek.FinishStop}},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// First sub-turn: a tool call that keeps the loop going, so the
			// session ends at max_sub_turns rather than finishing outright.
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []deepseek.ToolCallDelta{{Index: 0, ID: "call_0", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}}},
				}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishToolCalls)}},
				Usage:   &deepseek.Usage{PromptTokens: 300, PromptCacheHitTokens: 0, PromptCacheMissTokens: 300, CompletionTokens: 5},
			})
		} else {
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{Role: "assistant", Content: strPtr("done now")}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishStop)}},
				Usage:   &deepseek.Usage{PromptTokens: 350, PromptCacheHitTokens: 256, PromptCacheMissTokens: 94, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	ws := t.TempDir()
	first, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "start a task",
		MaxSubTurns: 1,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.Status != store.StatusMaxTurns {
		t.Fatalf("expected the first run to stop at max_turns, got %s", first.Status)
	}

	resumed, err := r.Resume(t.Context(), ResumeOptions{SessionID: first.SessionID, MaxTokens: 4000, MaxSubTurns: 5})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK || resumed.Text != "done now" {
		t.Fatalf("unexpected resumed result: %+v", resumed)
	}
	if resumed.SubTurns != 2 {
		t.Fatalf("expected the resumed run to finish at sub-turn 2, got %d", resumed.SubTurns)
	}

	events, err := r.Store.GetEvents(t.Context(), first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, e := range events {
		if e.Kind == store.KindSessionStarted {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("expected exactly 1 session_started event when Resume's Prompt is empty, got %d", starts)
	}
}
