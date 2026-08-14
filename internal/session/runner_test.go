package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/gemini/geminitest"
	"github.com/mrgeoffrich/deepseek-harness/internal/kimi"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

func testPrices() *pricing.Table {
	return &pricing.Table{
		CapturedAt: "2026-08-09",
		Models: map[string]pricing.ModelPrices{
			"test-model": {InputCacheHitPerMillionUSD: 0.003625, InputCacheMissPerMillionUSD: 0.435, OutputPerMillionUSD: 0.87},
			// The vision model the ReviewScreenshot tool uses by default,
			// with the introductory rates fetched from Google's pricing page
			// on 2026-08-14, so the Gemini cost accounting test below has a
			// real table to cost against. Keyed on the constant rather than a
			// literal so it follows the default when that moves; a table
			// pricing the wrong Gemini model does not fail loudly here, it
			// just silently costs the call at zero.
			gemini.DefaultModel: {InputCacheHitPerMillionUSD: 0.075, InputCacheMissPerMillionUSD: 0.75, OutputPerMillionUSD: 3.75},
		},
	}
}

func strPtr(s string) *string { return &s }

func writeSSEChunk(t *testing.T, w http.ResponseWriter, chunk wire.ChatCompletionChunk) {
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
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent(answer)}, FinishReason: wire.FinishStop}},
				Usage:   &wire.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr(answer)}}},
		})
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
			Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
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
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
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

// recordingAnswerServer answers every request like plainAnswerServer but
// records each request body, so a test can assert what the model was sent.
func recordingAnswerServer(t *testing.T, answer string, bodies *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr(answer)}}},
		})
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
			Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
}

// TestRunWithEmptyPromptWaitsForFirstSteer pins the browser-start contract
// (docs/RUN-CONTROL.md "The frontend"): POST /api/runs creates the run with
// no prompt, and the loop must not send anything to the model until the
// operator's first message lands as a steer_message. Before this wait
// existed, an empty task went to the model, which answered "what would you
// like me to do?" and ended the run (no_tool_calls) before the operator
// could type — the live run that exposed it. The test: Run with an
// empty prompt sends no request while waiting; appending a steer_message
// the way the HTTP handler does makes it proceed, and the request the model
// then sees carries the steer text as a user message.
func TestRunWithEmptyPromptWaitsForFirstSteer(t *testing.T) {
	var bodies []string
	srv := recordingAnswerServer(t, "all done", &bodies)
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	done := make(chan *RunResult, 1)
	errCh := make(chan error, 1)
	go func() {
		res, err := r.Run(t.Context(), RunOptions{
			SessionID: "sess-wait", Model: "test-model", Effort: wire.EffortHigh,
			Thinking: true, MaxTokens: 4000,
			Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "",
		})
		if err != nil {
			errCh <- err
			return
		}
		done <- res
	}()

	// Give the loop time to reach the wait, then assert nothing went to the
	// model — the whole point of the wait.
	time.Sleep(400 * time.Millisecond)
	if len(bodies) != 0 {
		t.Fatalf("sent %d request(s) to the model while the prompt was empty; the run must wait for the first steer", len(bodies))
	}

	// The steer the browser's POST would have committed (docs/RUN-CONTROL.md
	// "The HTTP surface").
	if _, err := r.Store.AppendEvents(t.Context(), "sess-wait", []store.EventInput{
		{Kind: store.KindSteerMessage, Payload: store.SteerMessagePayload{Text: "do the thing", Source: "web"}},
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case res := <-done:
		if res.Status != store.StatusOK {
			t.Fatalf("expected status ok, got %s", res.Status)
		}
		if res.SubTurns != 1 {
			t.Fatalf("expected 1 sub-turn, got %d", res.SubTurns)
		}
	case err := <-errCh:
		t.Fatalf("Run: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("run did not proceed after the first steer arrived")
	}

	if len(bodies) != 1 {
		t.Fatalf("expected exactly one request after the steer, got %d", len(bodies))
	}
	if !strings.Contains(bodies[0], "do the thing") {
		t.Fatalf("the model's request does not carry the first message: %s", bodies[0])
	}

	events, err := r.Store.GetEvents(t.Context(), "sess-wait")
	if err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, e := range events {
		if e.Kind == store.KindSteerApplied {
			var p store.SteerAppliedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			if p.SubTurn != 1 || p.Text != "do the thing" {
				t.Fatalf("steer_applied = %+v, want sub-turn 1", p)
			}
			seen = true
		}
	}
	if !seen {
		t.Fatal("no steer_applied event in the log; the first message was never delivered")
	}
}

// TestRunCancelMarksSessionCancelled pins the soft-stop terminal path (the
// live stop test that exposed it): when a stop cancels the run's
// context mid-request, the runner's fail() must still land its terminal
// bookkeeping — on a fresh context, because the run's own is cancelled — and
// mark the row cancelled (the status the CANCELLED badge and the stop
// escalation use), not leave it running forever while the queue result says
// cancelled. The deadline case is pinned to timeout by the same switch.
func TestRunCancelMarksSessionCancelled(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-block // hold the request open until the test cancels the run
	}))
	defer srv.Close()
	defer close(block)
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, RunOptions{
			SessionID: "sess-stop", Model: "test-model", Effort: wire.EffortHigh,
			Thinking: true, MaxTokens: 4000,
			Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "do the thing",
		})
		errCh <- err
	}()

	// Wait until the run's request is in flight, then cancel the context the
	// way a stop does (the worker's soft stop cancels runCtx).
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the run's request never reached the server")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after the cancel")
	}

	sess, err := r.Store.GetSession(context.Background(), "sess-stop")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusCancelled {
		t.Fatalf("session status = %q, want %q — the soft stop must terminal the row", sess.Status, store.StatusCancelled)
	}
	if sess.FinishedAt == nil {
		t.Fatal("session finished_at not set by the soft stop")
	}
}

// TestRunWritesProvenanceOntoSessionRow proves Run carries JobType,
// ParentAgentType, ParentAgentID, and ParentIsUser from RunOptions onto the
// session row it creates.
func TestRunWritesProvenanceOntoSessionRow(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
		JobType: agentmeta.JobTypeOrchestration, ParentAgentType: "orchestrator", ParentAgentID: "orch-1",
		ParentIsUser: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.JobType != agentmeta.JobTypeOrchestration ||
		sess.ParentAgentType != "orchestrator" || sess.ParentAgentID != "orch-1" || !sess.ParentIsUser {
		t.Fatalf("unexpected provenance on the session row: %+v", sess)
	}
}

// TestRunWritesTitleFieldsOntoSessionRow pins the four session fields end to
// end on the run path: RunOptions carries them onto the store.Session it
// creates, next to Task, so the session list can render a title and a phase
// chip without reading the event log.
func TestRunWritesTitleFieldsOntoSessionRow(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
		Title:       "Add session title fields",
		Description: "Carry a title, description, and phase position from every producer onto the session row.",
		Phase:       2,
		TotalPhases: 5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Task != "say something" {
		t.Fatalf("task must stay the raw prompt, got %q", sess.Task)
	}
	if sess.Title != "Add session title fields" || sess.Description != "Carry a title, description, and phase position from every producer onto the session row." {
		t.Fatalf("title fields not copied onto the session row: %+v", sess)
	}
	if sess.Phase != 2 || sess.TotalPhases != 5 {
		t.Fatalf("phase fields not copied onto the session row: %+v", sess)
	}
}

// TestSubagentTitleAndPhaseInheritance pins the child session's session
// fields: the Task tool's own description argument becomes the child's title
// (truncated to the title cap when a model supplies a long one), the child's
// Description stays empty, and the parent's phase position is inherited — a
// delegated slice of a phased job is still part of that job.
func TestSubagentTitleAndPhaseInheritance(t *testing.T) {
	srv := plainAnswerServer(t, "subagent answer")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	run := r.subagentRunner("parent-sess", RunOptions{PermissionMode: tools.ModeFull, Phase: 3, TotalPhases: 4}, ws)
	_, childID, err := run(t.Context(), "one two three four five six seven eight nine ten eleven words here", "find it", "general")
	if err != nil {
		t.Fatalf("subagent run: %v", err)
	}

	child, err := r.Store.GetSession(t.Context(), childID)
	if err != nil {
		t.Fatalf("expected the child session to exist in the store: %v", err)
	}
	if want := "one two three four five six seven eight nine ten"; child.Title != want {
		t.Fatalf("expected the child title truncated to the first ten words, got %q", child.Title)
	}
	if child.Description != "" {
		t.Fatalf("expected the child description to stay empty, got %q", child.Description)
	}
	if child.Phase != 3 || child.TotalPhases != 4 {
		t.Fatalf("expected the child to inherit the parent's phase position, got %d/%d", child.Phase, child.TotalPhases)
	}
}

// completeToolServer answers every streaming request with a single call to
// Complete, ending the run in one sub-turn. It is what
// TestRunGaveUpSetsCompleteStatus and its "done" counterpart drive against.
func completeToolServer(t *testing.T, status, summary string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		args := fmt.Sprintf(`{"summary":%q,"status":%q}`, summary, status)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
				Role:      "assistant",
				ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_00_complete", Type: "function", Function: wire.ToolCallFuncDelta{Name: "Complete", Arguments: args}}},
			}}},
		})
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
			Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
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
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
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
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "fix the bug",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.CompleteStatus != "done" {
		t.Fatalf("expected CompleteStatus %q, got %q", "done", res.CompleteStatus)
	}
}

// A model that keeps re-sending a Complete the schema keeps rejecting is not
// correcting anything, and the loop must stop paying for it: one run
// spent eleven sub-turns and 7% of its budget on identical rejections
// (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md). The server here
// answers every request with the same schema-failing Complete, so an unbounded
// loop would run to the sub-turn limit.
func TestRepeatedCompleteRejectionsEndTheRun(t *testing.T) {
	srv := completeToolServer(t, "done", "fixed it")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "fix the bug",
		MaxSubTurns:  50,
		ResultSchema: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason != ReasonCompleteRejected {
		t.Fatalf("expected reason %q, got %q", ReasonCompleteRejected, res.Reason)
	}
	if res.Status != store.StatusFailed {
		t.Fatalf("a run with no valid result must not be ok, got %s", res.Status)
	}
	if res.SubTurns != maxCompleteRejections {
		t.Fatalf("expected the run to stop at %d sub-turns, got %d", maxCompleteRejections, res.SubTurns)
	}
	if len(res.Result) != 0 {
		t.Fatalf("a rejected result must not reach the requester, got %s", res.Result)
	}

	// The transcript has to say why the run stopped where it did, or the
	// operator sees a run that ended three sub-turns in for no stated reason.
	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var errText string
	for _, ev := range events {
		if ev.Kind == store.KindError {
			errText = string(ev.Payload)
		}
	}
	if !strings.Contains(errText, "Complete rejected") || !strings.Contains(errText, "expected object, got null") {
		t.Fatalf("expected an error event naming the rejection, got %q", errText)
	}
}

// The counter is for a model stuck on one error, not for a model working
// through several. A rejection that differs from the last one restarts it.
func TestDifferingCompleteRejectionsDoNotAccumulate(t *testing.T) {
	var n int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		i := n
		mu.Unlock()
		// Alternating between a wrong type and a missing property means no
		// two consecutive rejections carry the same message, so the run
		// should keep going and exhaust its sub-turn budget instead.
		result := `{"count":"not a number"}`
		if i%2 == 0 {
			result = `{}`
		}
		args := fmt.Sprintf(`{"summary":"s","status":"done","result":%s}`, result)
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
				Role:      "assistant",
				ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_00_complete", Type: "function", Function: wire.ToolCallFuncDelta{Name: "Complete", Arguments: args}}},
			}}},
		})
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
			Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "fix the bug",
		MaxSubTurns:  5,
		ResultSchema: json.RawMessage(`{"type":"object","properties":{"count":{"type":"integer"}},"required":["count"]}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reason == ReasonCompleteRejected {
		t.Fatal("distinct rejections must not be counted as one stuck loop")
	}
	if res.SubTurns != 5 {
		t.Fatalf("expected the run to reach its sub-turn limit, got %d", res.SubTurns)
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
				Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
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
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("summary of prior work")}, FinishReason: wire.FinishStop}},
				Usage:   &wire.Usage{PromptTokens: 50, CompletionTokens: 10},
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
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_00_a", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 900, PromptCacheHitTokens: 0, PromptCacheMissTokens: 900, CompletionTokens: 5},
			})
		} else {
			// Second sub-turn, now inside the compacted session: a plain
			// answer that ends the run.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("continued and done")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheHitTokens: 100, PromptCacheMissTokens: 200, CompletionTokens: 5},
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
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "a task that will need compaction",
		JobType: agentmeta.JobTypeOrchestration, ParentAgentType: "orchestrator", ParentAgentID: "orch-1",
		ParentIsUser: true,
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
		child.ParentAgentType != "orchestrator" || child.ParentAgentID != "orch-1" || !child.ParentIsUser {
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

// TestRunGeminiCostShowsInSessionTotal drives a full run whose first
// sub-turn calls ReviewScreenshot, then asserts the session's cost total
// covers the Gemini call: the tool result carries its usage, the runner
// commits it as its own usage event, and SessionUsageSummaries sums it into
// the session cost exactly as it sums a DeepSeek turn's usage (docs/DESIGN.md
// §4.9). The DeepSeek side bills 100 hit + 100 miss + 5 completion per
// sub-turn; the Gemini side bills the verified sample: 15 uncached input +
// 57 completion (output 1 + thought 56, thinking at the output rate).
func TestRunGeminiCostShowsInSessionTotal(t *testing.T) {
	geminiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Answer("[{}]", `{"total_tokens":72,"total_input_tokens":15,"total_cached_tokens":0,"total_output_tokens":1,"total_thought_tokens":56}`)))
	}))
	defer geminiSrv.Close()
	geminiClient := gemini.NewClient(geminiSrv.URL, gemini.WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))

	var toolCallSent atomic.Bool
	deepseekSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if toolCallSent.CompareAndSwap(false, true) {
			// First sub-turn: a ReviewScreenshot call.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call-review", Type: "function", Function: wire.ToolCallFuncDelta{Name: "ReviewScreenshot", Arguments: `{"image_paths":["shot.png"],"question":"what is wrong?"}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		} else {
			// Second sub-turn: a plain answer that ends the run.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("all done")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer deepseekSrv.Close()

	r := newTestRunner(t, deepseekSrv.URL)
	r.Gemini = geminiClient

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "shot.png"), []byte("not really a png"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "review the screenshot",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
	}

	summaries, err := r.Store.SessionUsageSummaries(t.Context(), []string{res.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	sum := summaries[res.SessionID]

	deepseekCost := 2 * (float64(100)/1e6*0.003625 + float64(100)/1e6*0.435 + float64(5)/1e6*0.87)
	// Derived from the table the runner actually costs against rather than
	// from the rates written out again here. What this test is for is that a
	// Gemini call reaches the session total at all; restating the rates made
	// it fail every time they changed, which reads as the accounting breaking
	// when it is only the price of a token moving.
	geminiPrices := r.Prices.Models[gemini.DefaultModel]
	geminiCost := float64(15)/1e6*geminiPrices.InputCacheMissPerMillionUSD + float64(57)/1e6*geminiPrices.OutputPerMillionUSD
	if math.Abs(sum.CostUSD-(deepseekCost+geminiCost)) > 1e-9 {
		t.Fatalf("session cost = %.9f, want %.9f (deepseek %.9f + gemini %.9f)", sum.CostUSD, deepseekCost+geminiCost, deepseekCost, geminiCost)
	}
	if sum.CompletionTokens != 2*5+57 {
		t.Errorf("completion total = %d, want %d (2 deepseek turns of 5 + gemini 57)", sum.CompletionTokens, 2*5+57)
	}
	if sum.ReasoningTokens != 56 {
		t.Errorf("reasoning total = %d, want 56 (the Gemini thought tokens)", sum.ReasoningTokens)
	}
	if sum.PromptCacheMissTokens != 2*100+15 {
		t.Errorf("cache-miss total = %d, want %d", sum.PromptCacheMissTokens, 2*100+15)
	}
}

// TestTaskCreateOrderingWithinSubTurn is the concurrency fix's own test: two
// TaskCreate calls in one sub-turn must mint ids in the order the calls
// appear in the array, because the store's plan replay
// (internal/store/status.go RequestStatus) and the frontend both reconstruct
// ids by walking the event log in that same order. If the two calls raced
// for the executor's lock, whichever goroutine won would mint the lower id
// and the model would hold ids nobody else can reconstruct. Run with -race.
func TestTaskCreateOrderingWithinSubTurn(t *testing.T) {
	r := newTestRunner(t, "http://127.0.0.1:1") // no request is ever made
	executor, err := tools.NewExecutor(t.TempDir(), &tools.Policy{Mode: tools.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	calls := []wire.AssembledToolCall{
		{ID: "call_00_first", Name: "TaskCreate", Arguments: `{"tasks":[{"subject":"First task","description":"First thing","activeForm":"Firsting"}]}`},
		{ID: "call_01_second", Name: "TaskCreate", Arguments: `{"tasks":[{"subject":"Second task","description":"Second thing","activeForm":"Seconding"}]}`},
	}
	outcomes := r.executeToolCalls(t.Context(), store.Session{ID: "sess-order"}, executor, calls)

	// Each TaskCreate renders the whole checklist, so the first outcome
	// carries only its own task — the lower id — and the second outcome
	// carries both, with its new task under a higher id.
	if first := outcomes[0].Result.Content; !strings.Contains(first, "#1 First task") || strings.Contains(first, "#2") {
		t.Fatalf("first TaskCreate rendered %q, want its own task with the lower id only", first)
	}
	if second := outcomes[1].Result.Content; !strings.Contains(second, "#1 First task") || !strings.Contains(second, "#2 Second task") {
		t.Fatalf("second TaskCreate rendered %q, want both tasks with the second's id higher", second)
	}
}

// editBatchFixture writes a file, marks it read so Edit will accept it, and
// returns the runner, executor and path the same-file tests share.
func editBatchFixture(t *testing.T, content string) (*Runner, *tools.Executor, string) {
	t.Helper()
	r := newTestRunner(t, "http://127.0.0.1:1") // no request is ever made
	dir := t.TempDir()
	path := filepath.Join(dir, "styles.css")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	executor, err := tools.NewExecutor(dir, &tools.Policy{Mode: tools.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	// Edit refuses a file this session has not read.
	read := executor.Execute(t.Context(), wire.ToolCall{
		ID: "call_read", Type: "function",
		Function: wire.ToolCallFunc{Name: "Read", Arguments: `{"file_path":"styles.css"}`},
	})
	if read.Result.IsError {
		t.Fatalf("Read fixture: %s", read.Result.Content)
	}
	return r, executor, path
}

// TestSameFileEditsRunInOrder is the fix's own test for the case that used to
// lose work: two Edits to one file in one sub-turn. Run concurrently, each
// reads the file, applies its own replacement and writes the whole thing
// back, so one change is silently dropped and the model is told both
// succeeded. Ordered, both land. The second call names the file differently
// on purpose — resolution is what makes the two recognisable as one file.
// Run with -race.
func TestSameFileEditsRunInOrder(t *testing.T) {
	r, executor, path := editBatchFixture(t, "alpha\nbravo\n")
	calls := []wire.AssembledToolCall{
		{ID: "call_00_alpha", Name: "Edit", Arguments: `{"file_path":"styles.css","old_string":"alpha","new_string":"ALPHA"}`},
		{ID: "call_01_bravo", Name: "Edit", Arguments: `{"file_path":"./styles.css","old_string":"bravo","new_string":"BRAVO"}`},
	}
	outcomes := r.executeToolCalls(t.Context(), store.Session{ID: "sess-edits"}, executor, calls)

	for i, o := range outcomes {
		if o.Result.IsError {
			t.Fatalf("edit %d failed: %s", i, o.Result.Content)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ALPHA\nBRAVO\n" {
		t.Fatalf("file = %q, want both edits applied", got)
	}
}

// TestSameFileEditsClashExplainsWhy covers the case ordering cannot rescue:
// the second Edit was written against text the first one rewrote, so its
// old_string is genuinely gone. It has to fail — but "old_string not found"
// alone sends the model hunting for a typo in text that was correct when it
// wrote it, so the outcome carries the reason.
func TestSameFileEditsClashExplainsWhy(t *testing.T) {
	r, executor, path := editBatchFixture(t, "alpha bravo\n")
	calls := []wire.AssembledToolCall{
		{ID: "call_00_wide", Name: "Edit", Arguments: `{"file_path":"styles.css","old_string":"alpha bravo","new_string":"charlie"}`},
		{ID: "call_01_stale", Name: "Edit", Arguments: `{"file_path":"styles.css","old_string":"bravo","new_string":"BRAVO"}`},
	}
	outcomes := r.executeToolCalls(t.Context(), store.Session{ID: "sess-clash"}, executor, calls)

	if outcomes[0].Result.IsError {
		t.Fatalf("first edit should have applied: %s", outcomes[0].Result.Content)
	}
	if !outcomes[1].Result.IsError {
		t.Fatal("second edit should have failed: its old_string was rewritten by the first")
	}
	if !strings.Contains(outcomes[1].Result.Content, "earlier call in this same message") {
		t.Fatalf("clash result = %q, want it to name the earlier call as the cause", outcomes[1].Result.Content)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "charlie\n" {
		t.Fatalf("file = %q, want only the first edit applied", got)
	}
}

// TestDifferentFileEditsStayConcurrent guards the other half: only a real
// collision is ordered. Two edits to two files must not be serialised, or the
// fix would have cost the parallelism it was carving an exception out of.
func TestDifferentFileEditsStayConcurrent(t *testing.T) {
	r, executor, _ := editBatchFixture(t, "alpha\n")
	other := filepath.Join(executor.Workspace, "other.css")
	if err := os.WriteFile(other, []byte("bravo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := executor.Execute(t.Context(), wire.ToolCall{
		ID: "call_read2", Type: "function",
		Function: wire.ToolCallFunc{Name: "Read", Arguments: `{"file_path":"other.css"}`},
	})
	if read.Result.IsError {
		t.Fatalf("Read other.css: %s", read.Result.Content)
	}
	calls := []wire.AssembledToolCall{
		{ID: "call_00", Name: "Edit", Arguments: `{"file_path":"styles.css","old_string":"alpha","new_string":"ALPHA"}`},
		{ID: "call_01", Name: "Edit", Arguments: `{"file_path":"other.css","old_string":"bravo","new_string":"BRAVO"}`},
	}
	outcomes := r.executeToolCalls(t.Context(), store.Session{ID: "sess-two-files"}, executor, calls)
	for i, o := range outcomes {
		if o.Result.IsError {
			t.Fatalf("edit %d failed: %s", i, o.Result.Content)
		}
		if strings.Contains(o.Result.Content, "earlier call in this same message") {
			t.Fatalf("edit %d was treated as a collision; different files must not be grouped", i)
		}
	}
}

// TestTaskSubagentRunsAtMaxEffort pins the subagent's hardcoded effort:
// subagentRunner launches its nested flash session at max effort, matching
// docs/MODELS.md's `Task` subagent row. The child session row stores the
// RunOptions effort verbatim, so a regression shows up as the wrong value on
// the child rather than passing silently.
func TestTaskSubagentRunsAtMaxEffort(t *testing.T) {
	srv := plainAnswerServer(t, "subagent answer")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	run := r.subagentRunner("parent-sess", RunOptions{PermissionMode: tools.ModeFull}, ws)
	text, childID, err := run(t.Context(), "look something up", "find it", "general")
	if err != nil {
		t.Fatalf("subagent run: %v", err)
	}
	if text != "subagent answer" {
		t.Fatalf("expected the subagent's answer text, got %q", text)
	}

	child, err := r.Store.GetSession(t.Context(), childID)
	if err != nil {
		t.Fatalf("expected the child session to exist in the store: %v", err)
	}
	if child.Effort != wire.EffortMax {
		t.Fatalf("expected the subagent to run at effort %q, got %q", wire.EffortMax, child.Effort)
	}
	if child.Model != "test-model" {
		t.Fatalf("expected the subagent to run the flash model, got %q", child.Model)
	}
}

// kimiAnswerServer answers like Kimi K3 for one run: streaming
// reasoning_content then content, and a usage frame carrying Kimi's single
// cached_tokens figure instead of DeepSeek's hit/miss pair
// (third_party/kimi-docs/api/chat.md). The request body must not carry a
// thinking field — sending it to kimi-k3 is an error
// (third_party/kimi-docs/api/models-overview.md).
func kimiAnswerServer(t *testing.T, answer string, kimiRequests *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kimiRequests.Add(1)
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"thinking"`) {
			t.Errorf("kimi-k3 request body carries a thinking field: %s", body)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Content: strPtr(""), ReasoningContent: strPtr("thinking hard")}}},
		})
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Content: strPtr(answer)}}},
		})
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
			Usage:   &wire.Usage{PromptTokens: 200, CachedTokens: 100, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
}

// TestClientForRoutesByModel pins the per-model client wiring: a Runner
// with ClientFor serves whichever provider the request's model belongs to.
// A kimi-k3 run must reach the Kimi client — whose request carries no
// thinking field and whose cached_tokens splits into the stored usage event
// as hit 100 / miss 100 — and never the DeepSeek client
// (docs/KIMI-INTEGRATION.md §4.3).
func TestClientForRoutesByModel(t *testing.T) {
	var deepSeekRequests atomic.Int64
	deepSeekSrv := plainAnswerServer(t, "from deepseek")
	defer deepSeekSrv.Close()
	var kimiRequests atomic.Int64
	kimiSrv := kimiAnswerServer(t, "from kimi", &kimiRequests)
	defer kimiSrv.Close()

	deepSeekClient := deepseek.NewClient(deepSeekSrv.URL, "test-key")
	kimiClient := kimi.NewClient(kimiSrv.URL, "test-key")

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	r := &Runner{
		Store:  st,
		Mirror: store.NewMirror(filepath.Join(dir, "mirror")),
		Client: deepSeekClient,
		ClientFor: func(model string) Client {
			if model == "kimi-k3" {
				return kimiClient
			}
			return deepSeekClient
		},
		Prices:     testPrices(),
		FlashModel: "deepseek-v4-flash",
	}

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "kimi-k3", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Text != "from kimi" {
		t.Fatalf("answer = %q, want %q (the Kimi client's)", res.Text, "from kimi")
	}
	if kimiRequests.Load() != 1 {
		t.Errorf("Kimi server saw %d requests, want 1", kimiRequests.Load())
	}
	if deepSeekRequests.Load() != 0 {
		t.Errorf("DeepSeek server saw %d requests, want 0 (kimi-k3 must not reach the DeepSeek client)", deepSeekRequests.Load())
	}

	// The usage event's cache split must come from Kimi's cached_tokens:
	// prompt 200, cached 100 → hit 100, miss 100.
	events, err := st.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind != store.KindUsage {
			continue
		}
		var u store.UsagePayload
		if err := json.Unmarshal(e.Payload, &u); err != nil {
			t.Fatalf("decode usage payload: %v", err)
		}
		if u.PromptCacheHitTokens != 100 || u.PromptCacheMissTokens != 100 {
			t.Errorf("usage split = hit %d miss %d, want 100/100 from cached_tokens 100 of prompt 200", u.PromptCacheHitTokens, u.PromptCacheMissTokens)
		}
	}
}
