package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// bashThenAnswerServer answers the first streamed sub-turn with a single
// Bash tool call and every one after that with a plain answer that ends the
// run, so a test can drive exactly one tool execution and inspect what it
// published.
func bashThenAnswerServer(t *testing.T, command, finalAnswer string) *httptest.Server {
	var call int32Counter
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: finalAnswer}, FinishReason: wire.FinishStop}},
				Usage:   &wire.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}

		idx := call.next()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role: "assistant",
					ToolCalls: []wire.ToolCallDelta{{
						Index: 0, ID: "call_bash", Type: "function",
						Function: wire.ToolCallFuncDelta{Name: "Bash", Arguments: `{"command":"` + command + `"}`},
					}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		} else {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr(finalAnswer)}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
}

// TestBashPublishesLiveStdoutWhenHubSet proves the wiring end to end: with a
// Hub attached, a Bash call's output reaches the store as tool_stdout events
// ahead of (or alongside) its terminal tool_result, which is what lets the
// browser show output as it happens (docs/DESIGN.md §5.2).
func TestBashPublishesLiveStdoutWhenHubSet(t *testing.T) {
	srv := bashThenAnswerServer(t, "echo hello-stream", "done")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)
	r.Hub = hub.New()

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "run a command",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	var stdoutText strings.Builder
	found := false
	for _, e := range events {
		if e.Kind != store.KindToolStdout {
			continue
		}
		found = true
		var p store.ToolStdoutPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.ToolCallID != "call_bash" {
			t.Fatalf("tool_stdout carries the wrong tool_call_id: %q", p.ToolCallID)
		}
		stdoutText.WriteString(p.Text)
	}
	if !found {
		t.Fatal("expected at least one tool_stdout event with a Hub attached")
	}
	if !strings.Contains(stdoutText.String(), "hello-stream") {
		t.Fatalf("expected the streamed text to contain the command's output, got %q", stdoutText.String())
	}
}

// TestBashSkipsStdoutEventsWithoutHub confirms the sink is only wired when
// something can read it — the CLI's normal case (no Hub) should not pay for
// tool_stdout commits nobody subscribes to.
func TestBashSkipsStdoutEventsWithoutHub(t *testing.T) {
	srv := bashThenAnswerServer(t, "echo quiet", "done")
	defer srv.Close()
	r := newTestRunner(t, srv.URL) // r.Hub is nil

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "run a command",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Kind == store.KindToolStdout {
			t.Fatal("did not expect any tool_stdout events with no Hub attached")
		}
	}
}

// TestTaskResultCarriesChildSessionID drives a parent session whose one
// tool call is Task, delegating to a nested subagent run against the same
// mock server, and asserts the parent's tool_result names the child session
// it spawned — what the browser needs to render it as a collapsed child
// transcript (docs/TOOLS.md "Task").
func TestTaskResultCarriesChildSessionID(t *testing.T) {
	var call int32Counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: "parent done"}, FinishReason: wire.FinishStop}},
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
			// Parent sub-turn 1: delegate to a subagent via Task.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role: "assistant",
					ToolCalls: []wire.ToolCallDelta{{
						Index: 0, ID: "call_task", Type: "function",
						Function: wire.ToolCallFuncDelta{Name: "Task", Arguments: `{"description":"look something up","prompt":"find it","subagent_type":"general"}`},
					}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		case 1:
			// The subagent's own single sub-turn: a plain answer, ending its run.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("subagent answer")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 60, PromptCacheMissTokens: 60, CompletionTokens: 5},
			})
		default:
			// Parent sub-turn 2, now with the Task result in hand: a plain
			// answer that ends the parent's run.
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("parent done")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 300, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
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
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "delegate this",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var childID string
	for _, e := range events {
		if e.Kind != store.KindToolResult {
			continue
		}
		var p store.ToolResultPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Name == "Task" {
			childID = p.ChildSessionID
		}
	}
	if childID == "" {
		t.Fatal("expected the Task tool_result to carry a non-empty child_session_id")
	}
	if childID == res.SessionID {
		t.Fatal("expected the child session id to differ from the parent's")
	}

	child, err := r.Store.GetSession(t.Context(), childID)
	if err != nil {
		t.Fatalf("expected the child session to exist in the store: %v", err)
	}
	if child.ParentID != res.SessionID {
		t.Fatalf("expected the child's parent_id to point at the parent session, got %q", child.ParentID)
	}
}
