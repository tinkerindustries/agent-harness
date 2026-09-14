package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
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
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
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

// TestResumeKeepsVariantHeadAndToolArray pins the resume-variant fix: a
// session that ran under a tool-dropping variant must, when resumed, keep
// sending the variant's tool array and rendering the variant's head — the
// same array and head it sent before it was interrupted. The variant name
// is frozen on the session row (store.Session.PromptVariant) and Resume
// resolves through it, so a resumed arm of an eval stays that arm instead
// of silently reverting to the provider's full array while nothing warns
// anyone.
func TestResumeKeepsVariantHeadAndToolArray(t *testing.T) {
	var mu sync.Mutex
	var streamed [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if probe.Stream {
			mu.Lock()
			streamed = append(streamed, body)
			mu.Unlock()
		}
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
	ws := t.TempDir()
	first, err := r.Run(t.Context(), RunOptions{
		Model: "deepseek-v4-pro", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "first task",
		PromptVariant: "no-bash",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", first.Status)
	}

	sess, err := r.Store.GetSession(t.Context(), first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.PromptVariant != "no-bash" {
		t.Fatalf("session row does not record the variant: got %q, want %q", sess.PromptVariant, "no-bash")
	}

	resumed, err := r.Resume(t.Context(), ResumeOptions{
		SessionID: first.SessionID, Prompt: "keep going", MaxTokens: 4000,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK {
		t.Fatalf("expected the resumed run to finish ok, got %s", resumed.Status)
	}

	// Two streamed requests: the first run's and the resumed run's. Both
	// must carry the no-bash array and the no-bash head — identical to each
	// other, and identical to what the variant asks for.
	mu.Lock()
	defer mu.Unlock()
	if len(streamed) != 2 {
		t.Fatalf("expected 2 streamed requests (run then resume), got %d", len(streamed))
	}
	wantHead, err := RenderSystemPromptFor("deepseek-v4-pro", "no-bash")
	if err != nil {
		t.Fatal(err)
	}
	wantTools := tools.DefinitionsForVariant("deepseek-v4-pro", "no-bash")
	wantNames := make([]string, len(wantTools))
	for i, tool := range wantTools {
		wantNames[i] = tool.Function.Name
	}
	for i, body := range streamed {
		var req struct {
			Tools []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tools"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("request %d: decode body: %v", i+1, err)
		}
		if len(req.Tools) != len(wantNames) {
			t.Fatalf("request %d sends %d tools, want the no-bash array's %d", i+1, len(req.Tools), len(wantNames))
		}
		for j, tool := range req.Tools {
			if tool.Function.Name != wantNames[j] {
				t.Fatalf("request %d tool %d is %q, want %q (the no-bash array's order)", i+1, j, tool.Function.Name, wantNames[j])
			}
		}
		for _, name := range wantNames {
			if name == "Bash" {
				t.Fatal("the no-bash array must not contain Bash")
			}
		}
		if len(req.Messages) == 0 || req.Messages[0].Role != "system" {
			t.Fatalf("request %d has no system message", i+1)
		}
		if req.Messages[0].Content != wantHead {
			t.Fatalf("request %d renders a different head from the no-bash one:\n--- got ---\n%s\n--- want ---\n%s",
				i+1, req.Messages[0].Content, wantHead)
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
		ID: "sess-running", Model: "test-model", Effort: wire.EffortHigh, Workspace: t.TempDir(),
		PermissionMode: string(tools.ModeFull), SystemPrompt: RenderSystemPrompt(),
		ToolSchema: json.RawMessage(`[]`), Status: store.StatusRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resume(ctx, ResumeOptions{SessionID: "sess-running"}); err == nil {
		t.Fatal("expected Resume to refuse a running session")
	}

	if err := r.Store.CreateSession(ctx, store.Session{
		ID: "sess-compacted", Model: "test-model", Effort: wire.EffortHigh, Workspace: t.TempDir(),
		PermissionMode: string(tools.ModeFull), SystemPrompt: RenderSystemPrompt(),
		ToolSchema: json.RawMessage(`[]`), Status: store.StatusCompacted,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resume(ctx, ResumeOptions{SessionID: "sess-compacted"}); err == nil {
		t.Fatal("expected Resume to refuse a compacted session")
	}
}

// TestResumeSucceedsAfterAStop pins the fix for a session that could never be
// continued once a client stopped it mid-turn — not in this process, and not
// after a restart. store.AppendEvents refuses to write to a session whose
// status is "cancelled" (the fence that stops a wedged goroutine from
// dirtying the log it was stopped in, docs/RUN-CONTROL.md "Half two"), and
// Resume used to call it before lifting the row back to running, so a
// genuine resume tripped the exact fence meant for somebody else's write —
// "session is cancelled" on the very next message. This cancels a run the
// way a stop does, by cancelling its context mid-request
// (TestRunCancelMarksSessionCancelled's own pattern), then resumes the
// session and asserts the continuation actually lands rather than refusing.
func TestResumeSucceedsAfterAStop(t *testing.T) {
	var calls int32Counter
	started := make(chan struct{})
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.next() == 0 {
			close(started)
			<-block // held open until the test cancels the run
			return
		}

		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("resumed answer")}, FinishReason: wire.FinishStop}},
				Usage:   &wire.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("resumed answer")}}},
		})
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
			Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
		})
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	defer close(block)
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, RunOptions{
			SessionID: "sess-stop-resume", Model: "test-model", Effort: wire.EffortHigh,
			Thinking: true, MaxTokens: 4000,
			Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "do the thing",
		})
		errCh <- err
	}()

	// Wait until the run's request is in flight, then cancel the context the
	// way a stop does (the parent's cancelRun cancels the run's own context).
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

	sess, err := r.Store.GetSession(t.Context(), "sess-stop-resume")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusCancelled {
		t.Fatalf("session status = %q, want %q before resuming it", sess.Status, store.StatusCancelled)
	}

	resumed, err := r.Resume(t.Context(), ResumeOptions{
		SessionID: "sess-stop-resume", Prompt: "keep going", MaxTokens: 4000,
	})
	if err != nil {
		t.Fatalf("Resume after a stop: %v", err)
	}
	if resumed.Status != store.StatusOK {
		t.Fatalf("expected the resumed run to finish ok, got %s: %+v", resumed.Status, resumed)
	}
	if resumed.Text != "resumed answer" {
		t.Fatalf("expected the resumed answer text, got %q", resumed.Text)
	}

	events, err := r.Store.GetEvents(t.Context(), "sess-stop-resume")
	if err != nil {
		t.Fatal(err)
	}
	var starts []store.SessionStartedPayload
	for _, e := range events {
		if e.Kind != store.KindSessionStarted {
			continue
		}
		var p store.SessionStartedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		starts = append(starts, p)
	}
	if len(starts) != 2 {
		t.Fatalf("expected the stopped run's opening message plus the resume's continuation, got %d session_started events", len(starts))
	}
	if starts[1].OpeningMessage != "keep going" {
		t.Fatalf("expected the resume prompt as the continuation's opening message, got %q", starts[1].OpeningMessage)
	}

	final, err := r.Store.GetSession(t.Context(), "sess-stop-resume")
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != store.StatusOK {
		t.Fatalf("expected the session to finish ok after the resume, got %s", final.Status)
	}
}

// TestResumeOfASessionCancelledBeforeItsFirstTurnDoesNotHang pins a second
// bug the first one was hiding: a stop that lands before the run's first
// turn_started event ever commits leaves the session with zero committed
// sub-turns, so a resume's startSubTurn (countTurns(events)+1) is 1 — the
// same value a fresh, blank browser-start's is. runLoop's wait for the first
// steer (lifecycle.go, "A browser-started run is created with no prompt")
// used to key off exactly that: startSubTurn == 1 and opts.Prompt == "". A
// resumed run's RunOptions never carries opts.Prompt regardless of what the
// resume's own ResumeOptions.Prompt held (deliberately — see
// RunOptions.session's doc comment), so both halves of that condition could
// be true for a resume with every right to run, and it would block forever
// on a steer nobody is going to send. RunOptions.Resuming is what tells
// runLoop this is not the blank-start case. Before that fix existed, this
// session: session is cancelled fix alone would have turned one hang
// (an error on every resume attempt) into a different one (resume accepted,
// then stuck forever) for exactly this timing — worse, not better. Bounded
// with its own short deadline so a regression here fails this test rather
// than hanging the suite.
func TestResumeOfASessionCancelledBeforeItsFirstTurnDoesNotHang(t *testing.T) {
	srv := plainAnswerServer(t, "after the stop")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)
	ctx := t.Context()

	cancelledAt := time.Now().UTC()
	if err := r.Store.CreateSession(ctx, store.Session{
		ID: "sess-never-started", Model: "test-model", Effort: wire.EffortHigh,
		Workspace: t.TempDir(), PermissionMode: string(tools.ModeFull),
		SystemPrompt: RenderSystemPrompt(), ToolSchema: json.RawMessage(`[]`),
		Status: store.StatusCancelled, FinishedAt: &cancelledAt,
	}); err != nil {
		t.Fatal(err)
	}

	resumeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resumed, err := r.Resume(resumeCtx, ResumeOptions{
		SessionID: "sess-never-started", Prompt: "start over", MaxTokens: 4000,
	})
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("Resume hung waiting for a steer that was never going to arrive")
		}
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK {
		t.Fatalf("expected the resumed run to finish ok, got %s: %+v", resumed.Status, resumed)
	}
	if resumed.Text != "after the stop" {
		t.Fatalf("expected the resumed answer text, got %q", resumed.Text)
	}
}

// TestResumeWithoutPromptContinuesTheExistingTask exercises Resume with an
// empty Prompt: pick the loop back up with no new user message appended. The
// session row carries max_turns, the status an earlier binary wrote for a run
// that reached its sub-turn ceiling, which a row in a kept state directory can
// still hold and which must still resume.
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
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("n/a")}, FinishReason: wire.FinishStop}},
			}
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// First sub-turn: a tool call that keeps the loop going.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_0", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheHitTokens: 0, PromptCacheMissTokens: 300, CompletionTokens: 5},
			})
		} else if n == 2 {
			// The first run's second request fails, ending it between
			// sub-turns with one turn in the log.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"end the first run here","type":"invalid_request_error"}}`)
			return
		} else {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("done now")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 350, PromptCacheHitTokens: 256, PromptCacheMissTokens: 94, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	ws := t.TempDir()
	first, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "start a task",
	})
	if err == nil {
		t.Fatal("Run: expected the first run to fail on its second request")
	}
	if first.SubTurns != 1 {
		t.Fatalf("expected the first run to end after one sub-turn, got %d", first.SubTurns)
	}
	finished := time.Now().UTC()
	if err := r.Store.UpdateSessionStatus(t.Context(), first.SessionID, store.StatusMaxTurns, &finished); err != nil {
		t.Fatal(err)
	}
	loaded, err := r.Store.GetSession(t.Context(), first.SessionID)
	if err != nil {
		t.Fatalf("load a max_turns row: %v", err)
	}
	if loaded.Status != store.StatusMaxTurns {
		t.Fatalf("stored status = %q, want %q", loaded.Status, store.StatusMaxTurns)
	}

	resumed, err := r.Resume(t.Context(), ResumeOptions{SessionID: first.SessionID, MaxTokens: 4000})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK || resumed.Text != "done now" {
		t.Fatalf("unexpected resumed result: %+v", resumed)
	}
	// The failed request opened sub-turn 2 before it failed, so the resumed
	// run's numbering continues at 3.
	if resumed.SubTurns != 3 {
		t.Fatalf("expected the resumed run to finish at sub-turn 3, got %d", resumed.SubTurns)
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

// TestCompactionAfterResumeKeepsProvenanceFields pins a regression compact
// once had: Runner.Resume builds its RunOptions from the session row and
// deliberately leaves Title, Description, Phase, TotalPhases, and Prompt
// unset (resume.go — none of those is a resumed run's to choose again). A
// version of compact that built the successor row from that RunOptions via
// RunOptions.session was correct for a run that reached compaction through
// Runner.Run but silently blanked those five fields for a run that reached
// it through Runner.Resume. compact now copies the session row itself,
// which cannot have that problem.
//
// The first run finishes normally (a plain answer with no tool calls);
// Resume then continues it and, on its first sub-turn, reports usage over
// a deliberately low compaction threshold, forcing a fork mid-resume — the
// only way to observe what the successor row inherited.
func TestCompactionAfterResumeKeepsProvenanceFields(t *testing.T) {
	var call int32Counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)

		if !probe.Stream {
			// The compaction summary call.
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("summary of resumed work")}, FinishReason: wire.FinishStop}},
				Usage:   &wire.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := call.next()
		w.Header().Set("Content-Type", "text/event-stream")
		switch idx {
		case 0:
			// The first run's only sub-turn: a plain answer, ends cleanly.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("first answer")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 100, CompletionTokens: 5},
			})
		case 1:
			// The resumed run's first sub-turn: a tool call (so the loop
			// does not finish on "no tool calls" before reaching the
			// compaction check) with usage over the low threshold.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_00_a", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 900, CompletionTokens: 5},
			})
		default:
			// The compacted successor's sub-turn: a plain answer that ends
			// the (resumed) run.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("continued after resume")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 300, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.CompactionThresholdTokens = 500

	ws := t.TempDir()
	first, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "the original prompt",
		Title: "Port the vision tools", Description: "a longer description",
		Phase: 2, TotalPhases: 5,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.Status != store.StatusOK {
		t.Fatalf("expected the first run to finish ok, got %s: %+v", first.Status, first)
	}

	resumed, err := r.Resume(t.Context(), ResumeOptions{
		SessionID: first.SessionID, Prompt: "keep going", MaxTokens: 4000,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK {
		t.Fatalf("expected the resumed run to finish ok, got %s: %+v", resumed.Status, resumed)
	}
	if resumed.SessionID == first.SessionID {
		t.Fatalf("expected the low threshold to force a compaction fork, got the same session id back")
	}

	child, err := r.Store.GetSession(t.Context(), resumed.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if child.ParentID != first.SessionID {
		t.Fatalf("expected the child's parent_id to point at the resumed session, got %q", child.ParentID)
	}
	if child.Task != "the original prompt" {
		t.Fatalf("expected the child to carry the original task, got %q", child.Task)
	}
	if child.Title != "Port the vision tools" {
		t.Fatalf("expected the child to carry the parent's title, got %q", child.Title)
	}
	if child.Description != "a longer description" {
		t.Fatalf("expected the child to carry the parent's description, got %q", child.Description)
	}
	if child.Phase != 2 || child.TotalPhases != 5 {
		t.Fatalf("expected the child to carry the parent's phase 2/5, got %d/%d", child.Phase, child.TotalPhases)
	}
}

// TestResumeMaterialisesAttachmentsBeforeTheContinuation is the image half of
// continuing a session (docs/RUN-CONTROL.md, "Images in the composer"). A
// resumed session keeps the workspace it already has, so nothing prepares one
// for it and Resume writes the continuation's images into that existing
// workspace itself, before appending the message that names them.
//
// Three things are pinned, and the split between the last two is the point:
// the file lands under scratch/attachments/; the message the model reads
// names that path; and the payload's task keeps the person's own words alone,
// so the transcript never reads the file names back at whoever pasted them.
func TestResumeMaterialisesAttachmentsBeforeTheContinuation(t *testing.T) {
	srv := plainAnswerServer(t, "first answer")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	first, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "first task",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	png := []byte("\x89PNG fake bytes")
	id, err := r.Store.WriteAttachment(t.Context(), "mockup.png", "image/png", png)
	if err != nil {
		t.Fatalf("write attachment: %v", err)
	}
	if _, err := r.Resume(t.Context(), ResumeOptions{
		SessionID: first.SessionID, Prompt: "match this mockup", AttachmentIDs: []string{id}, MaxTokens: 4000,
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(ws, "scratch", "attachments", "mockup.png"))
	if err != nil {
		t.Fatalf("read materialised attachment: %v", err)
	}
	if string(got) != string(png) {
		t.Errorf("materialised bytes = %q, want %q", got, png)
	}

	events, err := r.Store.GetEvents(t.Context(), first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var starts []store.SessionStartedPayload
	for _, e := range events {
		if e.Kind != store.KindSessionStarted {
			continue
		}
		var p store.SessionStartedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		starts = append(starts, p)
	}
	if len(starts) != 2 {
		t.Fatalf("expected 2 session_started events, got %d", len(starts))
	}
	cont := starts[1]
	if !strings.Contains(cont.OpeningMessage, "scratch/attachments/mockup.png") {
		t.Errorf("the continuation the model reads does not name the attachment:\n%s", cont.OpeningMessage)
	}
	if !strings.HasSuffix(cont.OpeningMessage, "match this mockup") {
		t.Errorf("the words should come after the attachment block:\n%s", cont.OpeningMessage)
	}
	if cont.Task != "match this mockup" {
		t.Errorf("task = %q, want the person's own words alone", cont.Task)
	}
	if len(cont.Attachments) != 1 || cont.Attachments[0] != "scratch/attachments/mockup.png" {
		t.Errorf("attachments = %v, want the one workspace path the browser addresses", cont.Attachments)
	}
}

// TestResumeWithImagesAndNoWordsIsAMessage pins the empty-text case: pasting
// a screenshot and sending it without typing anything is a complete thing to
// say, and the continuation it produces is the attachment block alone rather
// than nothing at all — a resume with neither words nor images still appends
// no session_started, which is the shape a run stopped between sub-turns
// needs.
func TestResumeWithImagesAndNoWordsIsAMessage(t *testing.T) {
	srv := plainAnswerServer(t, "first answer")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	first, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "first task",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	id, err := r.Store.WriteAttachment(t.Context(), "mockup.png", "image/png", []byte("png"))
	if err != nil {
		t.Fatalf("write attachment: %v", err)
	}
	if _, err := r.Resume(t.Context(), ResumeOptions{
		SessionID: first.SessionID, AttachmentIDs: []string{id}, MaxTokens: 4000,
	}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	events, err := r.Store.GetEvents(t.Context(), first.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	var last store.SessionStartedPayload
	for _, e := range events {
		if e.Kind != store.KindSessionStarted {
			continue
		}
		starts++
		if err := json.Unmarshal(e.Payload, &last); err != nil {
			t.Fatal(err)
		}
	}
	if starts != 2 {
		t.Fatalf("expected the images alone to append a continuation, got %d session_started events", starts)
	}
	if !strings.Contains(last.OpeningMessage, "scratch/attachments/mockup.png") {
		t.Errorf("the wordless continuation should still name the image:\n%s", last.OpeningMessage)
	}
}
