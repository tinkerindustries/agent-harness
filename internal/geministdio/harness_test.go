package geministdio

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
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// The tests drive a real Server over an in-memory pipe, against a real
// session.Runner whose provider client is internal/gemini pointed at a
// scripted event-stream server. Nothing here reaches Google: the SSE bodies
// are built by geminitest, the same builder internal/gemini's own tests use.

const testModel = "gemini-3.7-flash"

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
	t      *testing.T
	client *client
	script *scriptedGemini
	cwd    string
	srv    *Server
	cancel context.CancelFunc
	done   chan struct{}
	stdin  io.WriteCloser
}

func newFixture(t *testing.T, streams ...string) *fixture {
	t.Helper()
	script := &scriptedGemini{streams: streams}
	api := script.serve()
	t.Cleanup(api.Close)

	dir := t.TempDir()
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

	srv := NewServer(Options{
		Store: st, Runner: runner, Hub: eventHub,
		Models: []string{testModel}, DefaultModel: testModel,
		HasAPIKey: func() bool { return true },
		Version:   "test",
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

	cwd := t.TempDir()
	f := &fixture{
		t: t, client: newClient(t, serverIn, serverOut), script: script,
		cwd: cwd, srv: srv, cancel: cancel, done: done, stdin: serverIn,
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
func steps(ms []message) []stepStart {
	var out []stepStart
	for _, m := range ms {
		if m.Method != NotifyStepStart {
			continue
		}
		var p stepStart
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
		out[i] = s.Step.Type
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

// assertStepLifecycle checks the ordering guarantee the protocol makes: every
// step.delta and step.stop names a step some step.start opened, and no step is
// stopped twice.
func assertStepLifecycle(t *testing.T, ms []message) {
	t.Helper()
	started := map[int]string{}
	stopped := map[int]bool{}
	for _, m := range ms {
		switch m.Method {
		case NotifyStepStart:
			var p stepStart
			if err := json.Unmarshal(m.Params, &p); err != nil {
				t.Fatalf("decode step.start: %v", err)
			}
			if _, dup := started[p.Index]; dup {
				t.Errorf("step index %d was started twice", p.Index)
			}
			started[p.Index] = p.Step.Type
		case NotifyStepDelta:
			var p stepDelta
			if err := json.Unmarshal(m.Params, &p); err != nil {
				t.Fatalf("decode step.delta: %v", err)
			}
			if _, ok := started[p.Index]; !ok {
				t.Errorf("step.delta for index %d, which no step.start opened", p.Index)
			}
			if stopped[p.Index] {
				t.Errorf("step.delta for index %d after its step.stop", p.Index)
			}
		case NotifyStepStop:
			var p stepStop
			if err := json.Unmarshal(m.Params, &p); err != nil {
				t.Fatalf("decode step.stop: %v", err)
			}
			if _, ok := started[p.Index]; !ok {
				t.Errorf("step.stop for index %d, which no step.start opened", p.Index)
			}
			if stopped[p.Index] {
				t.Errorf("step index %d was stopped twice", p.Index)
			}
			stopped[p.Index] = true
		}
	}
	for idx, kind := range started {
		if !stopped[idx] {
			t.Errorf("step %d (%s) was never stopped", idx, kind)
		}
	}
}

// stepOfType finds the first step.start of the given type.
func stepOfType(ms []message, kind string) (stepStart, bool) {
	for _, s := range steps(ms) {
		if s.Step.Type == kind {
			return s, true
		}
	}
	return stepStart{}, false
}

// textOf concatenates the text deltas of every step of the given type.
func textOf(ms []message, kind string) string {
	want := map[int]bool{}
	for _, s := range steps(ms) {
		if s.Step.Type == kind {
			want[s.Index] = true
		}
	}
	var out string
	for _, m := range ms {
		if m.Method != NotifyStepDelta {
			continue
		}
		var p stepDelta
		if json.Unmarshal(m.Params, &p) != nil || !want[p.Index] {
			continue
		}
		if p.Delta.Type == DeltaText {
			out += p.Delta.Text
		}
	}
	return out
}

// summaryOf concatenates every thought_summary delta.
func summaryOf(ms []message) string {
	var out string
	for _, m := range ms {
		if m.Method != NotifyStepDelta {
			continue
		}
		var p stepDelta
		if json.Unmarshal(m.Params, &p) != nil {
			continue
		}
		if p.Delta.Type == DeltaThoughtSummary && p.Delta.Content != nil {
			out += p.Delta.Content.Text
		}
	}
	return out
}

func stepResultText(s Step) string {
	var out string
	for _, c := range s.Result {
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
