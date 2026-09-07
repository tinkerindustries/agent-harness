package geministdio

import (
	"encoding/json"
	"strings"
	"testing"
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
	if rerr := a.client.call(MethodInteractionsCreate, a.createParams("first task"), &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyInteractionCompleted)
	sessionID := one.Interaction.Harness.SessionID
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
	stale := CreateParams{Model: testModel, PreviousInteractionID: one.Interaction.ID}
	stale.Input, _ = json.Marshal("stale")
	rerr := b.client.call(MethodInteractionsCreate, stale, &CreateResult{})
	if rerr == nil || rerr.Code != CodeInteractionNotFound {
		t.Fatalf("previous_interaction_id across processes: want %d, got %v", CodeInteractionNotFound, rerr)
	}
	if !strings.Contains(rerr.Message, "resume_session_id") {
		t.Errorf("the error does not point at resume_session_id: %s", rerr.Message)
	}

	var two CreateResult
	if rerr := b.client.call(MethodInteractionsCreate, resumeParams(sessionID, "second task"), &two); rerr != nil {
		t.Fatalf("resume: %v", rerr)
	}
	b.client.waitFor(NotifyInteractionCompleted)

	if two.Interaction.Harness.SessionID != sessionID {
		t.Errorf("the resumed interaction reports session %q, not %q", two.Interaction.Harness.SessionID, sessionID)
	}
	if two.Interaction.ID == one.Interaction.ID {
		t.Error("the resumed interaction reused the first process's interaction id")
	}
	if two.Interaction.Model != testModel {
		t.Errorf("the resumed interaction runs on %q, not the session's %q", two.Interaction.Model, testModel)
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
	next := CreateParams{Model: testModel, PreviousInteractionID: two.Interaction.ID}
	next.Input, _ = json.Marshal("third task")
	var three CreateResult
	if rerr := b.client.call(MethodInteractionsCreate, next, &three); rerr != nil {
		t.Fatalf("third create: %v", rerr)
	}
	b.client.waitFor(NotifyInteractionCompleted)
	if three.Interaction.Harness.SessionID != sessionID {
		t.Errorf("the continued interaction left the session: %q", three.Interaction.Harness.SessionID)
	}
}

// TestResumeUnknownSession pins that a session id from some other state
// directory is refused by name, with the state directory named as the likely
// cause.
func TestResumeUnknownSession(t *testing.T) {
	f := newFixture(t, answer("unused"))
	f.client.handshake(ClientCapabilities{})

	rerr := f.client.call(MethodInteractionsCreate, resumeParams("sess-nothing", "go"), &CreateResult{})
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
	if rerr := a.client.call(MethodInteractionsCreate, a.createParams("first task"), &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyInteractionCompleted)
	sessionID := one.Interaction.Harness.SessionID
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
		{"a different model", func(p *CreateParams) { p.Model = "deepseek-v4-flash" }, "cannot change model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := resumeParams(sessionID, "second task")
			tc.edit(&p)
			rerr := b.client.call(MethodInteractionsCreate, p, &CreateResult{})
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
	both.PreviousInteractionID = one.Interaction.ID
	rerr := b.client.call(MethodInteractionsCreate, both, &CreateResult{})
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
	if rerr := a.client.call(MethodInteractionsCreate, first, &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyInteractionCompleted)
	sessionID := one.Interaction.Harness.SessionID
	a.close()

	b := newFixtureIn(t, dir, cwd, answer("second"))
	b.client.handshake(ClientCapabilities{FunctionCalls: true})

	// Declaring nothing loses the tool.
	rerr := b.client.call(MethodInteractionsCreate, resumeParams(sessionID, "second task"), &CreateResult{})
	if rerr == nil || rerr.Code != CodeToolsetMismatch {
		t.Fatalf("a dropped function tool: want %d, got %v", CodeToolsetMismatch, rerr)
	}
	if !strings.Contains(rerr.Message, "mcp__host__show_widget") {
		t.Errorf("the error does not name the tool: %s", rerr.Message)
	}

	// A different schema under the same name is the same problem.
	changed := widget
	changed.Parameters = json.RawMessage(`{"type":"object","properties":{"title":{"type":"number"}}}`)
	rerr = b.client.call(MethodInteractionsCreate, resumeParams(sessionID, "second task", changed), &CreateResult{})
	if rerr == nil || rerr.Code != CodeToolsetMismatch {
		t.Fatalf("a changed schema: want %d, got %v", CodeToolsetMismatch, rerr)
	}
	if !strings.Contains(rerr.Message, "different parameter schema") {
		t.Errorf("the error does not say the schema moved: %s", rerr.Message)
	}

	// A tool the session never had cannot be added: the frozen array has
	// no room for it.
	extra := Tool{Type: ToolFunction, Name: "play_sound", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}
	rerr = b.client.call(MethodInteractionsCreate, resumeParams(sessionID, "second task", widget, extra), &CreateResult{})
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
	if rerr := b.client.call(MethodInteractionsCreate, resumeParams(sessionID, "second task", respelled), &two); rerr != nil {
		t.Fatalf("resume with the same toolset: %v", rerr)
	}
	b.client.waitFor(NotifyInteractionCompleted)
	if two.Interaction.Harness.SessionID != sessionID {
		t.Errorf("the resumed interaction reports session %q", two.Interaction.Harness.SessionID)
	}
}

// TestResumeRequiresEveryFrozenServerRedeclared pins the connection-metadata
// half. The url and headers a parent stood a server up on last time died with
// the process that spawned it, so a resume has to supply the ones that are
// good now — and a server the create forgets is named before anything is
// dialled, rather than turning into missing tools later.
func TestResumeRequiresEveryFrozenServerRedeclared(t *testing.T) {
	frozen, err := frozenToolsOf(json.RawMessage(`[
		{"type":"function","function":{"name":"Read","parameters":{}}},
		{"type":"function","function":{"name":"mcp__orchestrator__list_sessions","parameters":{"type":"object"}}},
		{"type":"function","function":{"name":"mcp__orchestrator__view_session","parameters":{"type":"object"}}},
		{"type":"function","function":{"name":"mcp__host__show_widget","parameters":{"type":"object"}}}
	]`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := len(frozen.schemas); got != 3 {
		t.Errorf("read %d mcp tools, want 3", got)
	}
	if _, ok := frozen.schemas["Read"]; ok {
		t.Error("a built-in tool was counted as part of the resumable toolset")
	}
	if len(frozen.servers) != 1 || frozen.servers[0] != "orchestrator" {
		t.Errorf("servers %v, want [orchestrator] — the host namespace is not a server", frozen.servers)
	}

	if rerr := requireDeclaredServers(frozen, nil); rerr == nil || rerr.Code != CodeToolsetMismatch {
		t.Fatalf("a create declaring no servers: want %d, got %v", CodeToolsetMismatch, rerr)
	} else if !strings.Contains(rerr.Message, "orchestrator") {
		t.Errorf("the error does not name the server: %s", rerr.Message)
	}

	decls := []Tool{{Type: ToolMCPServer, Name: "orchestrator", URL: "http://127.0.0.1:60123/s/abc"}}
	if rerr := requireDeclaredServers(frozen, decls); rerr != nil {
		t.Errorf("a create re-declaring the server: %v", rerr)
	}
}
