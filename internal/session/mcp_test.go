package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// fakeMCPProvider is a tools.MCPProvider test double: Definitions returns
// whatever defs/readOnly/err currently hold, mutable mid-test under mu so a
// test can simulate an operator enabling, disabling, or reconfiguring a
// server between a Run and a later Resume. instructions is the same for
// the initialize instructions Run quotes into the opening message, and
// instructionsErr fails that read alone — the two resolutions are separate
// calls on the seam, so a test can break one and leave the other working.
type fakeMCPProvider struct {
	mu              sync.Mutex
	defs            []wire.Tool
	readOnly        map[string]bool
	err             error
	instructions    map[string]string
	instructionsErr error
}

func (f *fakeMCPProvider) Definitions(ctx context.Context) ([]wire.Tool, map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.defs, f.readOnly, nil
}

func (f *fakeMCPProvider) Instructions(ctx context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.instructionsErr != nil {
		return nil, f.instructionsErr
	}
	return f.instructions, nil
}

func (f *fakeMCPProvider) set(defs []wire.Tool, readOnly map[string]bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.defs, f.readOnly = defs, readOnly
}

func (f *fakeMCPProvider) Call(ctx context.Context, toolName string, args json.RawMessage) (tools.MCPContent, error) {
	return tools.MCPContent{Text: "ok"}, nil
}

func (f *fakeMCPProvider) Resources(context.Context) ([]tools.MCPResource, error) { return nil, nil }

func (f *fakeMCPProvider) Prompts(context.Context) ([]tools.MCPPrompt, error) { return nil, nil }

func (f *fakeMCPProvider) ReadResource(context.Context, string, string) (tools.MCPContent, error) {
	return tools.MCPContent{}, nil
}

func (f *fakeMCPProvider) GetPrompt(context.Context, string, string, map[string]string) (tools.MCPContent, error) {
	return tools.MCPContent{}, nil
}

func mcpToolDef(name string) wire.Tool {
	return wire.Tool{Type: "function", Function: wire.ToolFunction{Name: name, Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}
}

// toolNames decodes a streamed request body's tools array into just their
// names, in order, for comparing against a wanted array.
func toolNames(t *testing.T, body []byte) []string {
	t.Helper()
	var req struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	names := make([]string, len(req.Tools))
	for i, tool := range req.Tools {
		names[i] = tool.Function.Name
	}
	return names
}

// mcpAccessToolNames are the four fixed tools tools.WithMCP inserts between
// the built-in array and a server's own whenever any MCP tool is present
// (docs/MCP.md, "Resources"). Spelled out rather than derived from
// tools.WithMCP, so these tests still assert the order the model is offered
// rather than agreeing with whatever that function does today.
func mcpAccessToolNames() []string {
	return []string{"MCPListResources", "MCPReadResource", "MCPListPrompts", "MCPGetPrompt"}
}

func baseToolNames(model string) []string {
	defs := tools.DefinitionsFor(model)
	names := make([]string, len(defs))
	for i, d := range defs {
		names[i] = d.Function.Name
	}
	return names
}

// recordingMultiTurnServer answers a streamed request with a TaskList call
// on its first turn (keeping the loop going without touching anything
// filesystem-related) and a plain answer on every turn after, recording
// every request body along the way — so a test can inspect what every
// sub-turn of a run actually sent.
func recordingMultiTurnServer(t *testing.T, bodies *[][]byte, mu *sync.Mutex) *httptest.Server {
	var call int32Counter
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, body)
		mu.Unlock()

		idx := call.next()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_list", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}}},
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
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
}

// TestRunSendsMCPToolsOnEveryRequestOfTheRun pins docs/MCP.md's "Resolution
// happens once per run": every request of a two-sub-turn run — not just the
// first — carries the base array with the configured server's tools
// appended after it, and the session row's own tool_schema agrees with
// exactly what was sent.
func TestRunSendsMCPToolsOnEveryRequestOfTheRun(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	srv := recordingMultiTurnServer(t, &bodies, &mu)
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.MCP = &fakeMCPProvider{defs: []wire.Tool{mcpToolDef("mcp__blender__get_objects_summary")}}

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "use the plan tools",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
	}

	want := append(append(baseToolNames("test-model"), mcpAccessToolNames()...), "mcp__blender__get_objects_summary")

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(bodies))
	}
	for i, body := range bodies {
		got := toolNames(t, body)
		if len(got) != len(want) {
			t.Fatalf("request %d sends %d tools, want %d", i+1, len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("request %d tool %d = %q, want %q", i+1, j, got[j], want[j])
			}
		}
	}

	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sess.ToolSchema) == 0 {
		t.Fatal("session row's tool_schema is empty")
	}
	var stored []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(sess.ToolSchema, &stored); err != nil {
		t.Fatalf("decode stored tool_schema: %v", err)
	}
	if len(stored) != len(want) || stored[len(stored)-1].Function.Name != "mcp__blender__get_objects_summary" {
		t.Fatalf("stored tool_schema does not carry the MCP tool at the end: %+v", stored)
	}
}

// TestRunProceedsWithNoMCPWhenDefinitionsErrors pins docs/MCP.md-adjacent
// resilience the spec calls out explicitly: a failure reading MCP
// definitions must not fail the run. The run proceeds with the base array
// alone, exactly as if no MCP provider were configured at all.
func TestRunProceedsWithNoMCPWhenDefinitionsErrors(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.MCP = &fakeMCPProvider{err: errors.New("mcp_servers table: disk I/O error")}

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok despite the MCP read failure, got %s", res.Status)
	}

	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	want := baseToolNames("test-model")
	var stored []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(sess.ToolSchema, &stored); err != nil {
		t.Fatalf("decode stored tool_schema: %v", err)
	}
	if len(stored) != len(want) {
		t.Fatalf("stored tool_schema has %d tools, want exactly the base array's %d (no MCP tools)", len(stored), len(want))
	}
}

// TestResumeSendsStoredToolArrayNotAFreshResolution pins the other half of
// "Resolution happens once per run": Resume reads the array back from the
// session's own stored tool_schema rather than calling MCPProvider.Definitions
// again, so a server enabled after the run started cannot change what a
// resumed session sends.
func TestResumeSendsStoredToolArrayNotAFreshResolution(t *testing.T) {
	var bodies []string
	srv := recordingAnswerServer(t, "answer", &bodies)
	defer srv.Close()

	fake := &fakeMCPProvider{defs: []wire.Tool{mcpToolDef("mcp__blender__get_objects_summary")}}
	r := newTestRunner(t, srv.URL)
	r.MCP = fake

	ws := t.TempDir()
	first, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "first task",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", first.Status)
	}

	// A server enabled since the run started — the fresh resolution this
	// test proves Resume must not pick up.
	fake.set([]wire.Tool{mcpToolDef("mcp__blender__get_objects_summary"), mcpToolDef("mcp__chrome__navigate")}, nil)

	resumed, err := r.Resume(t.Context(), ResumeOptions{
		SessionID: first.SessionID, Prompt: "keep going", MaxTokens: 4000,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", resumed.Status)
	}

	want := append(append(baseToolNames("test-model"), mcpAccessToolNames()...), "mcp__blender__get_objects_summary")
	if len(bodies) != 2 {
		t.Fatalf("expected 2 requests (run then resume), got %d", len(bodies))
	}
	for i, body := range bodies {
		got := toolNames(t, []byte(body))
		if len(got) != len(want) {
			t.Fatalf("request %d sends %d tools, want %d (the array frozen at run start, not the newly-enabled chrome server)", i+1, len(got), len(want))
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("request %d tool %d = %q, want %q", i+1, j, got[j], want[j])
			}
		}
	}
}

// TestTaskSubagentGetsMCPToolsToo pins that a Task subagent — which starts
// its own fresh session through Runner.Run (subagentRunner) rather than
// inheriting the parent's array — gets the same MCP resolution any other
// new run gets: it is its own run, so it resolves its own array from the
// same shared Runner.MCP, and its stored tool_schema carries the
// configured server's tools exactly like the parent's does.
func TestTaskSubagentGetsMCPToolsToo(t *testing.T) {
	srv := plainAnswerServer(t, "subagent answer")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)
	r.MCP = &fakeMCPProvider{defs: []wire.Tool{mcpToolDef("mcp__blender__get_objects_summary")}}

	ws := t.TempDir()
	run := r.subagentRunner("parent-sess", RunOptions{PermissionMode: tools.ModeFull}, ws)
	_, childID, err := run(t.Context(), "look something up", "find it", "general")
	if err != nil {
		t.Fatalf("subagent run: %v", err)
	}

	child, err := r.Store.GetSession(t.Context(), childID)
	if err != nil {
		t.Fatalf("expected the child session to exist: %v", err)
	}
	var stored []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(child.ToolSchema, &stored); err != nil {
		t.Fatalf("decode child tool_schema: %v", err)
	}
	if len(stored) == 0 || stored[len(stored)-1].Function.Name != "mcp__blender__get_objects_summary" {
		t.Fatalf("expected the subagent's own tool_schema to carry the MCP tool, got: %+v", stored)
	}
}

// TestCompactionKeepsTheSameMCPArray pins compact.go's own claim (its
// comment: "ToolSchema and ResultSchema are deliberately left as the copy
// gives them: the compacted session keeps the same tool array"): a run
// forced to compact mid-run carries an MCP-configured array into the
// compacted child unchanged, because compact forks curSess (whose
// ToolSchema already carries the array runLoop resolved once at the top of
// the run) rather than re-resolving anything.
func TestCompactionKeepsTheSameMCPArray(t *testing.T) {
	var streamCall int32Counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)
		if !probe.Stream {
			resp := wire.ChatCompletionResponse{
				Choices: []wire.Choice{{Message: wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("summary")}, FinishReason: wire.FinishStop}},
				Usage:   &wire.Usage{PromptTokens: 50, CompletionTokens: 10},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		idx := streamCall.next()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_00_a", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 900, PromptCacheMissTokens: 900, CompletionTokens: 5},
			})
		} else {
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
	r.MCP = &fakeMCPProvider{defs: []wire.Tool{mcpToolDef("mcp__blender__get_objects_summary")}}

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "a task that will need compaction",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
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
	if string(child.ToolSchema) != string(parent.ToolSchema) {
		t.Fatalf("compacted child's tool_schema differs from the parent's:\nparent: %s\nchild:  %s", parent.ToolSchema, child.ToolSchema)
	}
	if !strings.Contains(string(child.ToolSchema), "mcp__blender__get_objects_summary") {
		t.Fatalf("compacted child's tool_schema lost the MCP tool: %s", child.ToolSchema)
	}
}

// TestResumeReResolvesTheReadOnlyMapFreshEachTime pins the one piece of MCP
// state Resume does NOT freeze at run start: the per-server read-only
// allowance is policy, checked at call time, not prefix bytes — so a
// server whose allow_readonly an operator revokes between Run and Resume
// denies a readonly-mode call on the resumed session even though the same
// server's tool stayed in the frozen array the whole time.
func TestResumeReResolvesTheReadOnlyMapFreshEachTime(t *testing.T) {
	var call int32Counter
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := call.next()
		w.Header().Set("Content-Type", "text/event-stream")
		switch idx {
		case 0:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("started")}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishStop)}},
				Usage:   &wire.Usage{PromptTokens: 100, CompletionTokens: 5},
			})
		case 1:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_mcp", Type: "function", Function: wire.ToolCallFuncDelta{Name: "mcp__blender__get_objects_summary", Arguments: `{}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 200, CompletionTokens: 5},
			})
		default:
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{Role: "assistant", Content: strPtr("resumed done")}}},
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

	fake := &fakeMCPProvider{
		defs:     []wire.Tool{mcpToolDef("mcp__blender__get_objects_summary")},
		readOnly: map[string]bool{"blender": true},
	}
	r := newTestRunner(t, srv.URL)
	r.MCP = fake

	ws := t.TempDir()
	first, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeReadOnly, Prompt: "start",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", first.Status)
	}

	// The operator revokes allow_readonly on the blender server before the
	// resume — the live policy fact Resume must pick up fresh.
	fake.set([]wire.Tool{mcpToolDef("mcp__blender__get_objects_summary")}, map[string]bool{"blender": false})

	resumed, err := r.Resume(t.Context(), ResumeOptions{
		SessionID: first.SessionID, Prompt: "continue", MaxTokens: 4000,
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", resumed.Status)
	}

	events, err := r.Store.GetEvents(t.Context(), resumed.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	denied := false
	for _, e := range events {
		if e.Kind != store.KindToolDenied {
			continue
		}
		var p store.ToolDeniedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Name == "mcp__blender__get_objects_summary" {
			denied = true
			if !strings.Contains(p.Rule, "blender") {
				t.Errorf("denial rule should name the server, got: %s", p.Rule)
			}
		}
	}
	if !denied {
		t.Fatal("expected the MCP call to be denied after the server's read-only allowance was revoked between run and resume")
	}
}

// TestRunOpeningMessageNamesConfiguredMCPServers and its absence
// counterpart pin docs/MCP.md's "What the model is told": the opening
// message carries a short MCP section naming the connected servers only
// when the session's resolved array carries MCP tools, and is otherwise
// byte-identical to a run with none configured.
func TestRunOpeningMessageNamesConfiguredMCPServers(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.MCP = &fakeMCPProvider{defs: []wire.Tool{
		mcpToolDef("mcp__blender__get_objects_summary"),
		mcpToolDef("mcp__blender__render_thumbnail_to_path"),
		mcpToolDef("mcp__chrome__navigate"),
	}}

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var started store.SessionStartedPayload
	for _, e := range events {
		if e.Kind == store.KindSessionStarted {
			if err := json.Unmarshal(e.Payload, &started); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, want := range []string{"blender", "chrome", "mcp__<server>__<tool>"} {
		if !strings.Contains(started.OpeningMessage, want) {
			t.Errorf("opening message should mention %q, got: %s", want, started.OpeningMessage)
		}
	}
	if !strings.Contains(started.OpeningMessage, "blender (2 tools)") {
		t.Errorf("opening message should count blender's tools, got: %s", started.OpeningMessage)
	}
	if !strings.Contains(started.OpeningMessage, "chrome (1 tool)") {
		t.Errorf("opening message should count chrome's tool singular, got: %s", started.OpeningMessage)
	}
}

func TestRunOpeningMessageOmitsMCPSectionWhenNoneConfigured(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	withoutMCP := newTestRunner(t, srv.URL) // r.MCP left nil

	ws := t.TempDir()
	res, err := withoutMCP.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	events, err := withoutMCP.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var started store.SessionStartedPayload
	for _, e := range events {
		if e.Kind == store.KindSessionStarted {
			if err := json.Unmarshal(e.Payload, &started); err != nil {
				t.Fatal(err)
			}
		}
	}
	// "MCP" alone is not a safe needle here: t.TempDir() embeds the test's
	// own name in the workspace path, and this test's name contains "MCP".
	// The block's own opening phrase is what must be absent.
	if strings.Contains(started.OpeningMessage, "MCP servers:") {
		t.Errorf("opening message should carry no MCP section with no servers configured, got: %s", started.OpeningMessage)
	}
	want := RenderOpeningMessage(mustEvalSymlinks(t, ws), "say something", nil, "", "", "", nil)
	if started.OpeningMessage != want {
		t.Errorf("opening message with no MCP servers must be byte-identical to a plain run:\ngot:  %q\nwant: %q", started.OpeningMessage, want)
	}
}

// TestResumeEmptyStoredToolSchemaResolvesFresh pins the fallback for a
// stored tool_schema that decodes to nothing. "null" and "[]" both
// unmarshal without error, and taking either at its word would resume the
// session with no tools at all — the model would simply stop calling
// anything, with nothing in the log naming the cause.
func TestResumeEmptyStoredToolSchemaResolvesFresh(t *testing.T) {
	for _, raw := range []string{"null", "[]", "", "not json"} {
		if _, err := unmarshalToolSchema([]byte(raw)); err == nil {
			t.Errorf("unmarshalToolSchema(%q) = nil error, want a failure so the caller resolves fresh", raw)
		}
	}
	got, err := unmarshalToolSchema([]byte(`[{"type":"function","function":{"name":"Read"}}]`))
	if err != nil {
		t.Fatalf("unmarshalToolSchema(one tool): %v", err)
	}
	if len(got) != 1 || got[0].Function.Name != "Read" {
		t.Fatalf("unmarshalToolSchema(one tool) = %+v, want the single Read tool", got)
	}
}

// openingMessageOf reads back the opening message Run recorded on the
// session-started event, the only place the rendered MCP section can be
// observed from outside.
func openingMessageOf(t *testing.T, r *Runner, sessionID string) string {
	t.Helper()
	events, err := r.Store.GetEvents(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var started store.SessionStartedPayload
	for _, e := range events {
		if e.Kind == store.KindSessionStarted {
			if err := json.Unmarshal(e.Payload, &started); err != nil {
				t.Fatal(err)
			}
		}
	}
	return started.OpeningMessage
}

// TestRunOpeningMessageCarriesServerInstructions is the end of the plumbing
// this whole column exists for: what a server said about itself at
// initialize reaches the model, verbatim, in the run's first message.
func TestRunOpeningMessageCarriesServerInstructions(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	const text = "NEVER assume missing values - inspect the scene first."
	r := newTestRunner(t, srv.URL)
	r.MCP = &fakeMCPProvider{
		defs:         []wire.Tool{mcpToolDef("mcp__blender__get_objects_summary")},
		instructions: map[string]string{"blender": text},
	}

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	opening := openingMessageOf(t, r, res.SessionID)
	if !strings.Contains(opening, text) {
		t.Errorf("opening message should carry blender's instructions verbatim, got: %s", opening)
	}
	if !strings.Contains(opening, "What the blender server says about using it:") {
		t.Errorf("opening message should attribute the instructions to their server, got: %s", opening)
	}
}

// TestRunOpeningMessageSurvivesAnInstructionsReadFailure pins that losing
// the prose costs a log line, not the run: the tools are still on the array
// and the session still starts.
func TestRunOpeningMessageSurvivesAnInstructionsReadFailure(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	r := newTestRunner(t, srv.URL)
	r.MCP = &fakeMCPProvider{
		defs:            []wire.Tool{mcpToolDef("mcp__blender__get_objects_summary")},
		instructionsErr: errors.New("mcp_servers: disk on fire"),
	}

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	opening := openingMessageOf(t, r, res.SessionID)
	if !strings.Contains(opening, "blender (1 tool)") {
		t.Errorf("opening message should still name the server, got: %s", opening)
	}
	if strings.Contains(opening, "says about using it") {
		t.Errorf("opening message should carry no instructions heading, got: %s", opening)
	}
}

// TestRenderMCPBlockInstructions covers the rendering rules directly, which
// is cheaper than driving a run per case: attribution and sorted order for
// servers that sent instructions, silence for a server that sent none, and
// — the one that matters — nothing at all for a server whose instructions
// are stored but whose tools are not on this session's array.
func TestRenderMCPBlockInstructions(t *testing.T) {
	array := []wire.Tool{
		mcpToolDef("mcp__zebra__stripe"),
		mcpToolDef("mcp__alpha__first"),
	}
	got := RenderMCPBlock(array, map[string]string{
		"zebra":   "zebra says hello",
		"alpha":   "alpha says hello",
		"missing": "nobody should read this",
	})

	alpha := strings.Index(got, "What the alpha server says about using it:")
	zebra := strings.Index(got, "What the zebra server says about using it:")
	if alpha < 0 || zebra < 0 {
		t.Fatalf("both servers should be attributed, got: %s", got)
	}
	if alpha > zebra {
		t.Errorf("instruction sections should follow the same sorted order as the listing, got: %s", got)
	}
	if strings.Contains(got, "nobody should read this") {
		t.Errorf("a server contributing no tools should contribute no instructions, got: %s", got)
	}

	// A server with tools and no instructions renders exactly what it
	// rendered before any of this existed.
	plain := RenderMCPBlock(array, nil)
	if strings.Contains(plain, "says about using it") {
		t.Errorf("no instructions should render no heading, got: %s", plain)
	}
	if plain != RenderMCPBlock(array, map[string]string{"alpha": "   "}) {
		t.Error("whitespace-only instructions should render identically to none")
	}
}
