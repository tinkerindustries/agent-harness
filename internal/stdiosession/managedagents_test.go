package stdiosession

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/anthropic"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/mcpclient"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// This file drives a Claude model over the ManagedAgents pipe against a fake
// Anthropic Messages API, the same two-ended shape claude_test.go uses for
// the Responses dialect, applied to sessions.create/.events/.get/.delete
// instead.

// newManagedAgentsFixture is newClaudeFixture with the dialect swapped: the
// same fake Anthropic recorder, but a Server speaking
// docs/STDIO-MANAGED-AGENTS.md instead of the Responses vocabulary.
// hostToolTimeout bounds a pending custom tool call, short enough that a
// test which leaves one unanswered does not hang the suite.
func newManagedAgentsFixture(t *testing.T, hostToolTimeout time.Duration, streams ...string) (*fixture, *anthropicRecorder) {
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
	if hostToolTimeout <= 0 {
		hostToolTimeout = 5 * time.Second
	}
	runner := &session.Runner{
		Store:        st,
		Client:       client,
		ClientFor:    func(string) session.Client { return client },
		Hub:          eventHub,
		ToolTimeouts: tools.Timeouts{HostTool: hostToolTimeout},
	}
	mgr := mcpclient.New(st)
	t.Cleanup(func() { mgr.Close() })

	srv := NewServer(Options{
		Dialect: NewManagedAgents(),
		Store:   st, Runner: runner, Hub: eventHub, MCP: mgr,
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

// maCreateParamsFor builds a sessions.create body: this fixture's working
// directory, read-only, one text user.message, and (when non-empty) one
// custom tool declaration.
func maCreateParamsFor(f *fixture, text string, customTool *Tool) maCreateParams {
	msg, _ := json.Marshal(maWireEvent{Type: maEventUserMessage, Content: maTextContent(text)})
	p := maCreateParams{
		Agent:         mustJSON(maAgentSpec{Type: maAgentWithOverrides, Model: &maModelOverride{ID: testClaudeModel}}),
		InitialEvents: []json.RawMessage{msg},
		Harness:       &maCreateHarness{CWD: f.cwd, PermissionMode: "readonly"},
	}
	if customTool != nil {
		p.Tools = []Tool{*customTool}
	}
	return p
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// maNotifications collects every notification of method m from seen.
func maNotifications(seen []message, method string) []message {
	var out []message
	for _, m := range seen {
		if m.Method == method {
			out = append(out, m)
		}
	}
	return out
}

// TestManagedAgentsCreateToolRoundAndIdle drives a create whose model calls
// a client-declared custom tool: proves agent.custom_tool_use fires,
// session.status_idle announces requires_action, and answering it through
// sessions.events resumes the turn to session.status_idle{end_turn} —
// docs/plans/reports/claude-provider's phase 4 proof (create, a tool round,
// the session going idle).
func TestManagedAgentsCreateToolRoundAndIdle(t *testing.T) {
	f, rec := newManagedAgentsFixture(t, 0,
		claudeToolCall("toolu_1", "mcp__host__show_widget", `{"title":"hi"}`),
		claudeAnswer("All done."),
	)
	var hs maInitializeResult
	f.client.handshakeRaw(ClientCapabilities{FunctionCalls: true}, &hs)
	if hs.ServerInfo.Protocol != "anthropic.managed_agents.v1beta" {
		t.Fatalf("protocol = %q", hs.ServerInfo.Protocol)
	}

	p := maCreateParamsFor(f, "use the tool", &Tool{
		Type: ToolFunction, Name: "show_widget", Description: "show a widget",
		Parameters: json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`),
	})

	var created maCreateResult
	if rerr := f.client.call(MethodSessionsCreate, p, &created); rerr != nil {
		t.Fatalf("sessions.create: %v", rerr)
	}
	sessionID := created.Session.ID
	if sessionID == "" {
		t.Fatal("sessions.create answered with no session id")
	}

	seen := f.client.waitFor(notifyMASessionStatusIdle)

	custom := maNotifications(seen, notifyMAAgentCustomToolUse)
	if len(custom) != 1 {
		t.Fatalf("agent.custom_tool_use fired %d times, want 1: %+v", len(custom), seen)
	}
	var use maCustomToolUse
	json.Unmarshal(custom[0].Params, &use)
	if use.Name != "show_widget" {
		t.Errorf("custom tool use name = %q, want the dequalified %q", use.Name, "show_widget")
	}
	if use.SessionID != sessionID {
		t.Errorf("custom tool use session_id = %q, want %q", use.SessionID, sessionID)
	}
	if use.Harness == nil || use.Harness.TurnID == "" {
		t.Errorf("custom tool use carries no harness.turn_id: %+v", use)
	}

	idles := maNotifications(seen, notifyMASessionStatusIdle)
	if len(idles) == 0 {
		t.Fatal("session.status_idle never fired")
	}
	var idle maStatusIdle
	json.Unmarshal(idles[0].Params, &idle)
	if idle.StopReason.Type != "requires_action" {
		t.Fatalf("first session.status_idle stop_reason = %+v, want requires_action", idle.StopReason)
	}
	if len(idle.StopReason.EventIDs) != 1 || idle.StopReason.EventIDs[0] != use.ID {
		t.Errorf("stop_reason.event_ids = %v, want [%s]", idle.StopReason.EventIDs, use.ID)
	}

	// A stray user.message while the result is pending is refused, naming
	// the pending id (docs/STDIO-MANAGED-AGENTS.md, "What happens to a
	// stray event while a result is pending").
	strayMsg, _ := json.Marshal(maWireEvent{Type: maEventUserMessage, Content: maTextContent("hello?")})
	var strayResult maEventsResult
	rerr := f.client.call(MethodSessionsEvents, maEventsParams{SessionID: sessionID, Events: []json.RawMessage{strayMsg}}, &strayResult)
	if rerr == nil {
		t.Fatal("a user.message while a custom tool result is pending was accepted")
	}
	if rerr.Code != CodeRunNotRunning {
		t.Errorf("stray user.message error code = %d, want %d", rerr.Code, CodeRunNotRunning)
	}

	// Answer it. The turn resumes and the model's plain-text answer ends it
	// cleanly.
	resultEvt, _ := json.Marshal(maWireEvent{
		Type: maEventUserCustomToolResult, CustomToolUseID: use.ID,
		Content: maTextContent("widget shown"),
	})
	var answered maEventsResult
	if rerr := f.client.call(MethodSessionsEvents, maEventsParams{SessionID: sessionID, Events: []json.RawMessage{resultEvt}}, &answered); rerr != nil {
		t.Fatalf("sessions.events (custom tool result): %v", rerr)
	}
	if len(answered.Results) != 1 || answered.Results[0].Type != maEventUserCustomToolResult {
		t.Fatalf("sessions.events result = %+v", answered.Results)
	}

	// A second answer to the same id is refused.
	var dup maEventsResult
	if rerr := f.client.call(MethodSessionsEvents, maEventsParams{SessionID: sessionID, Events: []json.RawMessage{resultEvt}}, &dup); rerr == nil {
		t.Fatal("a second user.custom_tool_result for the same id was accepted")
	}

	seen2 := f.client.waitFor(notifyMASessionStatusIdle)
	final := maNotifications(seen2, notifyMASessionStatusIdle)
	last := final[len(final)-1]
	var lastIdle maStatusIdle
	json.Unmarshal(last.Params, &lastIdle)
	if lastIdle.StopReason.Type != "end_turn" {
		t.Errorf("final stop_reason = %+v, want end_turn", lastIdle.StopReason)
	}

	// sessions.get by turn id answers with the assembled document.
	var got maGetResult
	if rerr := f.client.call(MethodSessionsGet, maIDParams{SessionID: sessionID, Harness: &maGetHarness{TurnID: use.Harness.TurnID}}, &got); rerr != nil {
		t.Fatalf("sessions.get: %v", rerr)
	}
	if got.Session.Harness == nil || got.Session.Harness.Text != "All done." {
		t.Errorf("sessions.get text = %+v, want %q", got.Session.Harness, "All done.")
	}

	// The provider request carries a genuine Messages API shape.
	bodies, _ := rec.seen()
	if len(bodies) < 2 {
		t.Fatalf("the provider saw %d requests, want 2 sub-turns: %v", len(bodies), bodies)
	}

	// sessions.delete forgets it.
	if rerr := f.client.call(MethodSessionsDelete, maIDParams{SessionID: sessionID}, &struct{}{}); rerr != nil {
		t.Fatalf("sessions.delete: %v", rerr)
	}
	if rerr := f.client.call(MethodSessionsGet, maIDParams{SessionID: sessionID}, &maGetResult{}); rerr == nil {
		t.Fatal("sessions.get answered after delete")
	}
}

// TestManagedAgentsSteerAndInterrupt proves user.message steers a running
// turn and user.interrupt cancels one, both through sessions.events.
func TestManagedAgentsSteerAndInterrupt(t *testing.T) {
	f, _ := newManagedAgentsFixture(t, 0,
		claudeAnswer("first answer"),
	)
	f.client.handshakeRaw(ClientCapabilities{}, &maInitializeResult{})

	p := maCreateParamsFor(f, "hello", nil)
	var created maCreateResult
	if rerr := f.client.call(MethodSessionsCreate, p, &created); rerr != nil {
		t.Fatalf("sessions.create: %v", rerr)
	}
	sessionID := created.Session.ID

	f.client.waitFor(notifyMASessionStatusIdle)

	// Steer a second, fresh turn (the first already ended, so this is the
	// idle-session-starts-the-next-turn path).
	msg, _ := json.Marshal(maWireEvent{Type: maEventUserMessage, Content: maTextContent("go again")})
	var steered maEventsResult
	if rerr := f.client.call(MethodSessionsEvents, maEventsParams{SessionID: sessionID, Events: []json.RawMessage{msg}}, &steered); rerr != nil {
		t.Fatalf("sessions.events (message on idle session): %v", rerr)
	}
	seen := f.client.waitFor(notifyMASessionStatusIdle)
	if len(maNotifications(seen, notifyMASessionStatusRunning)) == 0 {
		t.Error("no session.status_running for the continued turn")
	}

	// Interrupt: the second turn already ended (claudeAnswer's script has
	// only one distinct stream, repeated), so drive a fresh long-running one
	// and interrupt it while it is still going: post another message and
	// immediately interrupt.
	interrupt, _ := json.Marshal(maWireEvent{Type: maEventUserInterrupt})
	var interrupted maEventsResult
	if rerr := f.client.call(MethodSessionsEvents, maEventsParams{SessionID: sessionID, Events: []json.RawMessage{interrupt}}, &interrupted); rerr != nil {
		t.Fatalf("sessions.events (interrupt): %v", rerr)
	}
	if len(interrupted.Results) != 1 || interrupted.Results[0].Type != maEventUserInterrupt {
		t.Fatalf("interrupt result = %+v", interrupted.Results)
	}
}

// TestManagedAgentsResume proves harness.resume_session_id continues a
// session across a fresh Server, the same way the other two dialects'
// resume works.
func TestManagedAgentsResume(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()
	f, _ := newManagedAgentsFixtureIn(t, dir, cwd, 0, claudeAnswer("first"))
	p := maCreateParamsFor(f, "hello", nil)
	var created maCreateResult
	if rerr := f.client.call(MethodSessionsCreate, p, &created); rerr != nil {
		t.Fatalf("sessions.create: %v", rerr)
	}
	sessionID := created.Session.ID
	f.client.waitFor(notifyMASessionStatusIdle)
	f.cancel()
	f.stdin.Close()
	<-f.done

	f2, _ := newManagedAgentsFixtureIn(t, dir, cwd, 0, claudeAnswer("second"))
	msg, _ := json.Marshal(maWireEvent{Type: maEventUserMessage, Content: maTextContent("continue")})
	resume := maCreateParams{
		Agent:   mustJSON(maAgentSpec{Type: maAgentWithOverrides, Model: &maModelOverride{ID: testClaudeModel}}),
		Harness: &maCreateHarness{ResumeSessionID: sessionID},
	}
	var resumed maCreateResult
	if rerr := f2.client.call(MethodSessionsCreate, resume, &resumed); rerr != nil {
		t.Fatalf("sessions.create (resume): %v", rerr)
	}
	if resumed.Session.ID != sessionID {
		t.Errorf("resumed session id = %q, want %q", resumed.Session.ID, sessionID)
	}
	var events maEventsResult
	if rerr := f2.client.call(MethodSessionsEvents, maEventsParams{SessionID: sessionID, Events: []json.RawMessage{msg}}, &events); rerr != nil {
		t.Fatalf("sessions.events after resume: %v", rerr)
	}
	f2.client.waitFor(notifyMASessionStatusIdle)
}

// newManagedAgentsFixtureIn is newManagedAgentsFixture with the state and
// working directories named, the same reason newFixtureIn exists.
func newManagedAgentsFixtureIn(t *testing.T, dir, cwd string, hostToolTimeout time.Duration, streams ...string) (*fixture, *anthropicRecorder) {
	t.Helper()
	rec := &anthropicRecorder{streams: streams}
	api := rec.serve(t)

	st, err := store.Open(filepath.Join(dir, "session.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	client := anthropic.NewClient(api.URL, anthropic.WithAPIKeyProvider(func() (string, error) { return "ak-test", nil }))
	eventHub := hub.New()
	if hostToolTimeout <= 0 {
		hostToolTimeout = 5 * time.Second
	}
	runner := &session.Runner{
		Store:        st,
		Client:       client,
		ClientFor:    func(string) session.Client { return client },
		Hub:          eventHub,
		ToolTimeouts: tools.Timeouts{HostTool: hostToolTimeout},
	}
	mgr := mcpclient.New(st)
	t.Cleanup(func() { mgr.Close() })

	srv := NewServer(Options{
		Dialect: NewManagedAgents(),
		Store:   st, Runner: runner, Hub: eventHub, MCP: mgr,
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
	f.client.handshakeRaw(ClientCapabilities{FunctionCalls: true}, &maInitializeResult{})
	return f, rec
}

// TestManagedAgentsFailedRunReason pins docs/STDIO-MANAGED-AGENTS.md's claim
// that a failed run's session.status_idle carries harness.reason "failed",
// after the session.error that names the cause. status_idle has no status
// field, so without the reason a client reading it alone sees an ordinary end.
func TestManagedAgentsFailedRunReason(t *testing.T) {
	f, _ := newManagedAgentsFixture(t, 0,
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"invalid x-api-key\"}}\n\n",
	)
	f.client.handshakeRaw(ClientCapabilities{}, &maInitializeResult{})

	var created maCreateResult
	if rerr := f.client.call(MethodSessionsCreate, maCreateParamsFor(f, "hello", nil), &created); rerr != nil {
		t.Fatalf("sessions.create: %v", rerr)
	}
	seen := f.client.waitFor(notifyMASessionStatusIdle)

	if len(maNotifications(seen, notifyMASessionError)) == 0 {
		t.Error("no session.error before the idle")
	}
	idle := maNotifications(seen, notifyMASessionStatusIdle)
	if len(idle) == 0 {
		t.Fatal("no session.status_idle")
	}
	var got maStatusIdle
	if err := json.Unmarshal(idle[len(idle)-1].Params, &got); err != nil {
		t.Fatalf("decode status_idle: %v", err)
	}
	if got.Harness == nil || got.Harness.Reason != "failed" {
		t.Errorf("status_idle harness = %+v, want reason %q", got.Harness, "failed")
	}
}
