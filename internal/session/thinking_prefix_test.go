package session

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/anthropic"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// mintBlock is one content block a scripted Anthropic response carries, in
// the form the API holds it: the JSON object it would accept back.
type mintBlock map[string]any

func thinkingBlock(sig, text string) mintBlock {
	return mintBlock{"type": "thinking", "thinking": text, "signature": sig}
}

func textBlock(text string) mintBlock { return mintBlock{"type": "text", "text": text} }

func toolUseBlock(id, name string, input map[string]any) mintBlock {
	return mintBlock{"type": "tool_use", "id": id, "name": name, "input": input}
}

func serverToolUseBlock(id string) mintBlock {
	return mintBlock{"type": "server_tool_use", "id": id, "name": "code_execution", "input": map[string]any{"code": "search('x')"}}
}

func serverResultBlock(id string) mintBlock {
	return mintBlock{"type": "code_execution_tool_result", "tool_use_id": id,
		"content": map[string]any{"type": "code_execution_result", "stdout": "ok", "stderr": "", "return_code": float64(0), "content": []any{}}}
}

// mintStream renders blocks as the SSE frames the Messages API streams them
// in: thinking and text from empty starts plus deltas, tool calls from an
// empty input plus one input_json_delta, a result block whole at its start.
func mintStream(stop string, blocks ...mintBlock) string {
	var b strings.Builder
	b.WriteString(claudeFrame("message_start", `{"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":1}}}`))
	for i, blk := range blocks {
		start := func(v any) {
			raw, _ := json.Marshal(map[string]any{"type": "content_block_start", "index": i, "content_block": v})
			b.WriteString(claudeFrame("content_block_start", string(raw)))
		}
		delta := func(v any) {
			raw, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": i, "delta": v})
			b.WriteString(claudeFrame("content_block_delta", string(raw)))
		}
		switch blk["type"] {
		case "thinking":
			start(map[string]any{"type": "thinking", "thinking": "", "signature": ""})
			delta(map[string]any{"type": "thinking_delta", "thinking": blk["thinking"]})
			delta(map[string]any{"type": "signature_delta", "signature": blk["signature"]})
		case "text":
			start(map[string]any{"type": "text", "text": ""})
			delta(map[string]any{"type": "text_delta", "text": blk["text"]})
		case "tool_use", "server_tool_use":
			start(map[string]any{"type": blk["type"], "id": blk["id"], "name": blk["name"], "input": map[string]any{}})
			in, _ := json.Marshal(blk["input"])
			delta(map[string]any{"type": "input_json_delta", "partial_json": string(in)})
		default:
			start(blk)
		}
		b.WriteString(claudeFrame("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, i)))
	}
	b.WriteString(claudeFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"`+stop+`","stop_sequence":null},"usage":{"output_tokens":20}}`))
	b.WriteString(claudeFrame("message_stop", `{"type":"message_stop"}`))
	return b.String()
}

type mintResponse struct {
	stop   string
	blocks []mintBlock
}

// mintedPrefix is what a thinking block's signature records: the system
// prompt, the tool set, every message before the block, and the blocks
// before it in its own assistant turn — as the request that produced it
// carried them, with a pause_turn continuation's trailing assistant message
// folded into the turn it continues.
type mintedPrefix struct {
	request   int
	system    any
	tools     []string
	messages  []any
	turnStart []any
}

// mintingServer serves scripted responses in order and records, for every
// thinking block it hands out, the prefix that block was minted on.
type mintingServer struct {
	t         *testing.T
	mu        sync.Mutex
	responses []mintResponse
	bodies    []map[string]any
	minted    map[string]mintedPrefix
	onRequest func(n int, r *http.Request)
}

func (s *mintingServer) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			s.t.Errorf("decode request: %v", err)
		}
		body = stripCacheControl(body).(map[string]any)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		n := len(s.bodies)
		s.mu.Unlock()
		if s.onRequest != nil {
			s.onRequest(n, r)
		}
		if n > len(s.responses) {
			s.t.Errorf("request %d has no scripted response", n)
			http.Error(w, "unscripted", http.StatusInternalServerError)
			return
		}
		resp := s.responses[n-1]

		msgs, _ := body["messages"].([]any)
		var turnStart []any
		if k := len(msgs); k > 0 {
			if last, _ := msgs[k-1].(map[string]any); last["role"] == wire.RoleAssistant {
				turnStart, _ = last["content"].([]any)
				msgs = msgs[:k-1]
			}
		}
		s.mu.Lock()
		for j, blk := range resp.blocks {
			if blk["type"] != "thinking" {
				continue
			}
			before := append([]any{}, turnStart...)
			for _, prev := range resp.blocks[:j] {
				before = append(before, roundTrip(prev))
			}
			s.minted[blk["signature"].(string)] = mintedPrefix{
				request: n, system: body["system"], tools: toolSet(body["tools"]),
				messages: msgs, turnStart: before,
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, mintStream(resp.stop, resp.blocks...))
	}))
}

// check holds every thinking block a request replays against the prefix it
// was minted on, the comparison preserved thinking's history-editing check
// makes, and reports the first one that differs the way the API does.
func (s *mintingServer) check(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	replayed := 0
	for n, body := range s.bodies {
		msgs, _ := body["messages"].([]any)
		for i, m := range msgs {
			msg, _ := m.(map[string]any)
			content, _ := msg["content"].([]any)
			for k, c := range content {
				blk, _ := c.(map[string]any)
				if blk["type"] != "thinking" {
					continue
				}
				sig, _ := blk["signature"].(string)
				mint, ok := s.minted[sig]
				if !ok {
					t.Errorf("request %d messages.%d.content.%d replays a thinking block no response minted", n+1, i, k)
					continue
				}
				replayed++
				where := fmt.Sprintf("request %d messages.%d.content.%d (minted on request %d)", n+1, i, k, mint.request)
				switch {
				case !reflect.DeepEqual(body["system"], mint.system):
					t.Errorf("%s: system prompt differs from the one the block was minted on", where)
				case !reflect.DeepEqual(toolSet(body["tools"]), mint.tools):
					t.Errorf("%s: tool set differs from the one the block was minted on", where)
				case !reflect.DeepEqual(append([]any{}, msgs[:i]...), append([]any{}, mint.messages...)):
					t.Errorf("%s: messages before the block differ from the ones it was minted on, starting at messages.%d\nreplayed: %s\nminted:   %s",
						where, firstDiff(msgs[:i], mint.messages), compactJSON(msgs[:i]), compactJSON(mint.messages))
				case !reflect.DeepEqual(append([]any{}, content[:k]...), mint.turnStart):
					t.Errorf("%s: the blocks ahead of it in its own turn differ from the ones it was minted after\nreplayed: %s\nminted:   %s",
						where, compactJSON(content[:k]), compactJSON(mint.turnStart))
				}
			}
		}
	}
	if replayed == 0 {
		t.Fatal("no request replayed a thinking block, so nothing was checked")
	}
}

func firstDiff(a, b []any) int {
	for i := range a {
		if i >= len(b) || !reflect.DeepEqual(a[i], b[i]) {
			return i
		}
	}
	return len(a)
}

func compactJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func roundTrip(v any) any {
	raw, _ := json.Marshal(v)
	var out any
	json.Unmarshal(raw, &out)
	return out
}

// stripCacheControl removes every cache_control marker, which the check does
// not compare: requestFromIntent moves one to the last message each request.
func stripCacheControl(v any) any {
	switch x := v.(type) {
	case map[string]any:
		delete(x, "cache_control")
		for k, e := range x {
			x[k] = stripCacheControl(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = stripCacheControl(e)
		}
		return x
	}
	return v
}

// toolSet is the tool set as the check compares it: a name-keyed set,
// order ignored.
func toolSet(v any) []string {
	list, _ := v.([]any)
	var out []string
	for _, t := range list {
		raw, _ := json.Marshal(t)
		out = append(out, string(raw))
	}
	sort.Strings(out)
	return out
}

// TestReplayedThinkingKeepsItsMintedPrefix drives the shape of the Android
// session that stopped taking turns — parallel tool calls with thinking
// between them, web searches that pause_turn and that leave a server tool
// open beside a client call, a steer behind that open call, and later turns
// run by a fresh Runner on the reopened state directory the way a respawned
// harness resumes one — and holds every thinking block each request replays
// against the prefix it was minted on.
func TestReplayedThinkingKeepsItsMintedPrefix(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "harness.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	os.WriteFile(filepath.Join(workspace, "a.md"), []byte("alpha\n"), 0o644)
	os.WriteFile(filepath.Join(workspace, "b.md"), []byte("beta\n"), 0o644)
	read := func(name string) map[string]any { return map[string]any{"file_path": filepath.Join(workspace, name)} }

	const sessionID = "sess-thinking-prefix"
	const steer = "also look at the privacy policy"
	srv := &mintingServer{t: t, minted: map[string]mintedPrefix{}, responses: []mintResponse{
		// Turn 1.
		{"tool_use", []mintBlock{thinkingBlock("sig-1", "plan"), textBlock("Reading both."),
			toolUseBlock("toolu_1", "Read", read("a.md")), thinkingBlock("sig-2", "and the other"),
			toolUseBlock("toolu_2", "Read", read("b.md"))}},
		{"pause_turn", []mintBlock{thinkingBlock("sig-3", "search"), serverToolUseBlock("srvtoolu_1"),
			serverResultBlock("srvtoolu_1"), thinkingBlock("sig-4", "search more"), textBlock("Searching."),
			serverToolUseBlock("srvtoolu_2")}},
		{"tool_use", []mintBlock{serverResultBlock("srvtoolu_2"), thinkingBlock("sig-5", "now run it"),
			toolUseBlock("toolu_3", "Bash", map[string]any{"command": "echo one", "description": "echo"})}},
		{"tool_use", []mintBlock{thinkingBlock("sig-6", "search and glob"), serverToolUseBlock("srvtoolu_3"),
			toolUseBlock("toolu_4", "Glob", map[string]any{"pattern": "*.md"})}},
		{"end_turn", []mintBlock{serverResultBlock("srvtoolu_3"), thinkingBlock("sig-7", "wrap up"), textBlock("Done.")}},
		{"end_turn", []mintBlock{thinkingBlock("sig-8", "the steer"), textBlock("The policy needs a line.")}},
		// Turn 2, a respawned process.
		{"end_turn", []mintBlock{thinkingBlock("sig-9", "list them"), textBlock("Five questions.")}},
		// Turn 3, another respawn: a search left open beside two client calls.
		{"tool_use", []mintBlock{thinkingBlock("sig-10", "look it up"), serverToolUseBlock("srvtoolu_4"),
			toolUseBlock("toolu_5", "Grep", map[string]any{"pattern": "alpha"}),
			toolUseBlock("toolu_6", "Bash", map[string]any{"command": "echo two", "description": "echo"})}},
		{"end_turn", []mintBlock{serverResultBlock("srvtoolu_4"), thinkingBlock("sig-11", "answer"), textBlock("Written.")}},
		// Turn 4.
		{"end_turn", []mintBlock{thinkingBlock("sig-12", "again"), textBlock("Again.")}},
	}}
	srv.onRequest = func(n int, r *http.Request) {
		if n != 4 {
			return
		}
		if _, err := st.AppendEvents(r.Context(), sessionID, []store.EventInput{
			{Kind: store.KindSteerMessage, Payload: store.SteerMessagePayload{Text: steer, Source: "web"}},
		}); err != nil {
			t.Errorf("append steer: %v", err)
		}
	}
	hs := srv.serve()
	defer hs.Close()

	run, err := newClaudeRunner(t, st, dir, hs.URL).Run(t.Context(), RunOptions{
		Model: anthropic.ModelSonnet55, Effort: wire.EffortHigh, MaxTokens: 4000,
		Workspace: workspace, PermissionMode: tools.ModeFull, Prompt: "plan the analytics work",
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != store.StatusOK {
		t.Fatalf("turn 1 = %s %q", run.Status, run.Text)
	}

	// Each later turn runs in a "new process": a fresh store handle and
	// Runner on the same state directory.
	for turn, prompt := range []string{"what are the questions", "1. ask 2. worker 3. 90 days", "try again"} {
		st.Close()
		st, err = store.Open(dbPath)
		if err != nil {
			t.Fatal(err)
		}
		res, err := newClaudeRunner(t, st, dir, hs.URL).Resume(t.Context(), ResumeOptions{SessionID: sessionID, Prompt: prompt, MaxTokens: 4000})
		if err != nil {
			t.Fatalf("turn %d Resume: %v", turn+2, err)
		}
		if res.Status != store.StatusOK {
			t.Fatalf("turn %d = %s %q", turn+2, res.Status, res.Text)
		}
	}
	st.Close()

	if got := len(srv.bodies); got != len(srv.responses) {
		t.Fatalf("requests = %d, want %d", got, len(srv.responses))
	}
	srv.check(t)
}

// TestMismatchedThinkingNoLongerWedgesTheSession is the Android session's
// failure at the loop: once the API refuses a request because a replayed
// thinking block is bound to a different conversation, the log replays that
// block on every later request, so the turn fails and so does every "try
// again" after it. The API here refuses any request that replays a thinking
// block under "error", the way it refused that session's from sub-turn 24 on.
func TestMismatchedThinkingNoLongerWedgesTheSession(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	responses := []string{
		mintStream("tool_use", thinkingBlock("sig-1", "look"),
			toolUseBlock("toolu_1", "Bash", map[string]any{"command": "echo one", "description": "echo"})),
		mintStream("end_turn", thinkingBlock("sig-2", "answer"), textBlock("Done.")),
		mintStream("end_turn", thinkingBlock("sig-3", "again"), textBlock("Again.")),
	}
	var mu sync.Mutex
	var behaviours []string
	served := 0
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req anthropic.MessagesRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		behaviour := req.Thinking.BlockBinding.PrefixMismatchBehavior
		mu.Lock()
		defer mu.Unlock()
		behaviours = append(behaviours, behaviour)
		if behaviour == "error" && strings.Contains(string(raw), `"type":"thinking"`) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0: Invalid `+"`signature`"+` in `+"`thinking`"+` block. The block is bound to a different conversation. Remove the block, or set `+"`thinking.block_binding.prefix_mismatch_behavior`"+` to \"drop_block\"."}}`)
			return
		}
		if served >= len(responses) {
			t.Errorf("request %d has no scripted response", len(behaviours))
			http.Error(w, "unscripted", http.StatusInternalServerError)
			return
		}
		served++
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, responses[served-1])
	}))
	defer hs.Close()

	r := newClaudeRunner(t, st, dir, hs.URL)
	run, err := r.Run(t.Context(), RunOptions{
		Model: anthropic.ModelSonnet55, Effort: wire.EffortHigh, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "plan it",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if run.Status != store.StatusOK || run.Text != "Done." {
		t.Fatalf("turn 1 = %s %q, want it to finish despite the refused replay", run.Status, run.Text)
	}
	again, err := r.Resume(t.Context(), ResumeOptions{SessionID: run.SessionID, Prompt: "try again", MaxTokens: 4000})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if again.Status != store.StatusOK || again.Text != "Again." {
		t.Fatalf("turn 2 = %s %q", again.Status, again.Text)
	}
	want := []string{"error", "error", "drop_block", "drop_block"}
	if !reflect.DeepEqual(behaviours, want) {
		t.Fatalf("behaviours sent = %v, want %v: one refusal, then drop_block for the rest of the session", behaviours, want)
	}
}
