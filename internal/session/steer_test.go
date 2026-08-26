package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/deepseek"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// runSteerScenario drives a two-sub-turn run — sub-turn 1 makes a TaskList
// call so the loop continues, sub-turn 2 answers plainly and ends the run —
// against a fake API that appends the given steer_message events while the
// first request is still in flight, the exact shape a mid-run steer takes
// (docs/RUN-CONTROL.md "How the loop picks one up"). It returns the messages
// arrays of the two requests the fake API received, in order, for the prefix
// and ordering assertions, and the run's workspace, which a steer carrying
// images writes into.
//
// The steers are built from the store rather than passed in ready-made, so a
// test can write an attachment row first and name its id on the message —
// the id is minted by the store, and the store belongs to this harness.
func runSteerScenario(t *testing.T, sessionID string, steers func(*store.Store) []store.SteerMessagePayload) ([][]wire.Message, string) {
	t.Helper()
	var mu sync.Mutex
	var requests [][]wire.Message

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	pending := steers(st)
	workspace := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req wire.ChatCompletionRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		n := len(requests) + 1
		requests = append(requests, req.Messages)
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			// The steer lands while this request — the start of the run's
			// first tool round — is still in flight.
			inputs := make([]store.EventInput, 0, len(pending))
			for _, s := range pending {
				inputs = append(inputs, store.EventInput{Kind: store.KindSteerMessage, Payload: s})
			}
			if _, err := st.AppendEvents(r.Context(), sessionID, inputs); err != nil {
				t.Errorf("append steer: %v", err)
			}
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{
					Role:      "assistant",
					ToolCalls: []wire.ToolCallDelta{{Index: 0, ID: "call_steer_0", Type: "function", Function: wire.ToolCallFuncDelta{Name: "TaskList", Arguments: `{}`}}},
				}}},
			})
			writeSSEChunk(t, w, wire.ChatCompletionChunk{
				Choices: []wire.ChunkChoice{{Delta: wire.ChunkDelta{}, FinishReason: strPtr(wire.FinishToolCalls)}},
				Usage:   &wire.Usage{PromptTokens: 200, PromptCacheHitTokens: 100, PromptCacheMissTokens: 100, CompletionTokens: 5},
			})
		} else {
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
	defer srv.Close()

	r := &Runner{
		Store:      st,
		Mirror:     store.NewMirror(filepath.Join(dir, "mirror")),
		Client:     deepseek.NewClient(srv.URL, "test-key"),
		Prices:     testPrices(),
		FlashModel: "test-model",
	}
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: workspace, PermissionMode: tools.ModeFull, Prompt: "do the task",
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
	}

	mu.Lock()
	defer mu.Unlock()
	return requests, workspace
}

// requireMessageJSON marshals m the way the wire serialises it, so two
// messages are "byte-identical" exactly when they would serialise the same.
func requireMessageJSON(t *testing.T, m wire.Message) string {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal message: %v", err)
	}
	return string(raw)
}

// TestSteerAppearsAsNextUserMessage is the steering property end to end: a
// steer sent while the first tool round is in flight appears as the next
// request's final user message, verbatim, and every message the previous
// request already carried is byte-identical in the new one — the steer
// appends at the tail and rewrites nothing before it (docs/CACHE.md's
// append-only property, asserted the way prefix_test.go compares bytes). The
// tool round in between is undisturbed: the assistant(tool_calls) and tool
// messages sit intact ahead of the steer.
func TestSteerAppearsAsNextUserMessage(t *testing.T) {
	requests, _ := runSteerScenario(t, "sess-steer-1", func(*store.Store) []store.SteerMessagePayload {
		return []store.SteerMessagePayload{{Text: "be terse now", Source: "web"}}
	})
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}

	// The steer is the new request's final message, verbatim, as a user role.
	last := requests[1][len(requests[1])-1]
	if last.Role != wire.RoleUser || last.Content.String() != "be terse now" {
		t.Fatalf("final message = %+v, want user \"be terse now\" verbatim", last)
	}

	// The prefix survived: every message of the previous request appears
	// byte-identical at the same index of the new one.
	if len(requests[1]) <= len(requests[0]) {
		t.Fatalf("new request (%d messages) must extend the previous one (%d)", len(requests[1]), len(requests[0]))
	}
	for i, prev := range requests[0] {
		if got, want := requireMessageJSON(t, requests[1][i]), requireMessageJSON(t, prev); got != want {
			t.Fatalf("message %d changed across the steer:\n before: %s\n  after: %s", i, want, got)
		}
	}

	// The tool round the steer arrived during is intact: assistant with tool
	// calls, then the tool result, then the steer at the tail.
	mid := requests[1][len(requests[1])-2]
	if mid.Role != wire.RoleTool || mid.ToolCallID != "call_steer_0" {
		t.Fatalf("message before the steer = %+v, want the tool result of the round", mid)
	}
	assistant := requests[1][len(requests[1])-3]
	if assistant.Role != wire.RoleAssistant || len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].ID != "call_steer_0" {
		t.Fatalf("message before the tool result = %+v, want the assistant's tool call", assistant)
	}
}

// TestTwoSteersArriveInOrder pins the ordering property: two steers sent
// back to back fold to two user messages in the order they were sent, not in
// any completion order.
func TestTwoSteersArriveInOrder(t *testing.T) {
	requests, _ := runSteerScenario(t, "sess-steer-2", func(*store.Store) []store.SteerMessagePayload {
		return []store.SteerMessagePayload{
			{Text: "first instruction", Source: "cli"},
			{Text: "second instruction", Source: "mcp"},
		}
	})
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}

	tail := requests[1][len(requests[1])-2:]
	for i, want := range []string{"first instruction", "second instruction"} {
		m := tail[i]
		if m.Role != wire.RoleUser || m.Content.String() != want {
			t.Fatalf("tail message %d = %+v, want user %q in send order", i, m, want)
		}
	}
}

// recordingPlainAnswerServer answers every stream request with a plain
// answer that ends the run, recording each request's messages array. It is
// the fake API the resume steer test runs the resumed session against.
func recordingPlainAnswerServer(t *testing.T, answer string) (*httptest.Server, func() [][]wire.Message) {
	t.Helper()
	var mu sync.Mutex
	var requests [][]wire.Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req wire.ChatCompletionRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		mu.Lock()
		requests = append(requests, req.Messages)
		mu.Unlock()

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
	return srv, func() [][]wire.Message {
		mu.Lock()
		defer mu.Unlock()
		return requests
	}
}

// TestSteerHighWaterSurvivesResume is the high-water mark's reason to exist:
// a finished session whose log carries one applied steer and one unapplied
// steer is resumed, and the resumed run delivers only the unapplied one,
// exactly once. The mark is derived from the log on resume (LastAppliedSteerSeq)
// rather than carried in memory, so "applied" is whatever the log says and a
// steer can never be delivered twice (docs/RUN-CONTROL.md "How the loop
// picks one up").
func TestSteerHighWaterSurvivesResume(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	// First run: a plain answer ends it in one sub-turn.
	firstSrv := plainAnswerServer(t, "first answer")
	defer firstSrv.Close()
	r1 := &Runner{
		Store: st, Mirror: store.NewMirror(filepath.Join(dir, "mirror")),
		Client: deepseek.NewClient(firstSrv.URL, "test-key"), Prices: testPrices(), FlashModel: "test-model",
	}
	ws := t.TempDir()
	first, err := r1.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "start a task",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", first.Status)
	}

	// After the run: one steer that was applied (message + its steer_applied)
	// and one that never was. Appended to a terminal session, which the store
	// accepts — only a cancelled session refuses appends.
	ctx := t.Context()
	appended, err := st.AppendEvents(ctx, first.SessionID, []store.EventInput{
		{Kind: store.KindSteerMessage, Payload: store.SteerMessagePayload{Text: "applied before resume", Source: "cli"}},
	})
	if err != nil {
		t.Fatalf("append applied steer_message: %v", err)
	}
	if _, err := st.AppendEvents(ctx, first.SessionID, []store.EventInput{
		{Kind: store.KindSteerApplied, Payload: store.SteerAppliedPayload{SourceSeq: appended[0].Seq, Text: "applied before resume", SubTurn: 1}},
		{Kind: store.KindSteerMessage, Payload: store.SteerMessagePayload{Text: "only this one is new", Source: "mcp"}},
	}); err != nil {
		t.Fatalf("append applied + unapplied steers: %v", err)
	}

	// Resume against a recording server, over the same store.
	recSrv, requests := recordingPlainAnswerServer(t, "resumed answer")
	defer recSrv.Close()
	r2 := &Runner{
		Store: st, Mirror: store.NewMirror(filepath.Join(dir, "mirror2")),
		Client: deepseek.NewClient(recSrv.URL, "test-key"), Prices: testPrices(), FlashModel: "test-model",
	}
	resumed, err := r2.Resume(ctx, ResumeOptions{SessionID: first.SessionID, MaxTokens: 4000})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK {
		t.Fatalf("expected resumed status ok, got %s", resumed.Status)
	}

	got := requests()
	if len(got) != 1 {
		t.Fatalf("expected the resumed run to make 1 request, got %d", len(got))
	}
	messages := got[0]

	// Only the unapplied steer is new: it is the final user message, appears
	// exactly once, and the applied-before-resume steer is not re-applied
	// (it appears exactly once, from the steer_applied the log already had).
	if n := len(messages); n < 2 {
		t.Fatalf("resumed request has %d messages, want the fold tail to end with the unapplied steer", n)
	}
	last := messages[len(messages)-1]
	if last.Role != wire.RoleUser || last.Content.String() != "only this one is new" {
		t.Fatalf("final message = %+v, want the unapplied steer verbatim", last)
	}
	for _, tc := range []struct {
		text string
		want int
	}{
		{"applied before resume", 1}, // already applied: delivered once, never re-applied
		{"only this one is new", 1},  // the unapplied one: delivered once
	} {
		got := 0
		for _, m := range messages {
			if m.Role == wire.RoleUser && m.Content.String() == tc.text {
				got++
			}
		}
		if got != tc.want {
			t.Fatalf("user message %q appears %d times, want %d", tc.text, got, tc.want)
		}
	}
}

// TestSteerAttachmentsReachTheWorkspaceAndTheMessage is the image half of
// steering (docs/RUN-CONTROL.md, "Images in the composer"): a steer naming
// attachment rows has its files written into the running session's workspace
// and the message the model reads names the paths they landed under.
//
// The ordering is the property worth pinning. The bytes are stored when the
// HTTP handler accepts the message, but nothing writes them to disk until
// the loop applies it — so the assertion is not just that the file exists at
// the end, but that the model's message and the file agree on where it is.
func TestSteerAttachmentsReachTheWorkspaceAndTheMessage(t *testing.T) {
	png := []byte("\x89PNG fake bytes")
	requests, workspace := runSteerScenario(t, "sess-steer-att", func(st *store.Store) []store.SteerMessagePayload {
		id, err := st.WriteAttachment(t.Context(), "mockup.png", "image/png", png)
		if err != nil {
			t.Fatalf("write attachment: %v", err)
		}
		return []store.SteerMessagePayload{{Text: "match this mockup", Source: "web", AttachmentIDs: []string{id}}}
	})

	// The file is in the workspace, under the plain name it was stored with.
	path := filepath.Join(workspace, "scratch", "attachments", "mockup.png")
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read materialised attachment: %v", err)
	}
	if string(got) != string(png) {
		t.Errorf("materialised bytes = %q, want %q", got, png)
	}

	// And the message the model reads names that path, ahead of the words.
	if len(requests) != 2 {
		t.Fatalf("expected 2 requests, got %d", len(requests))
	}
	last := requests[1][len(requests[1])-1]
	if last.Role != wire.RoleUser {
		t.Fatalf("last message role = %q, want user", last.Role)
	}
	content := last.Content.String()
	if !strings.Contains(content, "scratch/attachments/mockup.png") {
		t.Errorf("steer message does not name the attachment path:\n%s", content)
	}
	if !strings.Contains(content, "match this mockup") {
		t.Errorf("steer message lost the operator's own words:\n%s", content)
	}
	if strings.Index(content, "scratch/attachments/mockup.png") > strings.Index(content, "match this mockup") {
		t.Errorf("the attachment block should come before the words:\n%s", content)
	}
}
