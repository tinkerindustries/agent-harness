package responsesstdio

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/gemini/geminitest"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/mcpclient"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// The tests drive a real Server over an in-memory pipe, against a real
// session.Runner whose provider client is internal/gemini pointed at a
// scripted event-stream server. Nothing here reaches Google: the SSE bodies
// are built by geminitest, the same builder internal/gemini's own tests use.

const (
	testModel = "gemini-3.7-flash"
	// testAltModel is a second hosted model, for the checks that need two
	// names this process accepts — a resume may not change model, and that
	// is a different refusal from naming a model nobody hosts.
	testAltModel = "gemini-3.5-flash"
	// testDeepSeekModel is the one DeepSeek model cmd/harness hosts
	// (deepSeekSessionModel), advertised here so the create path is
	// exercised with a model from the other provider.
	testDeepSeekModel = "deepseek-v4-flash-vision-exp"
	// testUnhostedModel is routable by internal/provider and deliberately
	// not hosted: it cannot see images, so a session on it would carry
	// vision tools that need Google's credentials (docs/DEEPSEEK-VISION.md).
	testUnhostedModel = "deepseek-v4-flash"
)

// scriptedGemini answers each request with the next stream in its script,
// repeating the last one once the script runs out.
type scriptedGemini struct {
	mu       sync.Mutex
	streams  []string
	requests []json.RawMessage
	n        int
}

func (s *scriptedGemini) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.requests = append(s.requests, json.RawMessage(body))
		i := s.n
		if i >= len(s.streams) {
			i = len(s.streams) - 1
		}
		s.n++
		out := s.streams[i]
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte(out))
	}))
}

func (s *scriptedGemini) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

// client is the test's half of the pipe: it writes frames to the server and
// reads what comes back, splitting answers from notifications.
type client struct {
	t    *testing.T
	w    io.WriteCloser
	enc  *json.Encoder
	mu   sync.Mutex
	next int

	notifications chan message
	replies       map[string]chan message
	// onRequest answers a server-initiated request. Nil declines everything
	// with a method-not-found, which is the behaviour the protocol requires
	// of a client that does not implement a method.
	onRequest func(m message) (any, *rpcError)
}

func newClient(t *testing.T, in io.WriteCloser, out io.Reader) *client {
	c := &client{
		t: t, w: in, enc: json.NewEncoder(in),
		notifications: make(chan message, 4096),
		replies:       map[string]chan message{},
	}
	go c.read(out)
	return c
}

func (c *client) read(out io.Reader) {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		var m message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) > 0:
			go c.answer(m)
		case m.Method != "":
			c.notifications <- m
		default:
			var id string
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			c.mu.Lock()
			ch := c.replies[id]
			c.mu.Unlock()
			if ch != nil {
				ch <- m
			}
		}
	}
	close(c.notifications)
}

func (c *client) answer(m message) {
	c.mu.Lock()
	h := c.onRequest
	c.mu.Unlock()
	if h == nil {
		c.send(message{ID: m.ID, Error: errorf(CodeMethodNotFound, "no handler for %s", m.Method)})
		return
	}
	res, rerr := h(m)
	if rerr != nil {
		c.send(message{ID: m.ID, Error: rerr})
		return
	}
	raw, _ := json.Marshal(res)
	c.send(message{ID: m.ID, Result: raw})
}

func (c *client) send(m message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.enc.Encode(m); err != nil {
		c.t.Logf("client write: %v", err)
	}
}

// call sends a request and waits for its answer.
func (c *client) call(method string, params any, out any) *rpcError {
	c.t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		c.t.Fatalf("encode %s params: %v", method, err)
	}
	c.mu.Lock()
	c.next++
	id := "c" + itoa(c.next)
	ch := make(chan message, 1)
	c.replies[id] = ch
	c.mu.Unlock()

	idRaw, _ := json.Marshal(id)
	c.send(message{ID: idRaw, Method: method, Params: raw})

	select {
	case m := <-ch:
		if m.Error != nil {
			return m.Error
		}
		if out != nil && len(m.Result) > 0 {
			if err := json.Unmarshal(m.Result, out); err != nil {
				c.t.Fatalf("decode %s result: %v", method, err)
			}
		}
		return nil
	case <-time.After(30 * time.Second):
		c.t.Fatalf("%s: no answer in 30s", method)
		return nil
	}
}

func (c *client) notify(method string, params any) {
	raw, _ := json.Marshal(params)
	c.send(message{Method: method, Params: raw})
}

// handshake performs initialize + initialized.
func (c *client) handshake(caps ClientCapabilities) InitializeResult {
	c.t.Helper()
	var res InitializeResult
	if rerr := c.call(MethodInitialize, InitializeParams{
		ClientInfo:   ClientInfo{Name: "test", Version: "0"},
		Capabilities: caps,
	}, &res); rerr != nil {
		c.t.Fatalf("initialize: %v", rerr)
	}
	c.notify(MethodInitialized, map[string]any{})
	return res
}

// waitFor collects notifications until one with the given method arrives,
// returning everything seen including it.
func (c *client) waitFor(method string) []message {
	c.t.Helper()
	var seen []message
	deadline := time.After(30 * time.Second)
	for {
		select {
		case m, ok := <-c.notifications:
			if !ok {
				c.t.Fatalf("stream ended before %s; saw %s", method, methodsOf(seen))
			}
			seen = append(seen, m)
			if m.Method == method {
				return seen
			}
		case <-deadline:
			c.t.Fatalf("no %s in 30s; saw %s", method, methodsOf(seen))
		}
	}
}

func methodsOf(ms []message) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Method
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// fixture is a running server, its client, and the scripted model behind it.
type fixture struct {
	t        *testing.T
	client   *client
	script   *scriptedGemini
	cwd      string
	stateDir string
	srv      *Server
	cancel   context.CancelFunc
	done     chan struct{}
	stdin    io.WriteCloser
}

func newFixture(t *testing.T, streams ...string) *fixture {
	t.Helper()
	return newFixtureIn(t, t.TempDir(), t.TempDir(), streams...)
}

// newFixtureIn is newFixture with the state directory and the working
// directory named, so a test can stand a second server up over the state a
// first one left behind. That is what a parent respawning
// `harness stdio-session` on the same -state-dir does, and it is the only
// way to exercise harness.resume_session_id.
func newFixtureIn(t *testing.T, dir, cwd string, streams ...string) *fixture {
	t.Helper()
	script := &scriptedGemini{streams: streams}
	api := script.serve()
	t.Cleanup(api.Close)

	st, err := store.Open(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	gc := gemini.NewClient(api.URL, gemini.WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	eventHub := hub.New()
	runner := &session.Runner{
		Store:     st,
		Client:    gc,
		ClientFor: func(string) session.Client { return gc },
		Hub:       eventHub,
		// A small ceiling keeps a misbehaving script from looping.
		MaxSubTurns: 8,
	}

	// A real manager, so the mcp_server path — registering a declaration,
	// probing it, and putting a credential back for the dial — runs the
	// production code. Nothing in these tests stands a server up, so every
	// probe fails, which is itself the tolerated case.
	mgr := mcpclient.New(st)
	t.Cleanup(func() { mgr.Close() })

	srv := NewServer(Options{
		Store: st, Runner: runner, Hub: eventHub, MCP: mgr,
		Models:       []string{testModel, testAltModel, testDeepSeekModel},
		DefaultModel: testModel,
		HasAPIKey:    func(string) bool { return true },
		Version:      "test",
	})

	clientIn, serverIn := io.Pipe()
	serverOut, clientOut := io.Pipe()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx, clientIn, clientOut)
		clientOut.Close()
	}()

	f := &fixture{
		t: t, client: newClient(t, serverIn, serverOut), script: script,
		cwd: cwd, stateDir: dir, srv: srv, cancel: cancel, done: done, stdin: serverIn,
	}
	t.Cleanup(f.close)
	return f
}

func (f *fixture) close() {
	f.stdin.Close()
	f.cancel()
	select {
	case <-f.done:
	case <-time.After(30 * time.Second):
		f.t.Error("server did not stop within 30s of stdin closing")
	}
}

// answer is the common script: one thought step, one model_output step, no
// tool calls — the shape of a sub-turn that ends the run because the model
// asked for nothing else.
func answer(text string) string {
	return geminitest.Stream([]geminitest.Step{
		{Type: "thought", Summaries: []string{"Thinking about it."}},
		{Type: "model_output", Texts: []string{text}},
	}, `{"total_tokens":40,"total_input_tokens":10,"total_output_tokens":20,"total_thought_tokens":10}`)
}

// callThen is a script that calls one function and says nothing else.
func callThen(id, name, args string) string {
	return geminitest.Stream([]geminitest.Step{
		{Type: "thought", Summaries: []string{"I should use a tool."}},
		{Type: "function_call", ID: id, Name: name, Arguments: []string{args}},
	}, `{"total_tokens":30,"total_input_tokens":10,"total_output_tokens":10,"total_thought_tokens":10}`)
}

// createParams is the common create body: this fixture's working directory,
// read-only, one text input.
func (f *fixture) createParams(text string) CreateParams {
	input, _ := json.Marshal(text)
	return CreateParams{
		Model: testModel,
		Input: input,
		Harness: &CreateHarness{
			CWD: f.cwd, PermissionMode: "readonly", MessageID: "msg-1",
		},
	}
}

// steps pulls every step.start payload out of a run of notifications.
func steps(ms []message) []itemEvent {
	var out []itemEvent
	for _, m := range ms {
		if m.Method != NotifyOutputItemAdded {
			continue
		}
		var p itemEvent
		if json.Unmarshal(m.Params, &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

func stepTypes(ms []message) []string {
	ss := steps(ms)
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Item.Type
	}
	return out
}

// srvRunningDone returns the done channel of whatever the server is running,
// or a closed channel when it is idle.
func (f *fixture) srvRunningDone() <-chan struct{} {
	<-f.done
	closed := make(chan struct{})
	close(closed)
	return closed
}

// assertStepLifecycle checks the ordering guarantee the protocol makes:
// every delta and every output_item.done names an item some
// output_item.added opened, and no item is done twice.
func assertStepLifecycle(t *testing.T, ms []message) {
	t.Helper()
	started := map[int]string{}
	stopped := map[int]bool{}
	for _, m := range ms {
		switch m.Method {
		case NotifyOutputItemAdded:
			var p itemEvent
			if err := json.Unmarshal(m.Params, &p); err != nil {
				t.Fatalf("decode output_item.added: %v", err)
			}
			if _, dup := started[p.OutputIndex]; dup {
				t.Errorf("output index %d was added twice", p.OutputIndex)
			}
			started[p.OutputIndex] = p.Item.Type
		case NotifyOutputTextDelta, NotifyReasoningTextDelta, NotifyFunctionCallArgsDelta:
			var p textDelta
			if err := json.Unmarshal(m.Params, &p); err != nil {
				t.Fatalf("decode %s: %v", m.Method, err)
			}
			if _, ok := started[p.OutputIndex]; !ok {
				t.Errorf("%s for index %d, which no output_item.added opened", m.Method, p.OutputIndex)
			}
			if stopped[p.OutputIndex] {
				t.Errorf("%s for index %d after its output_item.done", m.Method, p.OutputIndex)
			}
		case NotifyOutputItemDone:
			var p itemEvent
			if err := json.Unmarshal(m.Params, &p); err != nil {
				t.Fatalf("decode step.stop: %v", err)
			}
			if _, ok := started[p.OutputIndex]; !ok {
				t.Errorf("step.stop for index %d, which no step.start opened", p.OutputIndex)
			}
			if stopped[p.OutputIndex] {
				t.Errorf("step index %d was stopped twice", p.OutputIndex)
			}
			stopped[p.OutputIndex] = true
		}
	}
	for idx, kind := range started {
		if !stopped[idx] {
			t.Errorf("step %d (%s) was never stopped", idx, kind)
		}
	}
}

// stepOfType finds the first step.start of the given type.
func stepOfType(ms []message, kind string) (itemEvent, bool) {
	for _, s := range steps(ms) {
		if s.Item.Type == kind {
			return s, true
		}
	}
	return itemEvent{}, false
}

// textOf concatenates the output_text deltas of every item of the given
// type. Only an assistant message ever carries them: a user message item
// arrives complete on output_item.added.
func textOf(ms []message, kind string) string {
	want := map[int]bool{}
	for _, s := range steps(ms) {
		if s.Item.Type == kind {
			want[s.OutputIndex] = true
		}
	}
	var out string
	for _, m := range ms {
		if m.Method != NotifyOutputTextDelta {
			continue
		}
		var p textDelta
		if json.Unmarshal(m.Params, &p) != nil || !want[p.OutputIndex] {
			continue
		}
		out += p.Delta
	}
	return out
}

// summaryOf concatenates every reasoning_text delta.
func summaryOf(ms []message) string {
	var out string
	for _, m := range ms {
		if m.Method != NotifyReasoningTextDelta {
			continue
		}
		var p textDelta
		if json.Unmarshal(m.Params, &p) != nil {
			continue
		}
		out += p.Delta
	}
	return out
}

func stepResultText(s OutputItem) string {
	var out string
	for _, c := range s.Output {
		out += c.Text
	}
	return out
}

// mustFrame builds a request frame without waiting for its answer, for the
// pipelining test.
func mustFrame(t *testing.T, id, method string, params any) message {
	t.Helper()
	raw, err := json.Marshal(params)
	if err != nil {
		t.Fatalf("encode %s params: %v", method, err)
	}
	idRaw, _ := json.Marshal(id)
	return message{ID: idRaw, Method: method, Params: raw}
}
