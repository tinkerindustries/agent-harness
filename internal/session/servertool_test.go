package session

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/anthropic"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// The Anthropic client is the one provider whose responses can leave a
// server tool open across a client tool round; the loop only finds that out
// through the optional seam, so a client that stops satisfying it would
// silently lose the deferral.
var _ ServerToolHolder = (*anthropic.Client)(nil)

// openServerToolID is the server tool call the scripted responses below leave
// open, the id shape the API mints for one.
const openServerToolID = "srvtoolu_017316q55G8RgWFKLYMthRtU"

func claudeFrame(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

// claudeOpenServerTool is a response that calls a dynamic-filtering web
// search (a server_tool_use named code_execution) in parallel with a client
// tool, the shape that ends stop_reason "tool_use" with the server call left
// open (docs/ANTHROPIC-INTEGRATION.md, "Server tools left open across a tool
// round").
func claudeOpenServerTool(toolUseID, name, argsJSON string) string {
	args, _ := json.Marshal(argsJSON)
	var b strings.Builder
	b.WriteString(claudeFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1}}}`))
	b.WriteString(claudeFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"`+openServerToolID+`","name":"code_execution","input":{}}}`))
	b.WriteString(claudeFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"code\":\"search('go 1.26')\"}"}}`))
	b.WriteString(claudeFrame("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(claudeFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"`+toolUseID+`","name":"`+name+`","input":{}}}`))
	b.WriteString(claudeFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":`+string(args)+`}}`))
	b.WriteString(claudeFrame("content_block_stop", `{"type":"content_block_stop","index":1}`))
	b.WriteString(claudeFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":20}}`))
	b.WriteString(claudeFrame("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

// claudeServerToolClosed is the response to the request after
// claudeOpenServerTool's tool round: it begins with the open call's result,
// then answers in text and calls nothing.
func claudeServerToolClosed(text string) string {
	textJSON, _ := json.Marshal(text)
	var b strings.Builder
	b.WriteString(claudeFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":150,"output_tokens":1}}}`))
	b.WriteString(claudeFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"code_execution_tool_result","tool_use_id":"`+openServerToolID+`","content":{"type":"code_execution_result","stdout":"ok","stderr":"","return_code":0,"content":[]}}}`))
	b.WriteString(claudeFrame("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(claudeFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`))
	b.WriteString(claudeFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":`+string(textJSON)+`}}`))
	b.WriteString(claudeFrame("content_block_stop", `{"type":"content_block_stop","index":1}`))
	b.WriteString(claudeFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":30}}`))
	b.WriteString(claudeFrame("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

func claudePlainAnswer(text string) string {
	textJSON, _ := json.Marshal(text)
	var b strings.Builder
	b.WriteString(claudeFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":200,"output_tokens":1}}}`))
	b.WriteString(claudeFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	b.WriteString(claudeFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(textJSON)+`}}`))
	b.WriteString(claudeFrame("content_block_stop", `{"type":"content_block_stop","index":0}`))
	b.WriteString(claudeFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":30}}`))
	b.WriteString(claudeFrame("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

// claudeScript serves the given streams in order, one per request, and
// records each request's messages array. onRequest, when set, runs before
// request n (1-based) is answered — the moment a steer sent mid-request
// lands.
type claudeScript struct {
	mu        sync.Mutex
	streams   []string
	messages  [][]json.RawMessage
	onRequest func(n int, r *http.Request)
}

func (s *claudeScript) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []json.RawMessage `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		s.mu.Lock()
		s.messages = append(s.messages, req.Messages)
		n := len(s.messages)
		s.mu.Unlock()
		if s.onRequest != nil {
			s.onRequest(n, r)
		}
		if n > len(s.streams) {
			t.Errorf("request %d has no scripted response", n)
			http.Error(w, "unscripted", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, s.streams[n-1])
	}))
}

func (s *claudeScript) requests() [][]json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]json.RawMessage(nil), s.messages...)
}

// withoutCacheControl strips the one breakpoint requestFromIntent moves to
// the last message on every request, so two requests' shared messages
// compare on what the cache prefix is actually made of.
func withoutCacheControl(m json.RawMessage) string {
	return strings.ReplaceAll(string(m), `,"cache_control":{"type":"ephemeral"}`, "")
}

// requireAppendOnly asserts every message of prev reappears, unchanged and
// in place, at the head of next — the property preserved thinking's prefix
// check and the prompt cache both rest on.
func requireAppendOnly(t *testing.T, prev, next []json.RawMessage) {
	t.Helper()
	if len(next) < len(prev) {
		t.Fatalf("next request has %d messages, fewer than the previous %d", len(next), len(prev))
	}
	for i := range prev {
		if withoutCacheControl(prev[i]) != withoutCacheControl(next[i]) {
			t.Fatalf("message %d changed between requests:\nprev %s\nnext %s", i, prev[i], next[i])
		}
	}
}

// requireToolResultsOnly asserts m is a user message whose content is
// tool_result blocks and nothing else — the only thing the API accepts
// after a turn that left a server tool open.
func requireToolResultsOnly(t *testing.T, m json.RawMessage) {
	t.Helper()
	var msg struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal(m, &msg); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if msg.Role != wire.RoleUser || len(msg.Content) == 0 {
		t.Fatalf("message = %s, want a user message of tool results", m)
	}
	for _, b := range msg.Content {
		if b.Type != "tool_result" {
			t.Fatalf("message = %s, want tool_result blocks only", m)
		}
	}
}

func newClaudeRunner(t *testing.T, st *store.Store, dir, url string) *Runner {
	return &Runner{
		Store:      st,
		Mirror:     store.NewMirror(filepath.Join(dir, "mirror")),
		Client:     anthropic.NewClient(url, anthropic.WithAPIKeyProvider(func() (string, error) { return "test-key", nil })),
		Prices:     testPrices(),
		FlashModel: anthropic.ModelSonnet55,
	}
}

// TestSteerWaitsBehindOpenServerTool is the wedge the Android session hit,
// at the loop: a steer arrives while a response that left a server tool
// open is in flight. Applying it at the next boundary put a user message
// after the tool results, and the API refused that request — and every
// request after it, since the log replays — with "`code_execution` tool
// use with id `srvtoolu_…` was found without a corresponding
// `code_execution_tool_result` block". The steer must wait for the boundary
// after the response that closes the call, and must still be delivered even
// though that response calls no tools and would otherwise end the run.
func TestSteerWaitsBehindOpenServerTool(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	const sessionID = "sess-open-server-tool"
	const steer = "also check the changelog"
	script := &claudeScript{streams: []string{
		claudeOpenServerTool("toolu_list", "TaskList", `{}`),
		claudeServerToolClosed("searched and listed"),
		claudePlainAnswer("the changelog says so"),
	}}
	script.onRequest = func(n int, r *http.Request) {
		if n != 1 {
			return
		}
		if _, err := st.AppendEvents(r.Context(), sessionID, []store.EventInput{
			{Kind: store.KindSteerMessage, Payload: store.SteerMessagePayload{Text: steer, Source: "web"}},
		}); err != nil {
			t.Errorf("append steer: %v", err)
		}
	}
	srv := script.serve(t)
	defer srv.Close()

	r := newClaudeRunner(t, st, dir, srv.URL)
	res, err := r.Run(t.Context(), RunOptions{
		Model: anthropic.ModelSonnet55, Effort: wire.EffortHigh, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "look it up",
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK || res.Text != "the changelog says so" {
		t.Fatalf("run = %s %q, want ok ending on the answer to the steer", res.Status, res.Text)
	}

	reqs := script.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3: open the call, close it, answer the steer", len(reqs))
	}
	second := reqs[1]
	requireToolResultsOnly(t, second[len(second)-1])
	for i, m := range second {
		if strings.Contains(string(m), steer) {
			t.Fatalf("request 2 message %d carries the steer behind an open server tool: %s", i, m)
		}
	}
	third := reqs[2]
	requireAppendOnly(t, second, third)
	if !strings.Contains(string(third[len(third)-1]), steer) {
		t.Fatalf("request 3 ends %s, want the steer", third[len(third)-1])
	}

	events, err := st.GetEvents(t.Context(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	var applied []store.SteerAppliedPayload
	for _, e := range events {
		if e.Kind == store.KindSteerApplied {
			var p store.SteerAppliedPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			applied = append(applied, p)
		}
	}
	if len(applied) != 1 || applied[0].Text != steer || applied[0].SubTurn != 3 {
		t.Fatalf("steer_applied = %+v, want the steer applied once, at sub-turn 3", applied)
	}
}

// TestResumeBehindOpenServerToolAnswersTheNewMessage is the second way into
// the wedge: a run that ended right after a response that left a server tool
// open (here, the model called Complete beside it), then a new message. The
// client holds the message back from the first request so the open call can
// finish, and the loop must not end the run on that request's plain answer
// — the model has not seen the message yet.
func TestResumeBehindOpenServerToolAnswersTheNewMessage(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	const followUp = "now summarise what you found"
	script := &claudeScript{streams: []string{
		claudeOpenServerTool("toolu_complete", "Complete", `{"summary":"done","status":"done"}`),
		claudeServerToolClosed("the search finished"),
		claudePlainAnswer("here is the summary"),
	}}
	srv := script.serve(t)
	defer srv.Close()

	r := newClaudeRunner(t, st, dir, srv.URL)
	first, err := r.Run(t.Context(), RunOptions{
		Model: anthropic.ModelSonnet55, Effort: wire.EffortHigh, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "look it up",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if first.Status != store.StatusOK {
		t.Fatalf("first run status = %s", first.Status)
	}

	resumed, err := r.Resume(t.Context(), ResumeOptions{SessionID: first.SessionID, Prompt: followUp, MaxTokens: 4000})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.Status != store.StatusOK || resumed.Text != "here is the summary" {
		t.Fatalf("resumed run = %s %q, want ok ending on the answer to the new message", resumed.Status, resumed.Text)
	}

	reqs := script.requests()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3", len(reqs))
	}
	second := reqs[1]
	requireToolResultsOnly(t, second[len(second)-1])
	for i, m := range second {
		if strings.Contains(string(m), followUp) {
			t.Fatalf("request 2 message %d carries the new message behind an open server tool: %s", i, m)
		}
	}
	third := reqs[2]
	requireAppendOnly(t, second, third)
	if !strings.Contains(string(third[len(third)-1]), followUp) {
		t.Fatalf("request 3 ends %s, want the new message", third[len(third)-1])
	}
}
