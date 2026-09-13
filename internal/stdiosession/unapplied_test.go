package stdiosession

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests pin what happens to a steer the run never reaches
// (docs/STDIO-PROTOCOL.md, "responses.append"). The loop applies a steer
// only at the top of a sub-turn, so one committed after the last boundary
// would otherwise sit in the log and be applied by whichever run came next.
// Each test parks a provider request (scriptedGemini.hold) so the append
// lands at a known point in the run instead of racing a provider that
// answers at once.

// appendSteer sends one responses.append and fails the test if it is refused.
func (f *fixture) appendSteer(runID, text, messageID string) AppendResult {
	f.t.Helper()
	input, _ := json.Marshal(text)
	params := AppendParams{ResponseID: runID, Input: input}
	if messageID != "" {
		params.Harness = &AppendHarness{MessageID: messageID}
	}
	var out AppendResult
	if rerr := f.client.call(MethodResponsesAppend, params, &out); rerr != nil {
		f.t.Fatalf("append %q: %v", text, rerr)
	}
	return out
}

// completedResponse decodes the response.completed frame that ends seen.
func completedResponse(t *testing.T, seen []message) Response {
	t.Helper()
	var done responseEnvelope
	if err := json.Unmarshal(seen[len(seen)-1].Params, &done); err != nil {
		t.Fatalf("decode response.completed: %v", err)
	}
	if done.Response.Harness == nil {
		t.Fatal("response.completed carries no harness block")
	}
	return done.Response
}

func appendedItems(seen []message) []itemEvent {
	var out []itemEvent
	for _, s := range steps(seen) {
		if s.Item.Type == ItemMessage && s.Item.Harness != nil && s.Item.Harness.Source == "append" {
			out = append(out, s)
		}
	}
	return out
}

func sameIDs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestSteerAfterTheLastBoundaryIsWithdrawn is the case a client meets most:
// the model is answering with no tool calls, so the run has no boundary left,
// and a steer arrives. The steer comes back in harness.unapplied_message_ids
// on response.completed and on responses.get; one appended with no
// message_id is withdrawn too but names nothing; a later append is refused
// -32002; and the next response in the chain does not apply either steer,
// so its request carries only its own input. It fails if the sweep stops
// running, if the withdrawal stops moving the applied mark, or if the field
// is renamed.
func TestSteerAfterTheLastBoundaryIsWithdrawn(t *testing.T) {
	f := newFixture(t, answer("first"), answer("second"))
	held := f.script.hold(t, 0)
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("first task"), &created); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	held.waitArrived()
	f.appendSteer(created.Response.ID, "a late steer", "msg-late")
	f.appendSteer(created.Response.ID, "an anonymous steer", "")
	held.let()

	seen := f.client.waitFor(NotifyResponseCompleted)
	done := completedResponse(t, seen)
	if done.Harness.Reason != "no_tool_calls" {
		t.Fatalf("harness.reason = %q, want no_tool_calls", done.Harness.Reason)
	}
	if want := []string{"msg-late"}; !sameIDs(done.Harness.UnappliedMessageIDs, want) {
		t.Errorf("response.completed unapplied_message_ids = %v, want %v", done.Harness.UnappliedMessageIDs, want)
	}
	if got := appendedItems(seen); len(got) != 0 {
		t.Errorf("a withdrawn steer was echoed as applied: %+v", got)
	}

	var got GetResult
	if rerr := f.client.call(MethodResponsesGet, IDParams{ResponseID: created.Response.ID}, &got); rerr != nil {
		t.Fatalf("get: %v", rerr)
	}
	if want := []string{"msg-late"}; !sameIDs(got.Response.Harness.UnappliedMessageIDs, want) {
		t.Errorf("responses.get unapplied_message_ids = %v, want %v", got.Response.Harness.UnappliedMessageIDs, want)
	}

	input, _ := json.Marshal("too late")
	rerr := f.client.call(MethodResponsesAppend, AppendParams{ResponseID: created.Response.ID, Input: input}, nil)
	if rerr == nil || rerr.Code != CodeRunNotRunning {
		t.Fatalf("append after the run ended = %v, want code %d", rerr, CodeRunNotRunning)
	}

	next := CreateParams{Model: testModel, PreviousResponseID: created.Response.ID}
	next.Input, _ = json.Marshal("second task")
	var two CreateResult
	if rerr := f.client.call(MethodResponsesCreate, next, &two); rerr != nil {
		t.Fatalf("continued create: %v", rerr)
	}
	seen = f.client.waitFor(NotifyResponseCompleted)
	if ids := completedResponse(t, seen).Harness.UnappliedMessageIDs; len(ids) != 0 {
		t.Errorf("the next response reports unapplied steers %v", ids)
	}
	if got := appendedItems(seen); len(got) != 0 {
		t.Errorf("the next response applied a withdrawn steer: %+v", got)
	}

	f.script.mu.Lock()
	last := string(f.script.requests[len(f.script.requests)-1])
	f.script.mu.Unlock()
	if !strings.Contains(last, "second task") {
		t.Error("the continued run's request does not carry its own input")
	}
	for _, steer := range []string{"a late steer", "an anonymous steer"} {
		if strings.Contains(last, steer) {
			t.Errorf("the continued run's request carries the withdrawn steer %q", steer)
		}
	}
}

// TestAppliedSteerIsNotListedAsUnapplied pins the other side: a steer that
// lands while a tool-calling sub-turn is in flight is applied at the next
// boundary, echoed with its message_id, and left out of
// unapplied_message_ids, while one that lands during the final sub-turn is
// listed. It fails if the sweep withdraws applied steers, or if the pump
// translates a steer_applied before the append has recorded its id.
func TestAppliedSteerIsNotListedAsUnapplied(t *testing.T) {
	f := newFixture(t,
		callThen("call-1", "TodoWrite", `{"todos":[]}`),
		answer("Steered."),
	)
	first := f.script.hold(t, 0)
	second := f.script.hold(t, 1)
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("start"), &created); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	first.waitArrived()
	f.appendSteer(created.Response.ID, "use the other approach", "msg-applied")
	first.let()

	second.waitArrived()
	f.appendSteer(created.Response.ID, "and one more thing", "msg-late")
	second.let()

	seen := f.client.waitFor(NotifyResponseCompleted)
	applied := appendedItems(seen)
	if len(applied) != 1 {
		t.Fatalf("applied steer items = %d, want 1", len(applied))
	}
	if applied[0].Item.Harness.MessageID != "msg-applied" {
		t.Errorf("applied steer message_id = %q, want msg-applied", applied[0].Item.Harness.MessageID)
	}
	if want := []string{"msg-late"}; !sameIDs(completedResponse(t, seen).Harness.UnappliedMessageIDs, want) {
		t.Errorf("unapplied_message_ids = %v, want %v", completedResponse(t, seen).Harness.UnappliedMessageIDs, want)
	}
}

// TestCancelledRunListsItsUnappliedSteers pins the cancel path. The steer
// lands while a request is in flight, the cancel ends the run before any
// boundary could apply it, and both the cancel's answer and the terminal
// frame list it. The response continued from the cancelled one does not
// apply it, which is the store writing the withdrawal to a session the
// cancel already marked stopped.
func TestCancelledRunListsItsUnappliedSteers(t *testing.T) {
	f := newFixture(t, callThen("call-1", "TodoWrite", `{"todos":[]}`), answer("after"))
	held := f.script.hold(t, 0)
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("loop"), &created); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	held.waitArrived()
	f.appendSteer(created.Response.ID, "stop and do this", "msg-c")

	var cancelled GetResult
	if rerr := f.client.call(MethodResponsesCancel, IDParams{ResponseID: created.Response.ID}, &cancelled); rerr != nil {
		t.Fatalf("cancel: %v", rerr)
	}
	if cancelled.Response.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", cancelled.Response.Status)
	}
	if want := []string{"msg-c"}; !sameIDs(cancelled.Response.Harness.UnappliedMessageIDs, want) {
		t.Errorf("cancel answer unapplied_message_ids = %v, want %v", cancelled.Response.Harness.UnappliedMessageIDs, want)
	}
	seen := f.client.waitFor(NotifyResponseCompleted)
	if want := []string{"msg-c"}; !sameIDs(completedResponse(t, seen).Harness.UnappliedMessageIDs, want) {
		t.Errorf("response.completed unapplied_message_ids = %v, want %v", completedResponse(t, seen).Harness.UnappliedMessageIDs, want)
	}

	next := CreateParams{Model: testModel, PreviousResponseID: created.Response.ID}
	next.Input, _ = json.Marshal("carry on")
	if rerr := f.client.call(MethodResponsesCreate, next, nil); rerr != nil {
		t.Fatalf("continued create: %v", rerr)
	}
	seen = f.client.waitFor(NotifyResponseCompleted)
	if got := appendedItems(seen); len(got) != 0 {
		t.Errorf("the response after the cancel applied the withdrawn steer: %+v", got)
	}
	f.script.mu.Lock()
	last := string(f.script.requests[len(f.script.requests)-1])
	f.script.mu.Unlock()
	if strings.Contains(last, "stop and do this") {
		t.Error("the continued run's request carries the withdrawn steer")
	}
}

// TestInteractionsListsUnappliedSteers pins the same field on the
// Interactions dialect's harness block, on interaction.completed and on
// interactions.get.
func TestInteractionsListsUnappliedSteers(t *testing.T) {
	useDialect(t, NewInteractions())
	f := newFixture(t, answer("done"))
	held := f.script.hold(t, 0)
	f.client.handshake(ClientCapabilities{})

	var created iactCreateResult
	if rerr := f.client.call(MethodInteractionsCreate, f.createParams("go"), &created); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	held.waitArrived()
	input, _ := json.Marshal("late")
	if rerr := f.client.call(MethodInteractionsAppend, iactAppendParams{
		InteractionID: created.Interaction.ID, Input: input,
		Harness: &AppendHarness{MessageID: "msg-i"},
	}, nil); rerr != nil {
		t.Fatalf("append: %v", rerr)
	}
	held.let()

	seen := f.client.waitFor(notifyIactCompleted)
	var done iactEnvelope
	if err := json.Unmarshal(seen[len(seen)-1].Params, &done); err != nil {
		t.Fatalf("decode interaction.completed: %v", err)
	}
	if want := []string{"msg-i"}; done.Interaction.Harness == nil || !sameIDs(done.Interaction.Harness.UnappliedMessageIDs, want) {
		t.Errorf("interaction.completed harness = %+v, want unapplied_message_ids %v", done.Interaction.Harness, want)
	}

	var got iactCreateResult
	if rerr := f.client.call(MethodInteractionsGet, iactIDParams{InteractionID: created.Interaction.ID}, &got); rerr != nil {
		t.Fatalf("get: %v", rerr)
	}
	if want := []string{"msg-i"}; got.Interaction.Harness == nil || !sameIDs(got.Interaction.Harness.UnappliedMessageIDs, want) {
		t.Errorf("interactions.get harness = %+v, want unapplied_message_ids %v", got.Interaction.Harness, want)
	}
}

// TestNoAcceptedSteerIsLostAsTheRunEnds pins the race between an append and
// the run ending. A client appends as fast as it can while the run's last
// request is released; every append the server accepted must be listed in
// unapplied_message_ids, since the run has no boundary left to apply it at,
// and every one after that must be refused -32002. It fails if an append can
// commit after the withdrawal sweep has looked, which is what holding the
// status check and the commit under one lock prevents. Run it under -race
// for the strongest check.
func TestNoAcceptedSteerIsLostAsTheRunEnds(t *testing.T) {
	f := newFixture(t, answer("done"))
	held := f.script.hold(t, 0)
	f.client.handshake(ClientCapabilities{})

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("go"), &created); rerr != nil {
		t.Fatalf("create: %v", rerr)
	}
	held.waitArrived()

	accepted := make(chan []string, 1)
	go func() {
		var ids []string
		for n := 0; ; n++ {
			id := "msg-" + itoa(n)
			input, _ := json.Marshal("steer " + id)
			rerr := f.client.call(MethodResponsesAppend, AppendParams{
				ResponseID: created.Response.ID, Input: input,
				Harness: &AppendHarness{MessageID: id},
			}, nil)
			if rerr != nil {
				if rerr.Code != CodeRunNotRunning {
					t.Errorf("append %s refused with %d, want %d", id, rerr.Code, CodeRunNotRunning)
				}
				accepted <- ids
				return
			}
			ids = append(ids, id)
			if n == 3 {
				held.let()
			}
		}
	}()

	ids := <-accepted
	seen := f.client.waitFor(NotifyResponseCompleted)
	if got := completedResponse(t, seen).Harness.UnappliedMessageIDs; !sameIDs(got, ids) {
		t.Errorf("unapplied_message_ids = %v, want every accepted append %v", got, ids)
	}
}
