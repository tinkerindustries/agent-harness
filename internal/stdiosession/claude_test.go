package stdiosession

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/anthropic"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/mcpclient"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// This file drives a Claude model over the Responses pipe against a fake
// Anthropic Messages API — the same two-ended shape endtoend_test.go uses
// for DeepSeek: a real internal/anthropic.Client at one end, a real pipe at
// the other, with the outbound HTTP recorded while the inbound frames are
// read. Nothing here reaches api.anthropic.com.

// anthropicRecorder is a stand-in for api.anthropic.com: it records every
// request's headers and body and answers from a script of Messages API SSE
// streams.
type anthropicRecorder struct {
	mu       sync.Mutex
	bodies   []string
	headers  []http.Header
	streams  []string
	nextResp int
}

func (a *anthropicRecorder) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		a.mu.Lock()
		a.bodies = append(a.bodies, string(body))
		a.headers = append(a.headers, r.Header.Clone())
		i := a.nextResp
		if i >= len(a.streams) {
			i = len(a.streams) - 1
		}
		a.nextResp++
		stream := a.streams[i]
		a.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(stream))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (a *anthropicRecorder) seen() ([]string, []http.Header) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.bodies...), append([]http.Header(nil), a.headers...)
}

// claudeSSEFrame is one named Messages API SSE event.
func claudeSSEFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// claudeToolCall is a Messages API stream that calls one tool and stops for
// it, ending the sub-turn.
func claudeToolCall(toolUseID, name, argsJSON string) string {
	var b strings.Builder
	b.WriteString(claudeSSEFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1}}}`))
	b.WriteString(claudeSSEFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`))
	b.WriteString(claudeSSEFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"I should use a tool."}}`))
	b.WriteString(claudeSSEFrame("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(claudeSSEFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"`+toolUseID+`","name":"`+name+`","input":{}}}`))
	b.WriteString(claudeSSEFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+quoteJSON(argsJSON)+`}}`))
	b.WriteString(claudeSSEFrame("content_block_stop", `{"type":"content_block_stop","index":1}`))
	b.WriteString(claudeSSEFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`))
	b.WriteString(claudeSSEFrame("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

// claudeAnswer is a Messages API stream that answers in text and calls
// nothing, which ends the run.
func claudeAnswer(text string) string {
	textJSON, _ := json.Marshal(text)
	var b strings.Builder
	b.WriteString(claudeSSEFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":200,"output_tokens":1}}}`))
	b.WriteString(claudeSSEFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	b.WriteString(claudeSSEFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(textJSON)+`}}`))
	b.WriteString(claudeSSEFrame("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(claudeSSEFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":30}}`))
	b.WriteString(claudeSSEFrame("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

// quoteJSON re-encodes an already-valid JSON string as a Go string literal
// for embedding inside another JSON document's string field
// (input_json_delta's partial_json is a string, not a nested object).
func quoteJSON(raw string) string {
	b, _ := json.Marshal(raw)
	return string(b)
}

// newClaudeFixture is newFixture with the provider replaced: a real
// anthropic.Client pointed at a recorder, hosting only testClaudeModel, the
// way newDeepSeekFixture hosts only testDeepSeekModel.
func newClaudeFixture(t *testing.T, streams ...string) (*fixture, *anthropicRecorder) {
	t.Helper()
	dir, cwd := t.TempDir(), t.TempDir()

	rec := &anthropicRecorder{streams: streams}
	api := rec.serve(t)

	st, err := store.Open(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	client := anthropic.NewClient(api.URL, anthropic.WithAPIKeyProvider(func() (string, error) { return "ak-test", nil }))
	eventHub := hub.New()
	runner := &session.Runner{
		Store:     st,
		Client:    client,
		ClientFor: func(string) session.Client { return client },
		Hub:       eventHub,
	}
	mgr := mcpclient.New(st)
	t.Cleanup(func() { mgr.Close() })

	srv := NewServer(Options{
		Store: st, Runner: runner, Hub: eventHub, MCP: mgr,
		Models:       []string{testClaudeModel},
		DefaultModel: testClaudeModel,
		HasAPIKey:    func(string) bool { return true },
		Version:      "test",
	})

	clientIn, serverIn := io.Pipe()
	serverOut, clientOut := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.Serve(ctx, clientIn, clientOut)
	}()
	t.Cleanup(func() {
		cancel()
		serverIn.Close()
		<-done
	})

	f := &fixture{
		t: t, script: nil, cwd: cwd, stateDir: dir, srv: srv,
		cancel: cancel, done: done, stdin: serverIn,
		client: newClient(t, serverIn, serverOut),
	}
	return f, rec
}

// testClaudeModel is the model this file drives — the one the proof in
// docs/plans/reports/claude-provider names, so a live transcript excerpt
// and this fake one exercise the same model.
const testClaudeModel = "claude-sonnet-5-5"

// TestClaudeSessionOverResponsesDialect proves a Claude model drives
// correctly under stdio-session: the parent-facing frames are the same
// Responses vocabulary every other hosted model speaks (the dialect is
// provider-neutral), a tool call round-trips through the harness's own
// dispatcher, the outbound HTTP is a genuine Messages API request (headers,
// system/messages/tools shape), and its tool array carries Anthropic's own
// web_search server tool but not the harness's WebFetch
// (internal/tools.definitionsClaude).
func TestClaudeSessionOverResponsesDialect(t *testing.T) {
	f, rec := newClaudeFixture(t,
		claudeToolCall("toolu_1", "TaskCreate", `{"tasks":[{"subject":"x","description":"y","activeForm":"z"}]}`),
		claudeAnswer("All done."),
	)
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("make a plan")
	p.Model = testClaudeModel

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, p, &created); rerr != nil {
		t.Fatalf("responses.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)

	// --- the provider end ---------------------------------------------

	bodies, headers := rec.seen()
	if len(bodies) < 2 {
		t.Fatalf("the provider saw %d requests, want the two sub-turns: %v", len(bodies), bodies)
	}
	for i, h := range headers {
		if h.Get("x-api-key") == "" {
			t.Errorf("request %d carries no x-api-key", i)
		}
		if h.Get("anthropic-version") == "" {
			t.Errorf("request %d carries no anthropic-version", i)
		}
	}

	var first map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &first); err != nil {
		t.Fatalf("decode the first provider request: %v", err)
	}
	for _, want := range []string{"model", "system", "messages", "tools"} {
		if _, ok := first[want]; !ok {
			t.Errorf("the provider request has no %q: %s", want, bodies[0])
		}
	}
	for _, unwanted := range []string{"input", "instructions", "max_output_tokens", "reasoning", "reasoning_effort"} {
		if _, ok := first[unwanted]; ok {
			t.Errorf("the provider request carries the Responses/Chat-Completions field %q: %s", unwanted, bodies[0])
		}
	}

	tools, _ := first["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("the provider request carries no tools")
	}
	var sawWebSearch, sawWebFetchTool, sawReadTool bool
	for _, raw := range tools {
		tl, _ := raw.(map[string]any)
		switch tl["name"] {
		case "web_search":
			sawWebSearch = true
			if tl["type"] != "web_search_20260209" {
				t.Errorf("web_search tool has type %v, want web_search_20260209", tl["type"])
			}
		case "WebFetch":
			sawWebFetchTool = true
		case "Read":
			sawReadTool = true
			if tl["input_schema"] == nil {
				t.Error("the Read tool has no input_schema")
			}
		}
	}
	if !sawWebSearch {
		t.Error("the provider request carries no Anthropic web_search server tool")
	}
	if sawWebFetchTool {
		t.Error("the provider request carries the harness's own WebFetch tool; Claude sessions must drop it")
	}
	if !sawReadTool {
		t.Error("the provider request carries no Read tool")
	}

	// --- the parent end -----------------------------------------------

	methods := map[string]bool{}
	for _, m := range seen {
		methods[m.Method] = true
	}
	for _, want := range []string{
		NotifyResponseCreated, NotifyResponseInProgress,
		NotifyOutputItemAdded, NotifyOutputItemDone,
		NotifyFunctionCallArgsDelta, NotifyUsage, NotifyResponseCompleted,
	} {
		if !methods[want] {
			t.Errorf("the parent never saw %s", want)
		}
	}
	if got := stepTypes(seen); !containsString(got, ItemFunctionCall) || !containsString(got, ItemFunctionCallOutput) {
		t.Errorf("the parent did not see the call and its output: %v", got)
	}
	if txt := textOf(seen, ItemMessage); txt != "All done." {
		t.Errorf("assistant text = %q, want %q", txt, "All done.")
	}

	var done responseEnvelope
	for _, m := range seen {
		if m.Method == NotifyResponseCompleted {
			json.Unmarshal(m.Params, &done)
		}
	}
	if done.Response.Status != StatusCompleted {
		t.Errorf("final status = %q", done.Response.Status)
	}
}
