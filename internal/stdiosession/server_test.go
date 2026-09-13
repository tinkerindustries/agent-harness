package stdiosession

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

	rerr := f.client.call(MethodResponsesCreate, f.createParams("do a thing"), nil)
	if rerr == nil {
		t.Fatal("interactions.create was accepted before the handshake")
	}
	if rerr.Code != CodeNotInitialized {
		t.Errorf("code = %d, want %d", rerr.Code, CodeNotInitialized)
	}

	res := f.client.handshake(ClientCapabilities{})
	if res.ServerInfo.Protocol != "openai.responses.v1" {
		t.Errorf("protocol = %q", res.ServerInfo.Protocol)
	}
	if res.DefaultModel != testModel {
		t.Errorf("default model = %q, want %q", res.DefaultModel, testModel)
	}
	if !res.Capabilities.Append || !res.Capabilities.Cancel || !res.Capabilities.PreviousResponse {
		t.Errorf("capabilities = %+v", res.Capabilities)
	}

	// A notification the server has never heard of must be dropped in
	// silence: answering it would be a frame with no id for the client to
	// match, and erroring on it would end a session over a message the
	// client was free to send.
	f.client.notify("notifications/something_new", map[string]any{"x": 1})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("do a thing"), &created); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	if created.Response.Status != StatusInProgress {
		t.Errorf("status = %q, want %q", created.Response.Status, StatusInProgress)
	}
	f.client.waitFor(NotifyResponseCompleted)
}

// TestTurnStreamsGoogleSteps is the shape of one whole turn on the wire: the
// created frame, the sub-turn status update, a user_input step carrying what
// the model was given, a thought step, a model_output step streaming text,
// and the completed frame with usage on it.
func TestTurnStreamsGoogleSteps(t *testing.T) {
	f := newFixture(t, answer("All done."))
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("say something"), &created); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)

	if got := seen[0].Method; got != NotifyResponseCreated {
		t.Fatalf("first notification = %q, want %q", got, NotifyResponseCreated)
	}
	var first responseEnvelope
	if err := json.Unmarshal(seen[0].Params, &first); err != nil {
		t.Fatalf("decode interaction.created: %v", err)
	}
	if first.Response.ID != created.Response.ID {
		t.Errorf("created frame names %q, create returned %q", first.Response.ID, created.Response.ID)
	}
	if first.Type != NotifyResponseCreated {
		t.Errorf("type = %q, want %q", first.Type, NotifyResponseCreated)
	}

	types := stepTypes(seen)
	want := []string{ItemMessage, ItemReasoning, ItemMessage}
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
	if got := steps(seen)[0].Item.Harness.MessageID; got != "msg-1" {
		t.Errorf("user_input message_id = %q, want msg-1", got)
	}
	// It also carries what the model was actually given, which includes the
	// task the client sent.
	if body := steps(seen)[0].Item.Content[0].Text; !strings.Contains(body, "say something") {
		t.Errorf("user_input step does not carry the task: %q", body)
	}

	// Every step opened is closed, exactly once, and no delta names a step
	// that was never started.
	assertStepLifecycle(t, seen)

	// The answer text arrives as text deltas on the model_output step.
	if got := textOf(seen, ItemMessage); got != "All done." {
		t.Errorf("model_output text = %q, want %q", got, "All done.")
	}
	// The thought summary arrives as thought_summary deltas, never as text.
	if got := summaryOf(seen); got != "Thinking about it." {
		t.Errorf("thought summary = %q", got)
	}

	var done responseEnvelope
	if err := json.Unmarshal(seen[len(seen)-1].Params, &done); err != nil {
		t.Fatalf("decode interaction.completed: %v", err)
	}
	if done.Response.Status != StatusCompleted {
		t.Errorf("final status = %q, want %q", done.Response.Status, StatusCompleted)
	}
	if done.Response.Usage == nil || done.Response.Usage.TotalTokens == 0 {
		t.Errorf("completed frame carries no usage: %+v", done.Response.Usage)
	}
	if done.Response.Harness == nil || done.Response.Harness.Reason == "" {
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
		return FunctionCallResult{Output: TextPart(PartInputText, "pong")}, nil
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
	if rerr := f.client.call(MethodResponsesCreate, params, nil); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)

	f.client.mu.Lock()
	got := append([]FunctionCallParams(nil), asked...)
	f.client.mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("client was asked to run %d function(s), want 1", len(got))
	}
	if got[0].Name != "echo" {
		t.Errorf("client was asked for %q, want the unqualified name %q", got[0].Name, "echo")
	}
	if got[0].CallID != "call-1" {
		t.Errorf("function call id = %q, want the id from the function_call step", got[0].CallID)
	}
	if string(got[0].Arguments) != `{"text":"ping"}` {
		t.Errorf("arguments = %s", got[0].Arguments)
	}

	callStep, ok := stepOfType(seen, ItemFunctionCall)
	if !ok {
		t.Fatal("no function_call step reached the client")
	}
	if callStep.Item.Name != "mcp__host__echo" || callStep.Item.CallID != "call-1" {
		t.Errorf("function_call step = %+v", callStep.Item)
	}
	resultStep, ok := stepOfType(seen, ItemFunctionCallOutput)
	if !ok {
		t.Fatal("no function_result step reached the client")
	}
	if resultStep.Item.CallID != "call-1" {
		t.Errorf("function_result call_id = %q, want call-1", resultStep.Item.CallID)
	}
	if resultStep.Item.Harness.IsError {
		t.Errorf("function_result is flagged as an error: %+v", resultStep.Item)
	}
	if txt := stepResultText(resultStep.Item); !strings.Contains(txt, "pong") {
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
	rerr := f.client.call(MethodResponsesCreate, params, nil)
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
	if rerr := f.client.call(MethodResponsesCreate, params, nil); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)

	resultStep, ok := stepOfType(seen, ItemFunctionCallOutput)
	if !ok {
		t.Fatal("no function_result step for the declined call")
	}
	if !resultStep.Item.Harness.IsError {
		t.Errorf("a declined call produced a non-error result: %+v", resultStep.Item)
	}
	var done responseEnvelope
	json.Unmarshal(seen[len(seen)-1].Params, &done)
	if done.Response.Status != StatusCompleted {
		t.Errorf("a declined call ended the run as %q, want it to carry on", done.Response.Status)
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
	if rerr := f.client.call(MethodResponsesCreate, params, nil); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)

	resultStep, ok := stepOfType(seen, ItemFunctionCallOutput)
	if !ok {
		t.Fatal("no function_result step for the refused call")
	}
	if !resultStep.Item.Harness.IsError {
		t.Errorf("a refused call produced a non-error result: %+v", resultStep.Item)
	}
	if resultStep.Item.Harness == nil || resultStep.Item.Harness.Rule == "" {
		t.Errorf("a refused call carries no rule: %+v", resultStep.Item.Harness)
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
	// The first request is held until the append is in, so the steer lands
	// before the boundary that applies it. Released early, the run could
	// reach its last sub-turn first and withdraw the steer instead.
	held := f.script.hold(t, 0)
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("start"), &created); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	held.waitArrived()

	input, _ := json.Marshal("actually, do this instead")
	var appended AppendResult
	rerr := f.client.call(MethodResponsesAppend, AppendParams{
		ResponseID: created.Response.ID,
		Input:      input,
		Harness:    &AppendHarness{MessageID: "msg-2"},
	}, &appended)
	if rerr != nil {
		t.Fatalf("interactions.append: %v", rerr)
	}
	if appended.ResponseID != created.Response.ID || appended.Seq == 0 {
		t.Errorf("append result = %+v", appended)
	}
	held.let()

	seen := f.client.waitFor(NotifyResponseCompleted)
	var steered *itemEvent
	for _, s := range steps(seen) {
		s := s
		if s.Item.Type == ItemMessage && s.Item.Harness != nil && s.Item.Harness.Source == "append" {
			steered = &s
		}
	}
	if steered == nil {
		t.Fatalf("no appended user_input step; steps were %v", stepTypes(seen))
	}
	if steered.Item.Harness.MessageID != "msg-2" {
		t.Errorf("appended step message_id = %q, want msg-2", steered.Item.Harness.MessageID)
	}
	if got := steered.Item.Content[0].Text; !strings.Contains(got, "actually, do this instead") {
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
	f.client.call(MethodResponsesCreate, f.createParams("go"), &created)
	f.client.waitFor(NotifyResponseCompleted)

	input, _ := json.Marshal("more")
	rerr := f.client.call(MethodResponsesAppend, AppendParams{
		ResponseID: created.Response.ID, Input: input,
	}, nil)
	if rerr == nil {
		t.Fatal("append to a finished interaction was accepted")
	}
	if rerr.Code != CodeRunNotRunning {
		t.Errorf("code = %d, want %d", rerr.Code, CodeRunNotRunning)
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
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("loop"), &created); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	f.client.waitFor(NotifyOutputItemAdded)

	var cancelled GetResult
	if rerr := f.client.call(MethodResponsesCancel, IDParams{ResponseID: created.Response.ID}, &cancelled); rerr != nil {
		t.Fatalf("interactions.cancel: %v", rerr)
	}
	if cancelled.Response.Status != StatusCancelled {
		t.Fatalf("status after cancel = %q, want %q", cancelled.Response.Status, StatusCancelled)
	}
	if cancelled.Response.Harness.Reason != "cancelled" {
		t.Errorf("harness.reason = %q, want cancelled", cancelled.Response.Harness.Reason)
	}

	seen := f.client.waitFor(NotifyResponseCompleted)
	var done responseEnvelope
	json.Unmarshal(seen[len(seen)-1].Params, &done)
	if done.Response.Status != StatusCancelled {
		t.Errorf("completed frame status = %q, want %q", done.Response.Status, StatusCancelled)
	}

	// Cancelling again is not an error: it is the state the caller asked
	// for, and a client racing a completion should not have to handle both
	// outcomes.
	if rerr := f.client.call(MethodResponsesCancel, IDParams{ResponseID: created.Response.ID}, nil); rerr != nil {
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
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("first task"), &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)

	next := CreateParams{
		Model:              testModel,
		PreviousResponseID: one.Response.ID,
	}
	next.Input, _ = json.Marshal("second task")
	var two CreateResult
	if rerr := f.client.call(MethodResponsesCreate, next, &two); rerr != nil {
		t.Fatalf("second create: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)

	if two.Response.ID == one.Response.ID {
		t.Error("a continued interaction reused the previous interaction's id")
	}
	if two.Response.Harness.SessionID != one.Response.Harness.SessionID {
		t.Errorf("session ids differ: %q then %q", one.Response.Harness.SessionID, two.Response.Harness.SessionID)
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
	f.client.call(MethodResponsesCreate, f.createParams("go"), &created)
	f.client.waitFor(NotifyResponseCompleted)

	var got GetResult
	if rerr := f.client.call(MethodResponsesGet, IDParams{ResponseID: created.Response.ID}, &got); rerr != nil {
		t.Fatalf("responses.get: %v", rerr)
	}
	if got.Response.Status != StatusCompleted {
		t.Errorf("status = %q", got.Response.Status)
	}
	// Only the assistant's own message: a response's output here also
	// carries the user message items the loop folded in, because a response
	// spans a whole agentic run (docs/STDIO-PROTOCOL.md, "Deviations").
	var text string
	for _, s := range got.Response.Output {
		if s.Type == ItemMessage && s.Role == "assistant" {
			for _, c := range s.Content {
				text += c.Text
			}
		}
	}
	if text != "assembled" {
		t.Errorf("assembled assistant message = %q, want %q", text, "assembled")
	}
	if got.Response.Harness.Text != "assembled" {
		t.Errorf("harness.text = %q", got.Response.Harness.Text)
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
	if rerr := f.client.call(MethodResponsesCreate, params, &res); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	if res.Response.Status != StatusCompleted {
		t.Errorf("status = %q, want %q", res.Response.Status, StatusCompleted)
	}
	if len(res.Response.Output) == 0 {
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

	t.Run("background", func(t *testing.T) {
		p := base
		yes := true
		p.Background = &yes
		rerr := f.client.call(MethodResponsesCreate, p, nil)
		if rerr == nil || rerr.Code != CodeUnsupported {
			t.Fatalf("background was accepted or misreported: %v", rerr)
		}
	})
	t.Run("server-side tool", func(t *testing.T) {
		p := base
		p.Tools = []Tool{{Type: "web_search"}}
		rerr := f.client.call(MethodResponsesCreate, p, nil)
		if rerr == nil || rerr.Code != CodeUnsupported {
			t.Fatalf("web_search was accepted or misreported: %v", rerr)
		}
	})
	t.Run("unknown model", func(t *testing.T) {
		p := base
		p.Model = "gpt-9"
		rerr := f.client.call(MethodResponsesCreate, p, nil)
		if rerr == nil || rerr.Code != CodeInvalidParams {
			t.Fatalf("an unknown model was accepted or misreported: %v", rerr)
		}
	})
	t.Run("no cwd", func(t *testing.T) {
		p := base
		p.Harness = &CreateHarness{}
		rerr := f.client.call(MethodResponsesCreate, p, nil)
		if rerr == nil || rerr.Code != CodeInvalidParams {
			t.Fatalf("a create with no cwd was accepted or misreported: %v", rerr)
		}
	})
	t.Run("bad permission mode", func(t *testing.T) {
		p := base
		p.Harness = &CreateHarness{CWD: f.cwd, PermissionMode: "yolo"}
		rerr := f.client.call(MethodResponsesCreate, p, nil)
		if rerr == nil || rerr.Code != CodeInvalidParams {
			t.Fatalf("an unknown permission mode was accepted or misreported: %v", rerr)
		}
	})
	t.Run("unknown interaction", func(t *testing.T) {
		rerr := f.client.call(MethodResponsesGet, IDParams{ResponseID: "int_nope"}, nil)
		if rerr == nil || rerr.Code != CodeRunNotFound {
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
	f.srv.opts.HasAPIKey = func(string) bool { return false }
	f.client.handshake(ClientCapabilities{})

	rerr := f.client.call(MethodResponsesCreate, f.createParams("go"), nil)
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
	f.client.call(MethodResponsesCreate, f.createParams("loop"), &created)
	f.client.waitFor(NotifyOutputItemAdded)

	f.stdin.Close()
	// The fixture's cleanup asserts the server stopped; this asserts the
	// run it was hosting stopped with it.
	<-f.srvRunningDone()
	if got := f.srv.runs[created.Response.ID].view(false).Status; got != StatusCancelled {
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
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("go"), &created); rerr != nil {
		t.Fatalf("a create pipelined behind the handshake was refused: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)
}

// TestContinuedCreateCannotChangeTheSession pins the two things a continued
// create is refused for by name rather than ignored: a different model and a
// different working directory. A session's prompt prefix is frozen for its
// life, so neither is the caller's to choose again.
func TestContinuedCreateCannotChangeTheSession(t *testing.T) {
	f := newFixture(t, answer("first"))
	f.client.handshake(ClientCapabilities{})

	var one CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("first task"), &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)

	next := CreateParams{PreviousResponseID: one.Response.ID, Model: testModel}
	next.Input, _ = json.Marshal("more")

	t.Run("model", func(t *testing.T) {
		p := next
		p.Model = "deepseek-v4-pro"
		rerr := f.client.call(MethodResponsesCreate, p, nil)
		if rerr == nil || rerr.Code != CodeInvalidParams {
			t.Fatalf("a continued create changed model: %v", rerr)
		}
	})
	t.Run("cwd", func(t *testing.T) {
		p := next
		p.Harness = &CreateHarness{CWD: t.TempDir()}
		rerr := f.client.call(MethodResponsesCreate, p, nil)
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
	f.client.call(MethodResponsesCreate, f.createParams("loop"), &created)
	f.client.waitFor(NotifyOutputItemAdded)

	var got GetResult
	if rerr := f.client.call(MethodResponsesGet, IDParams{ResponseID: created.Response.ID}, &got); rerr != nil {
		t.Fatalf("responses.get: %v", rerr)
	}
	if got.Response.Status != StatusInProgress {
		t.Errorf("status = %q, want %q", got.Response.Status, StatusInProgress)
	}
	if len(got.Response.Output) == 0 {
		t.Error("a running interaction reported no steps")
	}
}

// TestSequenceNumbersRunUnbrokenToTheTerminalFrame pins that every frame of a
// stream, the last one included, carries the next sequence number.
//
// The terminal frames used to be built in server.go rather than by the
// translator, so they went out with no number at all: a client that had
// counted a stream up to N was handed a response.completed claiming to be
// frame 0. The two failure paths disagreed with each other for the same
// reason — the one the translator emits for a loop error numbered itself and
// the one record emitted did not.
func TestSequenceNumbersRunUnbrokenToTheTerminalFrame(t *testing.T) {
	f := newFixture(t, answer("All done."))
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("say something"), &created); rerr != nil {
		t.Fatalf("responses.create: %v", rerr)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)

	for i, m := range seen {
		var frame struct {
			SequenceNumber *int `json:"sequence_number"`
		}
		if err := json.Unmarshal(m.Params, &frame); err != nil {
			t.Fatalf("decode %s: %v", m.Method, err)
		}
		if frame.SequenceNumber == nil {
			t.Fatalf("%s carries no sequence_number", m.Method)
		}
		if *frame.SequenceNumber != i {
			t.Errorf("%s (frame %d) has sequence_number %d", m.Method, i, *frame.SequenceNumber)
		}
	}
}
