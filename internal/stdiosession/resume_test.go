package stdiosession

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// resumeParams is a create body that picks a session up out of the state
// directory: no previous_interaction_id, which only ever resolves in the
// process that minted the id.
func resumeParams(sessionID, text string, toolDecls ...Tool) CreateParams {
	input, _ := json.Marshal(text)
	return CreateParams{
		Input:   input,
		Tools:   toolDecls,
		Harness: &CreateHarness{ResumeSessionID: sessionID, MessageID: "msg-resume"},
	}
}

// TestResumeAcrossProcesses is the conformance test for
// harness.resume_session_id: one server runs a session and is shut down, a
// second server opens the same state directory, and the session carries on
// with its history intact. This is the case previous_interaction_id cannot
// cover — the interaction id died with the first process.
func TestResumeAcrossProcesses(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()

	// Process A.
	a := newFixtureIn(t, dir, cwd, answer("first"))
	caps := a.client.handshake(ClientCapabilities{})
	if !caps.Capabilities.ResumeSession {
		t.Fatal("the handshake does not advertise resume_session")
	}
	var one CreateResult
	if rerr := a.client.call(MethodResponsesCreate, a.createParams("first task"), &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyResponseCompleted)
	sessionID := one.Response.Harness.SessionID
	if sessionID == "" {
		t.Fatal("the first interaction reported no harness.session_id")
	}

	// Shutdown: the client asks, then closes the pipe, which is how a
	// parent ends a session.
	if rerr := a.client.call(MethodShutdown, map[string]any{}, nil); rerr != nil {
		t.Fatalf("shutdown: %v", rerr)
	}
	a.close()

	// Process B, over the same state directory and nothing else.
	b := newFixtureIn(t, dir, cwd, answer("second"))
	b.client.handshake(ClientCapabilities{})

	// An interaction id from process A means nothing here, and the error
	// says which field does.
	stale := CreateParams{Model: testModel, PreviousResponseID: one.Response.ID}
	stale.Input, _ = json.Marshal("stale")
	rerr := b.client.call(MethodResponsesCreate, stale, &CreateResult{})
	if rerr == nil || rerr.Code != CodeRunNotFound {
		t.Fatalf("previous_interaction_id across processes: want %d, got %v", CodeRunNotFound, rerr)
	}
	if !strings.Contains(rerr.Message, "resume_session_id") {
		t.Errorf("the error does not point at resume_session_id: %s", rerr.Message)
	}

	var two CreateResult
	if rerr := b.client.call(MethodResponsesCreate, resumeParams(sessionID, "second task"), &two); rerr != nil {
		t.Fatalf("resume: %v", rerr)
	}
	b.client.waitFor(NotifyResponseCompleted)

	if two.Response.Harness.SessionID != sessionID {
		t.Errorf("the resumed response reports session %q, not %q", two.Response.Harness.SessionID, sessionID)
	}
	if two.Response.ID == one.Response.ID {
		t.Error("the resumed response reused the first process's interaction id")
	}
	if two.Response.Model != testModel {
		t.Errorf("the resumed response runs on %q, not the session's %q", two.Response.Model, testModel)
	}

	// The whole point: the model is sent the conversation the first process
	// recorded, plus the new instruction.
	b.script.mu.Lock()
	last := b.script.requests[len(b.script.requests)-1]
	b.script.mu.Unlock()
	for _, want := range []string{"first task", "first", "second task"} {
		if !strings.Contains(string(last), want) {
			t.Errorf("the resumed run's request does not carry %q", want)
		}
	}

	// And the chain continues in this process the ordinary way.
	next := CreateParams{Model: testModel, PreviousResponseID: two.Response.ID}
	next.Input, _ = json.Marshal("third task")
	var three CreateResult
	if rerr := b.client.call(MethodResponsesCreate, next, &three); rerr != nil {
		t.Fatalf("third create: %v", rerr)
	}
	b.client.waitFor(NotifyResponseCompleted)
	if three.Response.Harness.SessionID != sessionID {
		t.Errorf("the continued response left the session: %q", three.Response.Harness.SessionID)
	}
}

// TestPreviousInteractionResumesAfterCancel reproduces the bug report end to
// end: a client cancels an interaction mid-turn, and the very next message —
// sent the ordinary way, naming the cancelled interaction as
// previous_interaction_id — used to fail no matter what, with
// "session: resume: record continuation: store: session is cancelled".
// store.AppendEvents refuses to write to a "cancelled" row (the fence that
// stops a wedged goroutine from dirtying the log it was stopped in), and
// internal/session's Resume used to call it before lifting the row back to
// running, so a genuine resume tripped the fence meant for somebody else's
// write. This is the in-process continuation route: previous_interaction_id
// resolves against this server's own memory, reaching Runner.Resume directly
// with no reclaim logic in between (unlike harness.resume_session_id's
// resumeTarget).
func TestPreviousInteractionResumesAfterCancel(t *testing.T) {
	// callThen keeps the run going with a tool call, so there is a turn in
	// flight to cancel; one stream only, the way TestCancelEndsTheInteraction
	// uses it, so the fake server's clamp-to-last-stream behaviour has
	// nothing to disambiguate between the cancelled interaction's own
	// requests and the resumed one's — every request, before and after the
	// cancel, gets the same tool call and keeps the run going.
	f := newFixture(t, callThen("call-1", "TodoWrite", `{"todos":[]}`))
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("loop"), &created); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	f.client.waitFor(NotifyOutputItemAdded)

	var cancelled GetResult
	if rerr := f.client.call(MethodResponsesCancel, IDParams{ResponseID: created.Response.ID}, &cancelled); rerr != nil {
		t.Fatalf("cancel: %v", rerr)
	}
	if cancelled.Response.Status != StatusCancelled {
		t.Fatalf("status after cancel = %q, want %q", cancelled.Response.Status, StatusCancelled)
	}
	f.client.waitFor(NotifyResponseCompleted)
	sessionID := cancelled.Response.Harness.SessionID

	sess, err := f.srv.opts.Store.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusCancelled {
		t.Fatalf("stored session status = %q, want %q before resuming it", sess.Status, store.StatusCancelled)
	}

	next := CreateParams{Model: testModel, PreviousResponseID: created.Response.ID}
	next.Input, _ = json.Marshal("continue after the stop")
	var two CreateResult
	if rerr := f.client.call(MethodResponsesCreate, next, &two); rerr != nil {
		t.Fatalf("resume in the same process after a stop: %v", rerr)
	}
	f.client.waitFor(NotifyUsage)
	if two.Response.Harness.SessionID != sessionID {
		t.Errorf("the resumed response left the session: got %q, want %q", two.Response.Harness.SessionID, sessionID)
	}

	// The script never stops asking for another tool call, so the resumed
	// interaction runs until it is cancelled again. The point here is only
	// that it gets to run at all: a sub-turn reports its usage, and the
	// cancel lands on a running interaction rather than one the "session is
	// cancelled" store error the bug report hit on this exact path had
	// already failed.
	var again GetResult
	if rerr := f.client.call(MethodResponsesCancel, IDParams{ResponseID: two.Response.ID}, &again); rerr != nil {
		t.Fatalf("cancel the resumed response: %v", rerr)
	}
	if again.Response.Status != StatusCancelled {
		t.Fatalf("resumed interaction status after cancel = %q, want %q", again.Response.Status, StatusCancelled)
	}
	f.client.waitFor(NotifyResponseCompleted)

	sess, err = f.srv.opts.Store.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusCancelled {
		t.Fatalf("stored session status after the resume = %q, want %q", sess.Status, store.StatusCancelled)
	}
}

// TestResumeAcrossProcessesAfterCancel is TestPreviousInteractionResumesAfterCancel's
// counterpart for the route that survives a restart: harness.resume_session_id,
// resolved against the state directory rather than this process's memory
// (TestResumeAcrossProcesses). The session is left "cancelled" by the first
// process — no in-process resume happens in between — so the second process
// resumes it straight out of that status, the same shape
// docs/RUN-CONTROL.md's "Continuing" describes and the bug report's repro
// actually hit.
func TestResumeAcrossProcessesAfterCancel(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()
	a := newFixtureIn(t, dir, cwd, callThen("call-1", "TodoWrite", `{"todos":[]}`))
	a.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := a.client.call(MethodResponsesCreate, a.createParams("loop"), &created); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyOutputItemAdded)

	var cancelled GetResult
	if rerr := a.client.call(MethodResponsesCancel, IDParams{ResponseID: created.Response.ID}, &cancelled); rerr != nil {
		t.Fatalf("cancel: %v", rerr)
	}
	if cancelled.Response.Status != StatusCancelled {
		t.Fatalf("status after cancel = %q, want %q", cancelled.Response.Status, StatusCancelled)
	}
	a.client.waitFor(NotifyResponseCompleted)
	sessionID := cancelled.Response.Harness.SessionID

	if rerr := a.client.call(MethodShutdown, map[string]any{}, nil); rerr != nil {
		t.Fatalf("shutdown: %v", rerr)
	}
	a.close()

	// A fresh process over the same state directory. The row it reads back
	// for sessionID is still "cancelled" — the first process never resumed
	// it — which is exactly the state the bug report's repro left behind.
	b := newFixtureIn(t, dir, cwd, answer("continued after the restart"))
	b.client.handshake(ClientCapabilities{})

	var two CreateResult
	if rerr := b.client.call(MethodResponsesCreate, resumeParams(sessionID, "continue after the restart"), &two); rerr != nil {
		t.Fatalf("resume across processes after a stop: %v", rerr)
	}
	b.client.waitFor(NotifyResponseCompleted)
	if two.Response.Harness.SessionID != sessionID {
		t.Errorf("the resumed response left the session: got %q, want %q", two.Response.Harness.SessionID, sessionID)
	}

	sess, err := b.srv.opts.Store.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusOK {
		t.Fatalf("stored session status after the cross-process resume = %q, want %q", sess.Status, store.StatusOK)
	}
}

// TestResumeAcrossProcessesOfAMaxTurnsSession pins that a state directory an
// earlier binary wrote stays usable: a session row that binary finished at
// its sub-turn ceiling carries the status max_turns, which nothing writes now,
// and harness.resume_session_id still continues it.
func TestResumeAcrossProcessesOfAMaxTurnsSession(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()
	a := newFixtureIn(t, dir, cwd, answer("first"))
	a.client.handshake(ClientCapabilities{})
	var one CreateResult
	if rerr := a.client.call(MethodResponsesCreate, a.createParams("first task"), &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyResponseCompleted)
	sessionID := one.Response.Harness.SessionID
	finished := time.Now().UTC()
	if err := a.srv.opts.Store.UpdateSessionStatus(context.Background(), sessionID, store.StatusMaxTurns, &finished); err != nil {
		t.Fatal(err)
	}
	if rerr := a.client.call(MethodShutdown, map[string]any{}, nil); rerr != nil {
		t.Fatalf("shutdown: %v", rerr)
	}
	a.close()

	b := newFixtureIn(t, dir, cwd, answer("second"))
	b.client.handshake(ClientCapabilities{})
	var two CreateResult
	if rerr := b.client.call(MethodResponsesCreate, resumeParams(sessionID, "second task"), &two); rerr != nil {
		t.Fatalf("resume a max_turns session: %v", rerr)
	}
	b.client.waitFor(NotifyResponseCompleted)
	if two.Response.Harness.SessionID != sessionID {
		t.Errorf("the resumed response left the session: got %q, want %q", two.Response.Harness.SessionID, sessionID)
	}
	sess, err := b.srv.opts.Store.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusOK {
		t.Fatalf("stored session status after the resume = %q, want %q", sess.Status, store.StatusOK)
	}
}

// TestResumeUnknownSession pins that a session id from some other state
// directory is refused by name, with the state directory named as the likely
// cause.
func TestResumeUnknownSession(t *testing.T) {
	f := newFixture(t, answer("unused"))
	f.client.handshake(ClientCapabilities{})

	rerr := f.client.call(MethodResponsesCreate, resumeParams("sess-nothing", "go"), &CreateResult{})
	if rerr == nil || rerr.Code != CodeSessionNotFound {
		t.Fatalf("want %d, got %v", CodeSessionNotFound, rerr)
	}
	if !strings.Contains(rerr.Message, "-state-dir") {
		t.Errorf("the error does not name -state-dir: %s", rerr.Message)
	}
}

// TestResumeRefusesAChangedPrefix pins that the fields a session's prompt
// prefix is built from cannot be renamed on the way back in. Each of these
// would be silently ignored by session.Resume, which takes them off the row.
func TestResumeRefusesAChangedPrefix(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()
	a := newFixtureIn(t, dir, cwd, answer("first"))
	a.client.handshake(ClientCapabilities{})
	var one CreateResult
	if rerr := a.client.call(MethodResponsesCreate, a.createParams("first task"), &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyResponseCompleted)
	sessionID := one.Response.Harness.SessionID
	a.close()

	b := newFixtureIn(t, dir, cwd, answer("second"))
	b.client.handshake(ClientCapabilities{})

	cases := []struct {
		name string
		edit func(*CreateParams)
		want string
	}{
		{"a different directory", func(p *CreateParams) { p.Harness.CWD = t.TempDir() }, "cannot change directory"},
		{"a different permission mode", func(p *CreateParams) { p.Harness.PermissionMode = "full" }, "permission mode"},
		{"a different deny list", func(p *CreateParams) { p.Harness.Deny = []string{"git push"} }, "deny patterns"},
		{"a different model", func(p *CreateParams) { p.Model = testAltModel }, "cannot change model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := resumeParams(sessionID, "second task")
			tc.edit(&p)
			rerr := b.client.call(MethodResponsesCreate, p, &CreateResult{})
			if rerr == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if rerr.Code != CodeInvalidParams {
				t.Fatalf("want %d, got %d: %s", CodeInvalidParams, rerr.Code, rerr.Message)
			}
			if tc.want != "" && !strings.Contains(rerr.Message, tc.want) {
				t.Errorf("the error does not say why: %s", rerr.Message)
			}
		})
	}

	// Both ways of naming a conversation at once is a client bug, not a
	// precedence question.
	both := resumeParams(sessionID, "second task")
	both.PreviousResponseID = one.Response.ID
	rerr := b.client.call(MethodResponsesCreate, both, &CreateResult{})
	if rerr == nil || rerr.Code != CodeInvalidParams {
		t.Fatalf("both ids at once: want %d, got %v", CodeInvalidParams, rerr)
	}
}

// TestResumeChecksTheFrozenToolset pins the tool half of a resume: a client
// function the session froze has to come back with the same name and the
// same schema, and nothing may be added, because the array the run sends is
// the one on the session row rather than the one this create resolves.
func TestResumeChecksTheFrozenToolset(t *testing.T) {
	widget := Tool{
		Type: ToolFunction, Name: "show_widget",
		Description: "Draw a diagram",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"title":{"type":"string"}}}`),
		Harness:     &ToolHarness{ReadOnly: true},
	}

	dir, cwd := t.TempDir(), t.TempDir()
	a := newFixtureIn(t, dir, cwd, answer("first"))
	a.client.handshake(ClientCapabilities{FunctionCalls: true})
	first := a.createParams("first task")
	first.Tools = []Tool{widget}
	var one CreateResult
	if rerr := a.client.call(MethodResponsesCreate, first, &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyResponseCompleted)
	sessionID := one.Response.Harness.SessionID
	a.close()

	b := newFixtureIn(t, dir, cwd, answer("second"))
	b.client.handshake(ClientCapabilities{FunctionCalls: true})

	// Declaring nothing loses the tool.
	rerr := b.client.call(MethodResponsesCreate, resumeParams(sessionID, "second task"), &CreateResult{})
	if rerr == nil || rerr.Code != CodeToolsetMismatch {
		t.Fatalf("a dropped function tool: want %d, got %v", CodeToolsetMismatch, rerr)
	}
	if !strings.Contains(rerr.Message, "mcp__host__show_widget") {
		t.Errorf("the error does not name the tool: %s", rerr.Message)
	}

	// A different schema under the same name is the same problem.
	changed := widget
	changed.Parameters = json.RawMessage(`{"type":"object","properties":{"title":{"type":"number"}}}`)
	rerr = b.client.call(MethodResponsesCreate, resumeParams(sessionID, "second task", changed), &CreateResult{})
	if rerr == nil || rerr.Code != CodeToolsetMismatch {
		t.Fatalf("a changed schema: want %d, got %v", CodeToolsetMismatch, rerr)
	}
	if !strings.Contains(rerr.Message, "different parameter schema") {
		t.Errorf("the error does not say the schema moved: %s", rerr.Message)
	}

	// A tool the session never had cannot be added: the frozen array has
	// no room for it.
	extra := Tool{Type: ToolFunction, Name: "play_sound", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}
	rerr = b.client.call(MethodResponsesCreate, resumeParams(sessionID, "second task", widget, extra), &CreateResult{})
	if rerr == nil || rerr.Code != CodeToolsetMismatch {
		t.Fatalf("an added tool: want %d, got %v", CodeToolsetMismatch, rerr)
	}
	if !strings.Contains(rerr.Message, "mcp__host__play_sound") {
		t.Errorf("the error does not name the new tool: %s", rerr.Message)
	}

	// The same declaration, spelled with different whitespace and key
	// order, is the same toolset and is accepted.
	respelled := widget
	respelled.Parameters = json.RawMessage("{\n  \"properties\": {\"title\": {\"type\": \"string\"}},\n  \"type\": \"object\"\n}")
	var two CreateResult
	if rerr := b.client.call(MethodResponsesCreate, resumeParams(sessionID, "second task", respelled), &two); rerr != nil {
		t.Fatalf("resume with the same toolset: %v", rerr)
	}
	b.client.waitFor(NotifyResponseCompleted)
	if two.Response.Harness.SessionID != sessionID {
		t.Errorf("the resumed response reports session %q", two.Response.Harness.SessionID)
	}
}

// TestResumeChecksServerDeclarations pins both directions of the
// declaration check, and that it decides provenance without taking a
// qualified name apart.
func TestResumeChecksServerDeclarations(t *testing.T) {
	schema := json.RawMessage(`[
		{"type":"function","function":{"name":"Read","parameters":{}}},
		{"type":"function","function":{"name":"mcp__orchestrator__list_sessions","parameters":{"type":"object"}}},
		{"type":"function","function":{"name":"mcp__orchestrator__view_session","parameters":{"type":"object"}}},
		{"type":"function","function":{"name":"mcp__host__show_widget","parameters":{"type":"object"}}}
	]`)
	frozen, err := frozenToolsOf(schema, map[string]bool{"orchestrator": false, HostServerName: true})
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(frozen.schemas); got != 3 {
		t.Errorf("read %d mcp tools, want 3", got)
	}
	if _, ok := frozen.schemas["Read"]; ok {
		t.Error("a built-in tool was counted as part of the resumable toolset")
	}
	if len(frozen.servers) != 1 || !frozen.servers["orchestrator"] {
		t.Errorf("servers %v, want just orchestrator — the host namespace is not a server", frozen.servers)
	}

	// A server named only by the tool array, with no allowance recorded, is
	// still a server the session had: a row written before the allowance
	// column existed must stay resumable.
	if legacy, err := frozenToolsOf(schema, nil); err != nil {
		t.Fatalf("decode: %v", err)
	} else if !legacy.servers["orchestrator"] {
		t.Error("a server named only by the frozen tool array was not counted")
	}

	// A server the create forgets is named, and the host namespace is not
	// mistaken for one.
	rerr := checkDeclarations(frozen, nil)
	if rerr == nil || rerr.Code != CodeToolsetMismatch {
		t.Fatalf("a create declaring no servers: want %d, got %v", CodeToolsetMismatch, rerr)
	}
	if !strings.Contains(rerr.Message, "orchestrator") {
		t.Errorf("the error does not name the server: %s", rerr.Message)
	}
	if strings.Contains(rerr.Message, HostServerName) {
		t.Errorf("a client function was treated as needing a server: %s", rerr.Message)
	}

	orchestrator := Tool{Type: ToolMCPServer, Name: "orchestrator", URL: "http://127.0.0.1:60123/s/abc"}
	if rerr := checkDeclarations(frozen, []Tool{orchestrator}); rerr != nil {
		t.Errorf("a create re-declaring the server: %v", rerr)
	}

	// A server the create adds serves none of the frozen tools, so it could
	// never be reached. Saying so before it is written to the store is what
	// keeps a refused create from leaving a row behind.
	added := Tool{Type: ToolMCPServer, Name: "scratch", URL: "http://127.0.0.1:60124/s/abc"}
	rerr = checkDeclarations(frozen, []Tool{orchestrator, added})
	if rerr == nil || rerr.Code != CodeToolsetMismatch {
		t.Fatalf("an added server: want %d, got %v", CodeToolsetMismatch, rerr)
	}
	if !strings.Contains(rerr.Message, "scratch") {
		t.Errorf("the error does not name the added server: %s", rerr.Message)
	}
}

// TestServerNameDelimiterIsForbidden pins the grammar the qualified name
// depends on. Without it "mcp__foo__bar__tool" is tool "bar__tool" of server
// "foo" and tool "tool" of server "foo__bar" at once, and every reader that
// takes a server back out of a name — the read-only gate above all — has two
// answers to choose between.
func TestServerNameDelimiterIsForbidden(t *testing.T) {
	err := store.ValidateMCPServer(store.MCPServer{
		Name: "foo__bar", Transport: store.MCPTransportHTTP, URL: "http://127.0.0.1:1/x",
	})
	if err == nil {
		t.Fatal("a server name carrying the qualified-name delimiter was accepted")
	}
	if !strings.Contains(err.Error(), "__") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
	if err := store.ValidateMCPServer(store.MCPServer{
		Name: "foo_bar", Transport: store.MCPTransportHTTP, URL: "http://127.0.0.1:1/x",
	}); err != nil {
		t.Errorf("a single underscore is legal and was refused: %v", err)
	}
}

// TestHostNamespaceIsReadOnlyOnlyWhenEveryFunctionIs pins the conservative
// reading of a per-namespace permission seam. One writing tool among
// read-only ones must not be carried into a readonly session by them.
func TestHostNamespaceIsReadOnlyOnlyWhenEveryFunctionIs(t *testing.T) {
	ro := Tool{Type: ToolFunction, Name: "show_widget", Harness: &ToolHarness{ReadOnly: true}}
	rw := Tool{Type: ToolFunction, Name: "write_file"}

	for _, tc := range []struct {
		name  string
		decls []Tool
		want  bool
	}{
		{"every function read-only", []Tool{ro}, true},
		{"one function is not", []Tool{ro, rw}, false},
		{"declared the other way round", []Tool{rw, ro}, false},
		{"none read-only", []Tool{rw}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHostTools(nil, NewResponses(), "int_1", nil)
			for _, d := range tc.decls {
				if err := h.addFunction(d); err != nil {
					t.Fatalf("add %s: %v", d.Name, err)
				}
			}
			_, readOnly, err := h.Definitions(context.Background())
			if err != nil {
				t.Fatalf("definitions: %v", err)
			}
			if readOnly[HostServerName] != tc.want {
				t.Errorf("host namespace read-only = %v, want %v", readOnly[HostServerName], tc.want)
			}
		})
	}
}

// TestResumeRefusesAWidenedReadOnlyAllowance pins that a client cannot grant
// itself a permission the session never had by re-declaring its tools on the
// way back in. The first run's session is readonly and its one client
// function is not marked read-only; the resume marks it.
func TestResumeRefusesAWidenedReadOnlyAllowance(t *testing.T) {
	plain := Tool{
		Type: ToolFunction, Name: "show_widget",
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
	}

	dir, cwd := t.TempDir(), t.TempDir()
	a := newFixtureIn(t, dir, cwd, answer("first"))
	a.client.handshake(ClientCapabilities{FunctionCalls: true})
	first := a.createParams("first task")
	first.Tools = []Tool{plain}
	var one CreateResult
	if rerr := a.client.call(MethodResponsesCreate, first, &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyResponseCompleted)
	sessionID := one.Response.Harness.SessionID
	a.close()

	b := newFixtureIn(t, dir, cwd, answer("second"))
	b.client.handshake(ClientCapabilities{FunctionCalls: true})

	widened := plain
	widened.Harness = &ToolHarness{ReadOnly: true}
	rerr := b.client.call(MethodResponsesCreate, resumeParams(sessionID, "second task", widened), &CreateResult{})
	if rerr == nil || rerr.Code != CodeToolsetMismatch {
		t.Fatalf("a widened allowance: want %d, got %v", CodeToolsetMismatch, rerr)
	}
	if !strings.Contains(rerr.Message, "read-only") {
		t.Errorf("the error does not say what changed: %s", rerr.Message)
	}

	// Unchanged, the same resume is accepted.
	var two CreateResult
	if rerr := b.client.call(MethodResponsesCreate, resumeParams(sessionID, "second task", plain), &two); rerr != nil {
		t.Fatalf("resume with the same permissions: %v", rerr)
	}
	b.client.waitFor(NotifyResponseCompleted)
}

// TestMCPHeadersNeverReachTheStore pins that a bearer token a parent hands
// this process for a loopback server of its own stays in memory. A
// -state-dir the parent keeps so it can resume must not be a file of
// plaintext credentials.
func TestMCPHeadersNeverReachTheStore(t *testing.T) {
	f := newFixture(t, answer("unused"))
	f.client.handshake(ClientCapabilities{})

	const secret = "Bearer super-secret-token"
	srv := Tool{
		Type: ToolMCPServer, Name: "orchestrator",
		URL:     "http://127.0.0.1:59999/s/abc",
		Headers: map[string]string{"Authorization": secret},
	}
	if err := f.srv.registerServers(context.Background(), []Tool{srv}, map[string]bool{}); err != nil {
		t.Fatalf("register: %v", err)
	}

	row, err := f.srv.opts.Store.GetMCPServer(context.Background(), "orchestrator")
	if err != nil {
		t.Fatalf("read the row back: %v", err)
	}
	if len(row.Headers) != 0 {
		t.Errorf("the stored row carries headers: %v", row.Headers)
	}

	// Every byte of the database, not just the column this reads: a
	// credential that leaked into any row would still be on disk.
	db, err := os.ReadFile(filepath.Join(f.stateDir, "session.db"))
	if err != nil {
		t.Fatalf("read the database: %v", err)
	}
	if bytes.Contains(db, []byte("super-secret-token")) {
		t.Error("the credential is on disk in the state directory")
	}

	// And it is put back for the length of a dial, or nothing could be
	// reached at all.
	dialled := f.srv.dialSecrets(row)
	if dialled.Headers["Authorization"] != secret {
		t.Errorf("the dial did not get the header back: %v", dialled.Headers)
	}
}

// TestCompletionFreesTheSlotBeforeItIsAnnounced pins the ordering a client
// reacting to interaction.completed depends on: by the time the frame is
// written, the next create is already accepted. It runs a chain rather than
// one turn because the failure it guards against was intermittent.
func TestCompletionFreesTheSlotBeforeItIsAnnounced(t *testing.T) {
	f := newFixture(t, answer("one"), answer("two"), answer("three"), answer("four"))
	f.client.handshake(ClientCapabilities{})

	prev := ""
	for i := 0; i < 4; i++ {
		p := f.createParams("task")
		if prev != "" {
			p.PreviousResponseID = prev
			p.Harness.CWD = ""
		}
		var res CreateResult
		if rerr := f.client.call(MethodResponsesCreate, p, &res); rerr != nil {
			t.Fatalf("create %d: %v", i, rerr)
		}
		// The next create goes out the moment interaction.completed
		// arrives, which is exactly what used to race the bookkeeping.
		f.client.waitFor(NotifyResponseCompleted)
		prev = res.Response.ID
	}
}

// TestResumeWithAServerThatAdvertisedNothing pins the case a probe failure
// leaves behind: a declared server that contributed no tools. A create
// tolerates that — the run carries on with whatever the server did advertise,
// which is none of it — so the session's frozen tool array names it nowhere,
// and the only record that it was ever declared is its entry in the
// read-only allowance. A resume has to be able to reproduce it either way.
func TestResumeWithAServerThatAdvertisedNothing(t *testing.T) {
	// Nothing is listening on this port, so the probe fails and the server
	// joins the session having advertised no tools at all.
	server := Tool{
		Type: ToolMCPServer, Name: "orchestrator",
		URL:     "http://127.0.0.1:59998/s/abc",
		Harness: &ToolHarness{ReadOnly: true},
	}

	dir, cwd := t.TempDir(), t.TempDir()
	a := newFixtureIn(t, dir, cwd, answer("first"))
	a.client.handshake(ClientCapabilities{})
	first := a.createParams("first task")
	first.Tools = []Tool{server}
	var one CreateResult
	if rerr := a.client.call(MethodResponsesCreate, first, &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyResponseCompleted)
	sessionID := one.Response.Harness.SessionID
	a.close()

	b := newFixtureIn(t, dir, cwd, answer("second"), answer("third"))
	b.client.handshake(ClientCapabilities{})

	// Re-declaring it is what the parent should do, and it has to work.
	var two CreateResult
	if rerr := b.client.call(MethodResponsesCreate, resumeParams(sessionID, "second task", server), &two); rerr != nil {
		t.Fatalf("resume re-declaring the server: %v", rerr)
	}
	b.client.waitFor(NotifyResponseCompleted)
	if two.Response.Harness.SessionID != sessionID {
		t.Errorf("the resumed response reports session %q", two.Response.Harness.SessionID)
	}

	// Dropping it is refused, and named as the server it is — a session
	// keeps the servers it was started with whether or not any of them
	// managed to advertise a tool.
	rerr := b.client.call(MethodResponsesCreate, resumeParams(sessionID, "third task"), &CreateResult{})
	if rerr == nil || rerr.Code != CodeToolsetMismatch {
		t.Fatalf("dropping the server: want %d, got %v", CodeToolsetMismatch, rerr)
	}
	if !strings.Contains(rerr.Message, "orchestrator") {
		t.Errorf("the error does not name the server: %s", rerr.Message)
	}
}
