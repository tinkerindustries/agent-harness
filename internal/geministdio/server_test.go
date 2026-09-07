package geministdio

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestHandshakeGatesEverything pins that nothing runs before initialize and
// its acknowledgement, and that an unrecognised notification is ignored
// rather than answered with an error.
func TestHandshakeGatesEverything(t *testing.T) {
	f := newFixture(t, answer("hello"))

	rerr := f.client.call(MethodInteractionsCreate, f.createParams("do a thing"), nil)
	if rerr == nil {
		t.Fatal("interactions.create was accepted before the handshake")
	}
	if rerr.Code != CodeNotInitialized {
		t.Errorf("code = %d, want %d", rerr.Code, CodeNotInitialized)
	}

	res := f.client.handshake(ClientCapabilities{})
	if res.ServerInfo.Protocol != "google.interactions.v1beta" {
		t.Errorf("protocol = %q", res.ServerInfo.Protocol)
	}
	if res.DefaultModel != testModel {
		t.Errorf("default model = %q, want %q", res.DefaultModel, testModel)
	}
	if !res.Capabilities.Append || !res.Capabilities.Cancel || !res.Capabilities.PreviousInteraction {
		t.Errorf("capabilities = %+v", res.Capabilities)
	}

	// A notification the server has never heard of must be dropped in
	// silence: answering it would be a frame with no id for the client to
	// match, and erroring on it would end a session over a message the
	// client was free to send.
	f.client.notify("notifications/something_new", map[string]any{"x": 1})

	var created CreateResult
	if rerr := f.client.call(MethodInteractionsCreate, f.createParams("do a thing"), &created); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	if created.Interaction.Status != StatusInProgress {
		t.Errorf("status = %q, want %q", created.Interaction.Status, StatusInProgress)
	}
	f.client.waitFor(NotifyInteractionCompleted)
}

// TestTurnStreamsGoogleSteps is the shape of one whole turn on the wire: the
// created frame, the sub-turn status update, a user_input step carrying what
// the model was given, a thought step, a model_output step streaming text,
// and the completed frame with usage on it.
func TestTurnStreamsGoogleSteps(t *testing.T) {
	f := newFixture(t, answer("All done."))
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodInteractionsCreate, f.createParams("say something"), &created); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyInteractionCompleted)

	if got := seen[0].Method; got != NotifyInteractionCreated {
		t.Fatalf("first notification = %q, want %q", got, NotifyInteractionCreated)
	}
	var first interactionEnvelope
	if err := json.Unmarshal(seen[0].Params, &first); err != nil {
		t.Fatalf("decode interaction.created: %v", err)
	}
	if first.Interaction.ID != created.Interaction.ID {
		t.Errorf("created frame names %q, create returned %q", first.Interaction.ID, created.Interaction.ID)
	}
	if first.EventType != NotifyInteractionCreated {
		t.Errorf("event_type = %q, want %q", first.EventType, NotifyInteractionCreated)
	}

	types := stepTypes(seen)
	want := []string{StepUserInput, StepThought, StepModelOutput}
	if len(types) != len(want) {
		t.Fatalf("step types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("step types = %v, want %v", types, want)
		}
	}

	// The user_input step echoes the client's own message id, so a parent
	// that optimistically rendered the message can match this to it.
	if got := steps(seen)[0].Step.Harness.MessageID; got != "msg-1" {
		t.Errorf("user_input message_id = %q, want msg-1", got)
	}
	// It also carries what the model was actually given, which includes the
	// task the client sent.
	if body := steps(seen)[0].Step.Content[0].Text; !strings.Contains(body, "say something") {
		t.Errorf("user_input step does not carry the task: %q", body)
	}

	// Every step opened is closed, exactly once, and no delta names a step
	// that was never started.
	assertStepLifecycle(t, seen)

	// The answer text arrives as text deltas on the model_output step.
	if got := textOf(seen, StepModelOutput); got != "All done." {
		t.Errorf("model_output text = %q, want %q", got, "All done.")
	}
	// The thought summary arrives as thought_summary deltas, never as text.
	if got := summaryOf(seen); got != "Thinking about it." {
		t.Errorf("thought summary = %q", got)
	}

	var done interactionEnvelope
	if err := json.Unmarshal(seen[len(seen)-1].Params, &done); err != nil {
		t.Fatalf("decode interaction.completed: %v", err)
	}
	if done.Interaction.Status != StatusCompleted {
		t.Errorf("final status = %q, want %q", done.Interaction.Status, StatusCompleted)
	}
	if done.Interaction.Usage == nil || done.Interaction.Usage.TotalTokens == 0 {
		t.Errorf("completed frame carries no usage: %+v", done.Interaction.Usage)
	}
	if done.Interaction.Harness == nil || done.Interaction.Harness.Reason == "" {
		t.Errorf("completed frame carries no harness.reason")
	}
}

// TestFunctionToolCallsBackOverThePipe pins the client-declared tool path:
// the model asks for a function the client declared, the step events name
// it, the client is asked to run it over harness.function_call, and its
// answer comes back as a function_result step.
func TestFunctionToolCallsBackOverThePipe(t *testing.T) {
	f := newFixture(t,
		callThen("call-1", "mcp__host__echo", `{"text":"ping"}`),
		answer("The tool said pong."),
	)
	f.client.handshake(ClientCapabilities{FunctionCalls: true})

	var asked []FunctionCallParams
	f.client.mu.Lock()
	f.client.onRequest = func(m message) (any, *rpcError) {
		if m.Method != MethodFunctionCall {
			return nil, errorf(CodeMethodNotFound, "no %s", m.Method)
		}
		var p FunctionCallParams
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, errorf(CodeInvalidParams, "%v", err)
		}
		f.client.mu.Lock()
		asked = append(asked, p)
		f.client.mu.Unlock()
		return FunctionCallResult{Result: TextContent("pong")}, nil
	}
	f.client.mu.Unlock()

	params := f.createParams("use the tool")
	params.Tools = []Tool{{
		Type: ToolFunction, Name: "echo", Description: "Echo the text back",
		Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		// A client tool reaches outside the working directory by
		// definition — this process has no idea what it does — so a
		// read-only session refuses it unless the client says it is safe,
		// the same rule an MCP server gets (docs/MCP.md, "Permissions").
		Harness: &ToolHarness{ReadOnly: true},
	}}
	if rerr := f.client.call(MethodInteractionsCreate, params, nil); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyInteractionCompleted)

	f.client.mu.Lock()
	got := append([]FunctionCallParams(nil), asked...)
	f.client.mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("client was asked to run %d function(s), want 1", len(got))
	}
	if got[0].Name != "echo" {
		t.Errorf("client was asked for %q, want the unqualified name %q", got[0].Name, "echo")
	}
	if got[0].ID != "call-1" {
		t.Errorf("function call id = %q, want the id from the function_call step", got[0].ID)
	}
	if string(got[0].Arguments) != `{"text":"ping"}` {
		t.Errorf("arguments = %s", got[0].Arguments)
	}

	callStep, ok := stepOfType(seen, StepFunctionCall)
	if !ok {
		t.Fatal("no function_call step reached the client")
	}
	if callStep.Step.Name != "mcp__host__echo" || callStep.Step.ID != "call-1" {
		t.Errorf("function_call step = %+v", callStep.Step)
	}
	resultStep, ok := stepOfType(seen, StepFunctionResult)
	if !ok {
		t.Fatal("no function_result step reached the client")
	}
	if resultStep.Step.CallID != "call-1" {
		t.Errorf("function_result call_id = %q, want call-1", resultStep.Step.CallID)
	}
	if resultStep.Step.IsError {
		t.Errorf("function_result is flagged as an error: %+v", resultStep.Step)
	}
	if txt := stepResultText(resultStep.Step); !strings.Contains(txt, "pong") {
		t.Errorf("function_result text = %q, want it to carry pong", txt)
	}
	assertStepLifecycle(t, seen)
}

// TestFunctionToolNeedsTheCapability pins the refusal: a client that never
// said it answers harness.function_call cannot declare function tools,
// because nothing would ever run them.
func TestFunctionToolNeedsTheCapability(t *testing.T) {
	f := newFixture(t, answer("hi"))
	f.client.handshake(ClientCapabilities{})

	params := f.createParams("use the tool")
	params.Tools = []Tool{{Type: ToolFunction, Name: "echo"}}
	rerr := f.client.call(MethodInteractionsCreate, params, nil)
	if rerr == nil {
		t.Fatal("a function tool was accepted from a client with no function_calls capability")
	}
	if rerr.Code != CodeInvalidParams {
		t.Errorf("code = %d, want %d", rerr.Code, CodeInvalidParams)
	}
}

// TestDeclinedFunctionCallBecomesAToolError pins that a client which
// declines the request does not stall or fail the run: the decline becomes an
// error tool result the model can react to. An unanswered request would hold
// the sub-turn open until the tool's own timeout, which is why the protocol
// requires a decline.
func TestDeclinedFunctionCallBecomesAToolError(t *testing.T) {
	f := newFixture(t,
		callThen("call-1", "mcp__host__echo", `{}`),
		answer("Never mind."),
	)
	f.client.handshake(ClientCapabilities{FunctionCalls: true})
	// onRequest stays nil, so the client declines with method-not-found.

	params := f.createParams("use the tool")
	params.Tools = []Tool{{Type: ToolFunction, Name: "echo", Harness: &ToolHarness{ReadOnly: true}}}
	if rerr := f.client.call(MethodInteractionsCreate, params, nil); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyInteractionCompleted)

	resultStep, ok := stepOfType(seen, StepFunctionResult)
	if !ok {
		t.Fatal("no function_result step for the declined call")
	}
	if !resultStep.Step.IsError {
		t.Errorf("a declined call produced a non-error result: %+v", resultStep.Step)
	}
	var done interactionEnvelope
	json.Unmarshal(seen[len(seen)-1].Params, &done)
	if done.Interaction.Status != StatusCompleted {
		t.Errorf("a declined call ended the run as %q, want it to carry on", done.Interaction.Status)
	}
}

// TestReadOnlyRefusesAnUndeclaredClientTool pins the permission posture for
// client tools: a read-only session refuses one the client did not mark
// read-only, and the refusal reaches the model as a function_result carrying
// the rule that made it.
func TestReadOnlyRefusesAnUndeclaredClientTool(t *testing.T) {
	f := newFixture(t,
		callThen("call-1", "mcp__host__echo", `{}`),
		answer("Fine, I will not."),
	)
	f.client.handshake(ClientCapabilities{FunctionCalls: true})
	f.client.mu.Lock()
	f.client.onRequest = func(m message) (any, *rpcError) {
		t.Errorf("the client was asked to run a tool a read-only session should have refused")
		return FunctionCallResult{}, nil
	}
	f.client.mu.Unlock()

	params := f.createParams("use the tool")
	params.Tools = []Tool{{Type: ToolFunction, Name: "echo"}}
	if rerr := f.client.call(MethodInteractionsCreate, params, nil); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyInteractionCompleted)

	resultStep, ok := stepOfType(seen, StepFunctionResult)
	if !ok {
		t.Fatal("no function_result step for the refused call")
	}
	if !resultStep.Step.IsError {
		t.Errorf("a refused call produced a non-error result: %+v", resultStep.Step)
	}
	if resultStep.Step.Harness == nil || resultStep.Step.Harness.Rule == "" {
		t.Errorf("a refused call carries no rule: %+v", resultStep.Step.Harness)
	}
}

// TestAppendSteersARunningInteraction pins steering: input added to an
// interaction already in flight reaches the model at the next sub-turn
// boundary and appears as a user_input step carrying the client's message id.
func TestAppendSteersARunningInteraction(t *testing.T) {
	// The first sub-turn calls a tool, which gives the steer somewhere to
	// land: it is applied at the boundary before the second sub-turn.
	f := newFixture(t,
		callThen("call-1", "TodoWrite", `{"todos":[]}`),
		answer("Steered."),
	)
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodInteractionsCreate, f.createParams("start"), &created); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	// Wait until the run is under way before steering, so the append lands
	// on a live interaction rather than racing its creation.
	f.client.waitFor(NotifyStepStart)

	input, _ := json.Marshal("actually, do this instead")
	var appended AppendResult
	rerr := f.client.call(MethodInteractionsAppend, AppendParams{
		InteractionID: created.Interaction.ID,
		Input:         input,
		Harness:       &AppendHarness{MessageID: "msg-2"},
	}, &appended)
	if rerr != nil {
		t.Fatalf("interactions.append: %v", rerr)
	}
	if appended.InteractionID != created.Interaction.ID || appended.Seq == 0 {
		t.Errorf("append result = %+v", appended)
	}

	seen := f.client.waitFor(NotifyInteractionCompleted)
	var steered *stepStart
	for _, s := range steps(seen) {
		s := s
		if s.Step.Type == StepUserInput && s.Step.Harness != nil && s.Step.Harness.Source == "append" {
			steered = &s
		}
	}
	if steered == nil {
		t.Fatalf("no appended user_input step; steps were %v", stepTypes(seen))
	}
	if steered.Step.Harness.MessageID != "msg-2" {
		t.Errorf("appended step message_id = %q, want msg-2", steered.Step.Harness.MessageID)
	}
	if got := steered.Step.Content[0].Text; !strings.Contains(got, "actually, do this instead") {
		t.Errorf("appended step text = %q", got)
	}
}

// TestAppendToAFinishedInteractionIsRefused pins that a client is told to
// start a new interaction rather than being allowed to steer something that
// has already stopped.
func TestAppendToAFinishedInteractionIsRefused(t *testing.T) {
	f := newFixture(t, answer("done"))
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	f.client.call(MethodInteractionsCreate, f.createParams("go"), &created)
	f.client.waitFor(NotifyInteractionCompleted)

	input, _ := json.Marshal("more")
	rerr := f.client.call(MethodInteractionsAppend, AppendParams{
		InteractionID: created.Interaction.ID, Input: input,
	}, nil)
	if rerr == nil {
		t.Fatal("append to a finished interaction was accepted")
	}
	if rerr.Code != CodeInteractionNotRunning {
		t.Errorf("code = %d, want %d", rerr.Code, CodeInteractionNotRunning)
	}
}

// TestCancelEndsTheInteraction pins interruption: cancel returns the
// interaction with a cancelled status, and the stream ends with a completed
// frame carrying it.
func TestCancelEndsTheInteraction(t *testing.T) {
	// A script that always asks for another tool call keeps the run going
	// until it is cancelled.
	f := newFixture(t, callThen("call-1", "TodoWrite", `{"todos":[]}`))
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodInteractionsCreate, f.createParams("loop"), &created); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	f.client.waitFor(NotifyStepStart)

	var cancelled GetResult
	if rerr := f.client.call(MethodInteractionsCancel, IDParams{InteractionID: created.Interaction.ID}, &cancelled); rerr != nil {
		t.Fatalf("interactions.cancel: %v", rerr)
	}
	if cancelled.Interaction.Status != StatusCancelled {
		t.Fatalf("status after cancel = %q, want %q", cancelled.Interaction.Status, StatusCancelled)
	}
	if cancelled.Interaction.Harness.Reason != "cancelled" {
		t.Errorf("harness.reason = %q, want cancelled", cancelled.Interaction.Harness.Reason)
	}

	seen := f.client.waitFor(NotifyInteractionCompleted)
	var done interactionEnvelope
	json.Unmarshal(seen[len(seen)-1].Params, &done)
	if done.Interaction.Status != StatusCancelled {
		t.Errorf("completed frame status = %q, want %q", done.Interaction.Status, StatusCancelled)
	}

	// Cancelling again is not an error: it is the state the caller asked
	// for, and a client racing a completion should not have to handle both
	// outcomes.
	if rerr := f.client.call(MethodInteractionsCancel, IDParams{InteractionID: created.Interaction.ID}, nil); rerr != nil {
		t.Errorf("second cancel: %v", rerr)
	}
}

// TestPreviousInteractionContinuesTheSession pins resume: a second
// interaction naming the first continues its session rather than starting a
// new one, and the second run's request carries the first's history.
func TestPreviousInteractionContinuesTheSession(t *testing.T) {
	f := newFixture(t, answer("first"), answer("second"))
	f.client.handshake(ClientCapabilities{})

	var one CreateResult
	if rerr := f.client.call(MethodInteractionsCreate, f.createParams("first task"), &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	f.client.waitFor(NotifyInteractionCompleted)

	next := CreateParams{
		Model:                 testModel,
		PreviousInteractionID: one.Interaction.ID,
	}
	next.Input, _ = json.Marshal("second task")
	var two CreateResult
	if rerr := f.client.call(MethodInteractionsCreate, next, &two); rerr != nil {
		t.Fatalf("second create: %v", rerr)
	}
	f.client.waitFor(NotifyInteractionCompleted)

	if two.Interaction.ID == one.Interaction.ID {
		t.Error("a continued interaction reused the previous interaction's id")
	}
	if two.Interaction.Harness.SessionID != one.Interaction.Harness.SessionID {
		t.Errorf("session ids differ: %q then %q", one.Interaction.Harness.SessionID, two.Interaction.Harness.SessionID)
	}

	// The second run's request replays the first run's conversation, which
	// is what continuing in place means.
	f.script.mu.Lock()
	last := f.script.requests[len(f.script.requests)-1]
	f.script.mu.Unlock()
	if !strings.Contains(string(last), "first task") {
		t.Error("the continued run's request does not carry the first turn's history")
	}
	if !strings.Contains(string(last), "second task") {
		t.Error("the continued run's request does not carry the new instruction")
	}
}

// TestGetReturnsTheAssembledInteraction pins that interactions.get answers
// with the same document a client would have built by following the stream.
func TestGetReturnsTheAssembledInteraction(t *testing.T) {
	f := newFixture(t, answer("assembled"))
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	f.client.call(MethodInteractionsCreate, f.createParams("go"), &created)
	f.client.waitFor(NotifyInteractionCompleted)

	var got GetResult
	if rerr := f.client.call(MethodInteractionsGet, IDParams{InteractionID: created.Interaction.ID}, &got); rerr != nil {
		t.Fatalf("interactions.get: %v", rerr)
	}
	if got.Interaction.Status != StatusCompleted {
		t.Errorf("status = %q", got.Interaction.Status)
	}
	var text string
	for _, s := range got.Interaction.Steps {
		if s.Type == StepModelOutput {
			for _, c := range s.Content {
				text += c.Text
			}
		}
	}
	if text != "assembled" {
		t.Errorf("assembled model_output = %q, want %q", text, "assembled")
	}
	if got.Interaction.Harness.Text != "assembled" {
		t.Errorf("harness.text = %q", got.Interaction.Harness.Text)
	}
}

// TestNonStreamingCreateWaits pins Google's stream:false shape: the answer is
// the whole finished interaction rather than the one that has just started.
func TestNonStreamingCreateWaits(t *testing.T) {
	f := newFixture(t, answer("unary"))
	f.client.handshake(ClientCapabilities{})

	params := f.createParams("go")
	no := false
	params.Stream = &no
	var res CreateResult
	if rerr := f.client.call(MethodInteractionsCreate, params, &res); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	if res.Interaction.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", res.Interaction.Status, StatusCompleted)
	}
	if len(res.Interaction.Steps) == 0 {
		t.Error("a non-streaming create returned no steps")
	}
}

// TestRefusals pins the things this surface says no to by name rather than
// ignoring, because a client that sent one and got a normal run would have no
// way to tell it had been dropped.
func TestRefusals(t *testing.T) {
	f := newFixture(t, answer("hi"))
	f.client.handshake(ClientCapabilities{})

	base := f.createParams("go")

	t.Run("agent", func(t *testing.T) {
		p := base
		p.Agent = "deep-research-pro-preview-12-2025"
		rerr := f.client.call(MethodInteractionsCreate, p, nil)
		if rerr == nil || rerr.Code != CodeUnsupported {
			t.Fatalf("agent was accepted or misreported: %v", rerr)
		}
	})
	t.Run("server-side tool", func(t *testing.T) {
		p := base
		p.Tools = []Tool{{Type: "google_search"}}
		rerr := f.client.call(MethodInteractionsCreate, p, nil)
		if rerr == nil || rerr.Code != CodeUnsupported {
			t.Fatalf("google_search was accepted or misreported: %v", rerr)
		}
	})
	t.Run("unknown model", func(t *testing.T) {
		p := base
		p.Model = "gpt-9"
		rerr := f.client.call(MethodInteractionsCreate, p, nil)
		if rerr == nil || rerr.Code != CodeInvalidParams {
			t.Fatalf("an unknown model was accepted or misreported: %v", rerr)
		}
	})
	t.Run("no cwd", func(t *testing.T) {
		p := base
		p.Harness = &CreateHarness{}
		rerr := f.client.call(MethodInteractionsCreate, p, nil)
		if rerr == nil || rerr.Code != CodeInvalidParams {
			t.Fatalf("a create with no cwd was accepted or misreported: %v", rerr)
		}
	})
	t.Run("bad permission mode", func(t *testing.T) {
		p := base
		p.Harness = &CreateHarness{CWD: f.cwd, PermissionMode: "yolo"}
		rerr := f.client.call(MethodInteractionsCreate, p, nil)
		if rerr == nil || rerr.Code != CodeInvalidParams {
			t.Fatalf("an unknown permission mode was accepted or misreported: %v", rerr)
		}
	})
	t.Run("unknown interaction", func(t *testing.T) {
		rerr := f.client.call(MethodInteractionsGet, IDParams{InteractionID: "int_nope"}, nil)
		if rerr == nil || rerr.Code != CodeInteractionNotFound {
			t.Fatalf("an unknown interaction was accepted or misreported: %v", rerr)
		}
	})
	t.Run("unknown method", func(t *testing.T) {
		rerr := f.client.call("thread/start", map[string]any{}, nil)
		if rerr == nil || rerr.Code != CodeMethodNotFound {
			t.Fatalf("an unknown method was accepted or misreported: %v", rerr)
		}
	})
}

// TestMissingCredentialsAreOneClearError pins that a process spawned without
// an API key says so at create, before any work happens, rather than failing
// on its first request to Google.
func TestMissingCredentialsAreOneClearError(t *testing.T) {
	f := newFixture(t, answer("hi"))
	f.srv.opts.HasAPIKey = func() bool { return false }
	f.client.handshake(ClientCapabilities{})

	rerr := f.client.call(MethodInteractionsCreate, f.createParams("go"), nil)
	if rerr == nil || rerr.Code != CodeCredentialsMissing {
		t.Fatalf("a create with no API key was accepted or misreported: %v", rerr)
	}
	if !strings.Contains(rerr.Message, "GEMINI_API_KEY") {
		t.Errorf("the error does not say where the key comes from: %q", rerr.Message)
	}
}

// TestStdinCloseEndsTheProcess pins the lifecycle: the parent closing stdin
// ends the session, and a run in flight is cancelled rather than left
// orphaned.
func TestStdinCloseEndsTheProcess(t *testing.T) {
	f := newFixture(t, callThen("call-1", "TodoWrite", `{"todos":[]}`))
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	f.client.call(MethodInteractionsCreate, f.createParams("loop"), &created)
	f.client.waitFor(NotifyStepStart)

	f.stdin.Close()
	// The fixture's cleanup asserts the server stopped; this asserts the
	// run it was hosting stopped with it.
	<-f.srvRunningDone()
	if got := f.srv.interactions[created.Interaction.ID].snapshot(false).Status; got != StatusCancelled {
		t.Errorf("status after stdin closed = %q, want %q", got, StatusCancelled)
	}
}

// TestPipelinedHandshakeIsOrdered pins that a client which sends initialize,
// initialized and a first create back to back without waiting for the answer
// gets them handled in that order. Handling the handshake concurrently let
// the acknowledgement be processed before the request it acknowledges, after
// which nothing was ever accepted.
func TestPipelinedHandshakeIsOrdered(t *testing.T) {
	f := newFixture(t, answer("pipelined"))

	f.client.send(mustFrame(t, "p1", MethodInitialize, InitializeParams{ClientInfo: ClientInfo{Name: "test"}}))
	f.client.notify(MethodInitialized, map[string]any{})
	var created CreateResult
	if rerr := f.client.call(MethodInteractionsCreate, f.createParams("go"), &created); rerr != nil {
		t.Fatalf("a create pipelined behind the handshake was refused: %v", rerr)
	}
	f.client.waitFor(NotifyInteractionCompleted)
}

// TestContinuedCreateCannotChangeTheSession pins the two things a continued
// create is refused for by name rather than ignored: a different model and a
// different working directory. A session's prompt prefix is frozen for its
// life, so neither is the caller's to choose again.
func TestContinuedCreateCannotChangeTheSession(t *testing.T) {
	f := newFixture(t, answer("first"))
	f.client.handshake(ClientCapabilities{})

	var one CreateResult
	if rerr := f.client.call(MethodInteractionsCreate, f.createParams("first task"), &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	f.client.waitFor(NotifyInteractionCompleted)

	next := CreateParams{PreviousInteractionID: one.Interaction.ID, Model: testModel}
	next.Input, _ = json.Marshal("more")

	t.Run("model", func(t *testing.T) {
		p := next
		p.Model = "deepseek-v4-pro"
		rerr := f.client.call(MethodInteractionsCreate, p, nil)
		if rerr == nil || rerr.Code != CodeInvalidParams {
			t.Fatalf("a continued create changed model: %v", rerr)
		}
	})
	t.Run("cwd", func(t *testing.T) {
		p := next
		p.Harness = &CreateHarness{CWD: t.TempDir()}
		rerr := f.client.call(MethodInteractionsCreate, p, nil)
		if rerr == nil || rerr.Code != CodeInvalidParams {
			t.Fatalf("a continued create changed directory: %v", rerr)
		}
	})
}

// TestGetWorksWhileTheRunIsGoing pins that interactions.get on an
// in-progress interaction answers with what has happened so far rather than
// with an empty document. A parent recovering from a dropped notification
// needs that during the run, not after it.
func TestGetWorksWhileTheRunIsGoing(t *testing.T) {
	f := newFixture(t, callThen("call-1", "TodoWrite", `{"todos":[]}`))
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	f.client.call(MethodInteractionsCreate, f.createParams("loop"), &created)
	f.client.waitFor(NotifyStepStart)

	var got GetResult
	if rerr := f.client.call(MethodInteractionsGet, IDParams{InteractionID: created.Interaction.ID}, &got); rerr != nil {
		t.Fatalf("interactions.get: %v", rerr)
	}
	if got.Interaction.Status != StatusInProgress {
		t.Errorf("status = %q, want %q", got.Interaction.Status, StatusInProgress)
	}
	if len(got.Interaction.Steps) == 0 {
		t.Error("a running interaction reported no steps")
	}
}
