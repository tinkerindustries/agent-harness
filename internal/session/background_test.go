package session

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// backgroundThenPollServer scripts a session across two runs: the first
// sub-turn starts command in the background and the second ends the run, then
// the next run's first sub-turn reads the shell with BashOutput. The id is
// bash_1 because the registry the test hands in is fresh, which is the one
// thing about the id a scripted request can know in advance.
func backgroundThenPollServer(t *testing.T, command, firstAnswer, polledAnswer string) *httptest.Server {
	t.Helper()
	var call int32Counter
	startArgs, err := json.Marshal(map[string]any{"command": command, "run_in_background": true})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("plain")}, FinishReason: wire.FinishStop}},
				Usage:   &wire.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := call.next()
		w.Header().Set("Content-Type", "text/event-stream")
		writeToolCall := func(id, name, args string) {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role: "assistant",
					ToolCalls: []wire.ToolCallDelta{{
						Index: 0, ID: id, Type: "function",
						Function: wire.ToolCallFuncDelta{Name: name, Arguments: args},
					}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		answer := func(text string) {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr(text)}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}

		switch idx {
		case 0:
			writeToolCall("call_bash", "Bash", string(startArgs))
		case 1:
			answer(firstAnswer)
		case 2:
			writeToolCall("call_poll", "BashOutput", `{"bash_id":"bash_1"}`)
		default:
			answer(polledAnswer)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
}

// TestBackgroundShellSurvivesTheRunForTheNextResume is fix one: a shell a
// Bash call started in one run is still addressable by the next run's
// BashOutput. Before the session-scoped registry, the run's deferred
// Executor.Close killed it and the resume answered "no background shell".
func TestBackgroundShellSurvivesTheRunForTheNextResume(t *testing.T) {
	srv := backgroundThenPollServer(t, "echo still-here; sleep 30", "started", "polled")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)
	registry := tools.NewShells("sess-test", nil)
	defer registry.Close()

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "start it", Shells: registry,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected the first run to end ok, got %s", res.Status)
	}

	resumed, err := r.Resume(t.Context(), ResumeOptions{
		SessionID: res.SessionID, MaxTokens: 4000, Shells: registry,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK {
		t.Fatalf("expected the resume to end ok, got %s", resumed.Status)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var polled, started bool
	for _, e := range events {
		if e.Kind != store.KindToolResult {
			continue
		}
		var p store.ToolResultPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(p.Content, "Running in the background with id bash_1") {
			started = true
		}
		if p.Name == "BashOutput" {
			polled = true
			if !strings.HasPrefix(p.Content, "Status: running") {
				t.Fatalf("expected the resumed run to find the shell still running, got: %s", p.Content)
			}
		}
	}
	if !started {
		t.Fatal("the first run never started the background shell")
	}
	if !polled {
		t.Fatal("the resumed run never called BashOutput")
	}
}

// heldToolCallServer starts a background shell on its first sub-turn and
// stalls the next request for stall, so a run with a shorter deadline is
// stopped while the shell it started is still alive. The stall is bounded so
// a test server can still be closed.
func heldToolCallServer(t *testing.T, stall time.Duration) *httptest.Server {
	t.Helper()
	var call int32Counter
	startArgs, err := json.Marshal(map[string]any{"command": "sleep 30", "run_in_background": true})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := call.next()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role: "assistant",
					ToolCalls: []wire.ToolCallDelta{{
						Index: 0, ID: "call_bash", Type: "function",
						Function: wire.ToolCallFuncDelta{Name: "Bash", Arguments: string(startArgs)},
					}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 100, CompletionTokens: 5},
			})
			fmt.Fprint(w, "data: [DONE]\n\n")
			w.(http.Flusher).Flush()
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(stall):
		}
	}))
}

// TestPrivateShellRegistryRecordsACompletion pins the fallback for a caller
// with no session registry, which is the Task subagent path: Run builds a
// registry for the run, so a shell that exits mid-run still commits a
// background_finished event even though no registry outlives the run.
func TestPrivateShellRegistryRecordsACompletion(t *testing.T) {
	var call int32Counter
	startArgs, err := json.Marshal(map[string]any{"command": "echo done", "run_in_background": true})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := call.next()
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSEChunk(t, w, wire.ChatCompletionChunk{
			Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant"}}},
		})
		if idx == 0 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{ToolCalls: []wire.ToolCallDelta{{
					Index: 0, ID: "call_bash", Type: "function",
					Function: wire.ToolCallFuncDelta{Name: "Bash", Arguments: string(startArgs)},
				}}}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
			})
		} else {
			// Spend long enough on the wire that "echo done" has exited before
			// the run ends, so the private registry is still open when the
			// callback fires.
			time.Sleep(300 * time.Millisecond)
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Content: strPtr("started")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "start it",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind != store.KindBackgroundFinished {
			continue
		}
		var p store.BackgroundFinishedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.BashID != "bash_1" || p.ExitCode != 0 {
			t.Fatalf("unexpected completion payload: %+v", p)
		}
		return
	}
	t.Fatal("the run's private registry never committed the shell's completion")
}

// compactionShellServer scripts one background Bash call, a summariser that
// takes summarizerDelay before answering, and then a final answer. The first
// streaming request carries usage over the threshold the tests set, so the
// loop compacts right after that sub-turn.
func compactionShellServer(t *testing.T, command string, summarizerDelay time.Duration) *httptest.Server {
	t.Helper()
	var streamCalls int32Counter
	startArgs, err := json.Marshal(map[string]any{"command": command, "run_in_background": true})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			// The compaction summariser, thinking off, no tools. The delay is
			// what lets a short-lived shell exit while it runs.
			time.Sleep(summarizerDelay)
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("summary")}, FinishReason: wire.FinishStop}},
				Usage:   &wire.Usage{PromptTokens: 20, CompletionTokens: 5},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := streamCalls.next()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role: "assistant",
					ToolCalls: []wire.ToolCallDelta{{
						Index: 0, ID: "call_bash", Type: "function",
						Function: wire.ToolCallFuncDelta{Name: "Bash", Arguments: string(startArgs)},
					}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		} else {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("done")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 10, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
}

// TestBackgroundShellFollowsCompaction pins the mid-run case: a shell the
// model started before compaction is still the successor session's shell
// afterwards. The registry moves with the run, so BashOutput finds the id and
// any exit records against the row that is still running.
func TestBackgroundShellFollowsCompaction(t *testing.T) {
	srv := compactionShellServer(t, "sleep 30", 0)
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.CompactionThresholdTokens = 50
	registry := tools.NewShells("sess-original", nil)
	defer registry.Close()

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "start it",
		SessionID: "sess-original", Shells: registry,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID == "sess-original" {
		t.Fatal("the run did not compact, so this test proves nothing")
	}
	if got := registry.SessionID(); got != res.SessionID {
		t.Fatalf("the registry follows %q, but the run finished under %q", got, res.SessionID)
	}

	// A shell started before compaction is still addressable afterwards, the
	// same way the next run of the successor will address it.
	e, err := tools.NewExecutor(t.TempDir(), &tools.Policy{Mode: tools.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Shells = registry
	out := e.Execute(t.Context(), wire.ToolCall{
		ID: "poll", Type: "function",
		Function: wire.ToolCallFunc{Name: "BashOutput", Arguments: `{"bash_id":"bash_1"}`},
	})
	if !strings.HasPrefix(out.Result.Content, "Status: running") {
		t.Fatalf("expected the shell to survive compaction, got: %s", out.Result.Content)
	}
}

// TestBackgroundCompactionCarriesLateCompletion is the other half of the
// mid-run case: a shell that exits while compaction is summarising commits its
// notice to the retiring log, and the loop must carry it into the successor so
// the conversation that is still running sees it.
func TestBackgroundCompactionCarriesLateCompletion(t *testing.T) {
	srv := compactionShellServer(t, "sleep 0.1", 300*time.Millisecond)
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.CompactionThresholdTokens = 50

	// No registry: Run's private one records the shell's exit into the log,
	// which is the path this test is about.
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "start it",
		SessionID: "sess-original",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.SessionID == "sess-original" {
		t.Fatal("the run did not compact, so this test proves nothing")
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var found int
	for _, e := range events {
		if e.Kind != store.KindBackgroundFinished {
			continue
		}
		var p store.BackgroundFinishedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Command != "sleep 0.1" {
			t.Fatalf("unexpected carried notice: %+v", p)
		}
		found++
	}
	if found != 1 {
		t.Fatalf("the successor carries %d background completions, want the one that landed during compaction", found)
	}
}

// TestStoppedRunEndsItsBackgroundShells is the other half of fix one: a run
// that ends because it was stopped must not leave a shell behind. The
// registry is the session's, but a cancelled run closes it explicitly.
func TestStoppedRunEndsItsBackgroundShells(t *testing.T) {
	srv := heldToolCallServer(t, 5*time.Second)
	defer srv.Close()
	r := newTestRunner(t, srv.URL)
	registry := tools.NewShells("sess-test", nil)
	defer registry.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	res, _ := r.Run(ctx, RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "start it", Shells: registry,
	})
	if res != nil && res.Status == store.StatusOK {
		t.Fatalf("expected the timed-out run to end not-ok, got %s", res.Status)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	started := false
	for _, e := range events {
		if e.Kind == store.KindToolResult && strings.Contains(string(e.Payload), "Running in the background with id bash_1") {
			started = true
		}
	}
	if !started {
		t.Fatal("the run never started the background shell, so the stop proves nothing")
	}

	// A fresh Executor, as the next run would build, shares the registry. The
	// kill reaches the process group asynchronously, so poll for the status
	// rather than reading it once.
	e, err := tools.NewExecutor(t.TempDir(), &tools.Policy{Mode: tools.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Shells = registry
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := e.Execute(t.Context(), wire.ToolCall{
			ID: "poll", Type: "function",
			Function: wire.ToolCallFunc{Name: "BashOutput", Arguments: `{"bash_id":"bash_1"}`},
		})
		if !strings.HasPrefix(out.Result.Content, "Status: running") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected the timed-out run to have killed the shell, got: %s", out.Result.Content)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
