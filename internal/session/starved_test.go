package session

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// starvedThenAnswerServer exhausts the budget on reasoning for the first
// request and answers normally on the second, which is the shape
// deepseek.IsReasoningStarved detects: finish_reason "length" with no
// content. Both responses report usage, because the API bills both.
func starvedThenAnswerServer(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant"}, FinishReason: strPtr(wire.FinishLength)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 4000},
			})
		} else {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("recovered")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 7},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
}

func usageEvents(t *testing.T, events []store.Event) []store.UsagePayload {
	t.Helper()
	var out []store.UsagePayload
	for _, e := range events {
		if e.Kind != store.KindUsage {
			continue
		}
		var p store.UsagePayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode usage payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// The starved attempt is billed in full — it is an exhausted max_tokens
// budget spent entirely on reasoning — so dropping it understates the run's
// cost by the most expensive kind of turn there is.
func TestStarvedRetryRecordsBothAttempts(t *testing.T) {
	srv := starvedThenAnswerServer(t)
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "think hard",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "recovered" {
		t.Fatalf("expected the retry's answer, got %q", res.Text)
	}

	events, err := r.Store.GetEventsAfter(t.Context(), res.SessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	usage := usageEvents(t, events)
	if len(usage) != 2 {
		t.Fatalf("expected one usage event per request, got %d", len(usage))
	}

	first, second := usage[0], usage[1]
	if first.Attempt != 1 || second.Attempt != 2 {
		t.Fatalf("expected attempts 1 then 2, got %d then %d", first.Attempt, second.Attempt)
	}
	if first.SubTurn != 1 || second.SubTurn != 1 {
		t.Fatalf("expected both attempts on sub-turn 1, got %d and %d", first.SubTurn, second.SubTurn)
	}
	if first.CompletionTokens != 4000 {
		t.Fatalf("expected the starved attempt's 4000 completion tokens, got %d", first.CompletionTokens)
	}
	if first.CostUSD <= 0 {
		t.Fatal("expected the starved attempt to carry a cost; its tokens were billed")
	}
	// The discarded attempt sent the same messages as the retry, so observing
	// it would count one prefix twice.
	if first.ExpectedMissTokens != 0 || first.ChurnPointIndex != nil {
		t.Fatalf("expected no churn report on the discarded attempt, got %+v", first)
	}
	if second.ExpectedMissTokens == 0 {
		t.Fatal("expected the final attempt to carry the churn report")
	}
}

// The turn_finished event's elapsed_ms is the wall time of the request(s)
// the run waited on, measured around r.stream — created_at cannot carry it,
// because AppendEvents stamps one instant across the whole batch.
func TestTurnFinishedCarriesElapsedMs(t *testing.T) {
	const sleep = 20 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(sleep)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("done")}}},
		})
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
			Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := turnFinishedElapsedMs(t, r, res.SessionID); elapsed < sleep.Milliseconds() {
		t.Fatalf("expected elapsed_ms to cover the request's %v, got %dms", sleep, elapsed)
	}
}

// On the reasoning-starved path the same sub-turn sends a second request,
// so the single elapsed_ms figure must cover both of them.
func TestTurnFinishedElapsedCoversBothStarvedAttempts(t *testing.T) {
	const sleep = 20 * time.Millisecond
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(sleep)
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant"}, FinishReason: strPtr(wire.FinishLength)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 4000},
			})
		} else {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("recovered")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 7},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "think hard",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if elapsed := turnFinishedElapsedMs(t, r, res.SessionID); elapsed < 2*sleep.Milliseconds() {
		t.Fatalf("expected elapsed_ms to cover both requests' %v, got %dms", 2*sleep, elapsed)
	}
}

func turnFinishedElapsedMs(t *testing.T, r *Runner, sessionID string) int64 {
	t.Helper()
	events, err := r.Store.GetEvents(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind != store.KindTurnFinished {
			continue
		}
		var p store.TurnFinishedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		return p.ElapsedMs
	}
	t.Fatal("no turn_finished event found")
	return 0
}

// turn_started carried sub_turn and turn_finished and usage did not, so a
// consumer could only attribute them by replaying the stream in order. Every
// turn-scoped event now names its own turn.
func TestTurnScopedEventsCarryTheirSubTurn(t *testing.T) {
	srv := bashThenAnswerServer(t, "echo hi", "done")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "run it",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := r.Store.GetEventsAfter(t.Context(), res.SessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	current := 0
	seen := 0
	for _, e := range events {
		switch e.Kind {
		case store.KindTurnStarted:
			var p store.TurnStartedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			current = p.SubTurn
		case store.KindTurnFinished:
			var p store.TurnFinishedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.SubTurn != current {
				t.Fatalf("turn_finished says sub-turn %d inside sub-turn %d", p.SubTurn, current)
			}
			seen++
		case store.KindUsage:
			var p store.UsagePayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.SubTurn != current {
				t.Fatalf("usage says sub-turn %d inside sub-turn %d", p.SubTurn, current)
			}
			seen++
		}
	}
	if current < 2 {
		t.Fatalf("expected a multi-turn run to exercise the pairing, got %d sub-turns", current)
	}
	if seen != current*2 {
		t.Fatalf("expected a turn_finished and a usage per sub-turn, got %d events across %d turns", seen, current)
	}
}

// The session list and the running-cost column sum every usage event, so the
// discarded attempt has to reach them for the total to be the truth.
func TestStarvedRetryCostReachesTheSessionSummary(t *testing.T) {
	srv := starvedThenAnswerServer(t)
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "think hard",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	sums, err := r.Store.SessionUsageSummaries(t.Context(), []string{res.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	sum := sums[res.SessionID]
	if sum.CompletionTokens != 4007 {
		t.Fatalf("expected both attempts' 4007 completion tokens in the summary, got %d", sum.CompletionTokens)
	}
	// Two requests, one sub-turn: SubTurns counts turn_started, so the extra
	// usage event must not inflate it.
	if sum.SubTurns != 1 {
		t.Fatalf("expected 1 sub-turn, got %d", sum.SubTurns)
	}
}
