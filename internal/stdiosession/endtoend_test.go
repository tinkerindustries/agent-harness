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

	"github.com/mrgeoffrich/agent-harness/internal/deepseek"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/mcpclient"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// This file is the one test that watches both ends of a session at once.
//
// Every other test in this package wires a scripted Gemini client behind the
// server, which proves the parent-facing half and says nothing about what
// leaves the process. The claim for this entry point spans both — the same
// vocabulary at the parent's frame and at the provider's request
// (docs/STDIO-PROTOCOL.md, docs/DEEPSEEK-RESPONSES.md) — and the only way to
// hold it is to record the outbound HTTP while the inbound frames are being
// read.
//
// The two ends are rendered separately from the session's event log; nothing
// is forwarded. That is exactly why this test is needed: the frames could be
// right while the request was the other dialect, and every other test here
// would still pass.
//
// So: a fake DeepSeek at one end, a real pipe at the other, and assertions
// on both.

// deepseekRecorder is a stand-in for api.deepseek.com that records every
// request it is sent and answers from a script of semantic event streams.
type deepseekRecorder struct {
	mu       sync.Mutex
	paths    []string
	bodies   []string
	streams  []string
	nextResp int
}

func (d *deepseekRecorder) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		d.mu.Lock()
		d.paths = append(d.paths, r.URL.Path)
		d.bodies = append(d.bodies, string(body))
		i := d.nextResp
		if i >= len(d.streams) {
			i = len(d.streams) - 1
		}
		d.nextResp++
		stream := d.streams[i]
		d.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(stream))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (d *deepseekRecorder) seen() ([]string, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.paths...), append([]string(nil), d.bodies...)
}

// sse frames one semantic event the way the provider's stream carries it.
func sse(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString("data: " + f + "\n\n")
	}
	return b.String()
}

// dsToolCall is a provider stream that reasons, then calls one tool.
func dsToolCall(callID, name, args string) string {
	argsJSON, _ := json.Marshal(args)
	return sse(
		`{"type":"response.created","response":{"status":"in_progress"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"I should use a tool."}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","call_id":"`+callID+`","name":"`+name+`"}}`,
		`{"type":"response.function_call_arguments.delta","output_index":1,"delta":`+string(argsJSON)+`}`,
		`{"type":"response.completed","response":{"status":"completed","output":[{"type":"function_call","call_id":"`+callID+`"}],`+
			`"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":64},"output_tokens":20,"output_tokens_details":{"reasoning_tokens":5},"total_tokens":120}}}`,
	)
}

// dsAnswer is a provider stream that answers in text and calls nothing,
// which ends the run.
func dsAnswer(text string) string {
	textJSON, _ := json.Marshal(text)
	return sse(
		`{"type":"response.created","response":{"status":"in_progress"}}`,
		`{"type":"response.reasoning_text.delta","output_index":0,"delta":"Nearly there."}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant"}}`,
		`{"type":"response.output_text.delta","output_index":1,"delta":`+string(textJSON)+`}`,
		`{"type":"response.completed","response":{"status":"completed",`+
			`"usage":{"input_tokens":200,"input_tokens_details":{"cached_tokens":128},"output_tokens":30,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":230}}}`,
	)
}

// newDeepSeekFixture is newFixture with the provider replaced: a real
// deepseek.ResponsesClient pointed at a recorder, rather than the scripted
// Gemini client the rest of this package uses.
func newDeepSeekFixture(t *testing.T, streams ...string) (*fixture, *deepseekRecorder) {
	t.Helper()
	dir, cwd := t.TempDir(), t.TempDir()

	rec := &deepseekRecorder{streams: streams}
	api := rec.serve(t)

	st, err := store.Open(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	client := deepseek.NewResponsesClient(api.URL, "sk-test")
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
		Models:       []string{testDeepSeekModel},
		DefaultModel: testDeepSeekModel,
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

// TestResponsesAllTheWayThrough is the claim this entry point makes, held to
// on both ends of one session: the parent speaks the Responses vocabulary
// over the pipe, and the request that leaves the process is a Responses
// request. Neither half is asserted from the other's shape — the outbound
// HTTP is recorded as it is sent.
func TestResponsesAllTheWayThrough(t *testing.T) {
	f, rec := newDeepSeekFixture(t,
		dsToolCall("call-1", "TodoWrite", `{"todos":[]}`),
		dsAnswer("All done."),
	)
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("write a todo list")
	p.Model = testDeepSeekModel
	p.Reasoning = &ReasoningConfig{Effort: "low"}
	p.MaxOutputTokens = 4096

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, p, &created); rerr != nil {
		t.Fatalf("responses.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)

	// --- the provider end ---------------------------------------------

	paths, bodies := rec.seen()
	if len(paths) < 2 {
		t.Fatalf("the provider saw %d requests, want the two sub-turns: %v", len(paths), paths)
	}
	for i, path := range paths {
		if path != "/responses" {
			t.Errorf("request %d went to %q, want /responses", i, path)
		}
	}

	// The first request is the Responses shape and nothing of the other
	// dialect. Both halves matter: a body carrying `input` *and* `messages`
	// would still be wrong.
	var first map[string]any
	if err := json.Unmarshal([]byte(bodies[0]), &first); err != nil {
		t.Fatalf("decode the first provider request: %v", err)
	}
	for _, want := range []string{"model", "instructions", "input", "reasoning", "max_output_tokens", "tools"} {
		if _, ok := first[want]; !ok {
			t.Errorf("the provider request has no %q: %s", want, bodies[0])
		}
	}
	for _, unwanted := range []string{"messages", "max_tokens", "reasoning_effort", "thinking", "stream_options"} {
		if _, ok := first[unwanted]; ok {
			t.Errorf("the provider request carries the Chat Completions field %q: %s", unwanted, bodies[0])
		}
	}
	if eff, _ := first["reasoning"].(map[string]any); eff == nil || eff["effort"] != "low" {
		t.Errorf("reasoning.effort did not reach the provider: %v", first["reasoning"])
	}
	if first["max_output_tokens"] != float64(4096) {
		t.Errorf("max_output_tokens = %v, want the create body's 4096", first["max_output_tokens"])
	}
	// The tool array is the flattened Responses shape, not the nested one.
	tools, _ := first["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("the provider request carries no tools")
	}
	tool0, _ := tools[0].(map[string]any)
	if tool0["name"] == nil || tool0["function"] != nil {
		t.Errorf("tool 0 is not the flattened Responses shape: %v", tool0)
	}

	// The second request replays the first sub-turn as input items. This is
	// the fold's output crossing the seam, which is the part no single-shot
	// probe can reach: reasoning as its own item, the call, and its output
	// paired by call_id.
	var second map[string]any
	if err := json.Unmarshal([]byte(bodies[1]), &second); err != nil {
		t.Fatalf("decode the second provider request: %v", err)
	}
	items, _ := second["input"].([]any)
	var kinds []string
	var callID, outputID string
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		kind, _ := item["type"].(string)
		kinds = append(kinds, kind)
		switch kind {
		case "function_call":
			callID, _ = item["call_id"].(string)
		case "function_call_output":
			outputID, _ = item["call_id"].(string)
		}
	}
	for _, want := range []string{"message", "reasoning", "function_call", "function_call_output"} {
		if !containsString(kinds, want) {
			t.Errorf("the replayed input has no %q item: %v", want, kinds)
		}
	}
	if callID == "" || callID != outputID {
		t.Errorf("the call and its output are not paired: %q vs %q", callID, outputID)
	}
	// The reasoning item is the one DeepSeek 400s without, on any request
	// carrying tools (third_party/deepseek-docs/guides/thinking_mode.md).
	if indexOf(kinds, "reasoning") > indexOf(kinds, "function_call") {
		t.Errorf("the reasoning item comes after the call it explains: %v", kinds)
	}

	// --- the parent end -----------------------------------------------

	methods := map[string]bool{}
	for _, m := range seen {
		methods[m.Method] = true
	}
	for _, want := range []string{
		NotifyResponseCreated, NotifyResponseInProgress,
		NotifyOutputItemAdded, NotifyOutputItemDone,
		NotifyReasoningTextDelta, NotifyOutputTextDelta,
		NotifyFunctionCallArgsDelta, NotifyUsage, NotifyResponseCompleted,
	} {
		if !methods[want] {
			t.Errorf("the parent never saw %s", want)
		}
	}
	// Nothing of the protocol this one replaced.
	for gone := range methods {
		if strings.HasPrefix(gone, "interaction.") || strings.HasPrefix(gone, "step.") {
			t.Errorf("an Interactions-era frame reached the parent: %s", gone)
		}
	}

	if got := stepTypes(seen); !containsString(got, ItemFunctionCall) || !containsString(got, ItemFunctionCallOutput) {
		t.Errorf("the parent did not see the call and its output: %v", got)
	}
	if txt := textOf(seen, ItemMessage); txt != "All done." {
		t.Errorf("assistant text = %q, want %q", txt, "All done.")
	}
	if s := summaryOf(seen); !strings.Contains(s, "I should use a tool.") {
		t.Errorf("reasoning text did not reach the parent: %q", s)
	}
	assertStepLifecycle(t, seen)

	// The terminal frame is the run's own, not one sub-turn's.
	var done responseEnvelope
	for _, m := range seen {
		if m.Method == NotifyResponseCompleted {
			json.Unmarshal(m.Params, &done)
		}
	}
	if done.Response.Status != StatusCompleted {
		t.Errorf("final status = %q", done.Response.Status)
	}
	if done.Response.Usage == nil || done.Response.Usage.TotalTokens != 350 {
		t.Errorf("usage is not the run's total across both sub-turns: %+v", done.Response.Usage)
	}
	if done.Response.Usage.InputTokensDetails.CachedTokens != 192 {
		t.Errorf("cached tokens = %d, want both sub-turns' 192",
			done.Response.Usage.InputTokensDetails.CachedTokens)
	}
}

// TestNoChatCompletionsRequestEverLeaves is the negative of the above, kept
// separate because it is the claim most likely to rot: a future change that
// wires this entry point back onto the other dialect would still pass every
// frame assertion above if the provider client were swapped.
func TestNoChatCompletionsRequestEverLeaves(t *testing.T) {
	f, rec := newDeepSeekFixture(t, dsAnswer("hi"))
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("say hi")
	p.Model = testDeepSeekModel
	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, p, &created); rerr != nil {
		t.Fatalf("responses.create: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)

	paths, bodies := rec.seen()
	for i, path := range paths {
		if strings.Contains(path, "chat/completions") {
			t.Errorf("request %d went to %q", i, path)
		}
	}
	for i, body := range bodies {
		if strings.Contains(body, `"messages"`) {
			t.Errorf("request %d carried a messages array: %s", i, body)
		}
	}
}

func containsString(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

func indexOf(all []string, want string) int {
	for i, s := range all {
		if s == want {
			return i
		}
	}
	return len(all) + 1
}
