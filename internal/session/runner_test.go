package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

func testPrices() *pricing.Table {
	return &pricing.Table{
		CapturedAt: "2026-08-09",
		Models: map[string]pricing.ModelPrices{
			"test-model": {InputCacheHitPerMillionUSD: 0.003625, InputCacheMissPerMillionUSD: 0.435, OutputPerMillionUSD: 0.87},
		},
	}
}

func strPtr(s string) *string { return &s }

func writeSSEChunk(t *testing.T, w http.ResponseWriter, chunk deepseek.ChatCompletionChunk) {
	t.Helper()
	b, err := json.Marshal(chunk)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, "data: %s\n\n", b)
	w.(http.Flusher).Flush()
}

// plainAnswerServer always answers the same short text with no tool calls,
// for both streaming and non-streaming requests. It is safe for concurrent
// use by multiple sessions.
func plainAnswerServer(t *testing.T, answer string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)

		if !probe.Stream {
			resp := deepseek.ChatCompletionResponse{
				Choices: []deepseek.Choice{{Message: deepseek.Message{Role: deepseek.RoleAssistant, Content: answer}, FinishReason: deepseek.FinishStop}},
				Usage:   &deepseek.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
			Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{Role: "assistant", Content: strPtr(answer)}}},
		})
		writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
			Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishStop)}},
			Usage:   &deepseek.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
}

func newTestRunner(t *testing.T, baseURL string) *Runner {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	return &Runner{
		Store:      st,
		Mirror:     store.NewMirror(filepath.Join(dir, "mirror")),
		Client:     deepseek.NewClient(baseURL, "test-key"),
		Prices:     testPrices(),
		FlashModel: "test-model",
	}
}

func TestRunCompletesWithNoToolCalls(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
	}
	if res.Text != "all done" {
		t.Fatalf("expected the answer text, got %q", res.Text)
	}
	if res.SubTurns != 1 {
		t.Fatalf("expected 1 sub-turn, got %d", res.SubTurns)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, string(e.Kind))
	}
	wantSequence := []string{"session_started", "turn_started", "content_delta", "turn_finished", "usage", "run_finished"}
	if strings.Join(kinds, ",") != strings.Join(wantSequence, ",") {
		t.Fatalf("unexpected event sequence: %v", kinds)
	}
}

// TestRunWritesProvenanceOntoSessionRow proves Run carries JobType,
// ParentAgentType, and ParentAgentID from RunOptions onto the session row
// it creates.
func TestRunWritesProvenanceOntoSessionRow(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
		JobType: agentmeta.JobTypeOrchestration, ParentAgentType: "orchestrator", ParentAgentID: "orch-1",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.JobType != agentmeta.JobTypeOrchestration ||
		sess.ParentAgentType != "orchestrator" || sess.ParentAgentID != "orch-1" {
		t.Fatalf("unexpected provenance on the session row: %+v", sess)
	}
}

// completeToolServer answers every streaming request with a single call to
// Complete, ending the run in one sub-turn. It is what
// TestRunGaveUpSetsCompleteStatus and its "done" counterpart drive against.
func completeToolServer(t *testing.T, status, summary string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args := fmt.Sprintf(`{"summary":%q,"status":%q}`, summary, status)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
			Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
				Role:      "assistant",
				ToolCalls: []deepseek.ToolCallDelta{{Index: 0, ID: "call_00_complete", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "Complete", Arguments: args}}},
			}}},
		})
		writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
			Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishToolCalls)}},
			Usage:   &deepseek.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
}

// TestRunGaveUpSetsCompleteStatus is the session-level half of the gave_up
// propagation fix (docs/DESIGN.md §4.10): a model that calls Complete with
// status "gave_up" must have that status readable off RunResult, not just
// buried in the run_finished event payload. The session's own terminal
// status stays "ok" either way — gave_up describes how the model
// characterised finishing, not whether the run itself completed.
func TestRunGaveUpSetsCompleteStatus(t *testing.T) {
	srv := completeToolServer(t, "gave_up", "could not find the bug")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "fix the bug",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected the session status to stay ok, got %s", res.Status)
	}
	if res.CompleteStatus != "gave_up" {
		t.Fatalf("expected CompleteStatus %q, got %q", "gave_up", res.CompleteStatus)
	}
	if res.Summary != "could not find the bug" {
		t.Fatalf("unexpected summary %q", res.Summary)
	}
}

// TestRunDoneSetsCompleteStatus is TestRunGaveUpSetsCompleteStatus's
// counterpart, proving CompleteStatus carries "done" too rather than only
// ever being read for the gave_up case.
func TestRunDoneSetsCompleteStatus(t *testing.T) {
	srv := completeToolServer(t, "done", "fixed it")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "fix the bug",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CompleteStatus != "done" {
		t.Fatalf("expected CompleteStatus %q, got %q", "done", res.CompleteStatus)
	}
}

// TestConcurrentSessionsAreIsolated runs two sessions at once against
// different workspaces on a shared Runner (shared Store, shared Client) and
// asserts neither one's event log leaks into the other's. This is the
// property docs/DESIGN.md §4.5 requires of the session runner: it holds no
// state outside the session it is running. Run with -race.
func TestConcurrentSessionsAreIsolated(t *testing.T) {
	srv := plainAnswerServer(t, "ok")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	const n = 6
	results := make([]*RunResult, n)
	workspaces := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		workspaces[i] = t.TempDir()
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := r.Run(t.Context(), RunOptions{
				Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
				Workspace: workspaces[i], PermissionMode: tools.ModeFull,
				Prompt: fmt.Sprintf("task number %d", i),
			})
			if err != nil {
				t.Errorf("session %d: Run: %v", i, err)
				return
			}
			results[i] = res
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for i, res := range results {
		if res == nil {
			t.Fatalf("session %d produced no result", i)
		}
		if seen[res.SessionID] {
			t.Fatalf("session id %s reused across runs", res.SessionID)
		}
		seen[res.SessionID] = true

		sess, err := r.Store.GetSession(t.Context(), res.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if sess.Workspace != mustEvalSymlinks(t, workspaces[i]) {
			t.Fatalf("session %d has workspace %s, want %s", i, sess.Workspace, workspaces[i])
		}

		events, err := r.Store.GetEvents(t.Context(), res.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		var started store.SessionStartedPayload
		if err := json.Unmarshal(events[0].Payload, &started); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprintf("task number %d", i)
		if !strings.Contains(started.OpeningMessage, want) {
			t.Fatalf("session %d opening message %q does not contain its own prompt %q", i, started.OpeningMessage, want)
		}
	}
}

func mustEvalSymlinks(t *testing.T, p string) string {
	t.Helper()
	resolved, err := tools.ResolvePath(p, ".")
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// TestCompactionForksNewSession drives a low compaction threshold so the
// first sub-turn (which reports oversized usage) triggers a fork before the
// second sub-turn runs. It proves the parent is marked compacted and the
// child is linked to it, carries a system prompt seeded with a summary, and
// inherits the parent's provenance.
func TestCompactionForksNewSession(t *testing.T) {
	var streamCall int32Counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)

		if !probe.Stream {
			resp := deepseek.ChatCompletionResponse{
				Choices: []deepseek.Choice{{Message: deepseek.Message{Role: deepseek.RoleAssistant, Content: "summary of prior work"}, FinishReason: deepseek.FinishStop}},
				Usage:   &deepseek.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := streamCall.next()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			// First sub-turn: a tool call, with usage that exceeds the
			// test's low compaction threshold.
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []deepseek.ToolCallDelta{{Index: 0, ID: "call_00_a", Type: "function", Function: deepseek.ToolCallFuncDelta{Name: "TodoWrite", Arguments: `{"todos":[]}`}}},
				}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishToolCalls)}},
				Usage:   &deepseek.Usage{PromptTokens: 900, PromptCacheHitTokens: 0, PromptCacheMissTokens: 900, CompletionTokens: 5},
			})
		} else {
			// Second sub-turn, now inside the compacted session: a plain
			// answer that ends the run.
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{Role: "assistant", Content: strPtr("continued and done")}}},
			})
			writeSSEChunk(t, w, deepseek.ChatCompletionChunk{
				Choices: []deepseek.ChunkChoice{{Delta: deepseek.ChunkDelta{}, FinishReason: strPtr(deepseek.FinishStop)}},
				Usage:   &deepseek.Usage{PromptTokens: 300, PromptCacheHitTokens: 100, PromptCacheMissTokens: 200, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.CompactionThresholdTokens = 500

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: deepseek.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "a task that will need compaction",
		JobType: agentmeta.JobTypeOrchestration, ParentAgentType: "orchestrator", ParentAgentID: "orch-1",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK || res.Text != "continued and done" {
		t.Fatalf("unexpected result: %+v", res)
	}

	sessions, err := r.Store.ListSessions(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 sessions (parent + compacted child), got %d", len(sessions))
	}
	var parent, child store.Session
	for _, s := range sessions {
		if s.ID == res.SessionID {
			child = s
		} else {
			parent = s
		}
	}
	if parent.Status != store.StatusCompacted {
		t.Fatalf("expected the parent session to be marked compacted, got %s", parent.Status)
	}
	if child.ParentID != parent.ID {
		t.Fatalf("expected the child's parent_id to point at the original session")
	}
	if !strings.Contains(child.SystemPrompt, "summary of prior work") {
		t.Fatalf("expected the child's system prompt to carry the summary, got: %s", child.SystemPrompt)
	}
	if child.JobType != agentmeta.JobTypeOrchestration ||
		child.ParentAgentType != "orchestrator" || child.ParentAgentID != "orch-1" {
		t.Fatalf("expected the compacted child to inherit the parent's provenance, got: %+v", child)
	}
}

// int32Counter is a tiny goroutine-safe counter, avoiding a dependency on
// sync/atomic's typed helpers for a single test.
type int32Counter struct {
	mu sync.Mutex
	n  int
}

func (c *int32Counter) next() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.n
	c.n++
	return v
}
