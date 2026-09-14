package stdiosession

import (
	"encoding/json"
	"strings"
	"testing"
)

// The Interactions dialect on the wire. These drive the same fixture and the
// same scripted Gemini server the Responses tests do; what they assert is
// that a client reads Google's vocabulary out of it.

// iactSteps pulls every step.start payload out of a run of notifications.
func iactSteps(ms []message) []iactStepStart {
	var out []iactStepStart
	for _, m := range ms {
		if m.Method != notifyIactStepStart {
			continue
		}
		var s iactStepStart
		if err := json.Unmarshal(m.Params, &s); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// iactDeltas pulls every step.delta payload of the named delta type.
func iactDeltas(ms []message, kind string) []iactStepDelta {
	var out []iactStepDelta
	for _, m := range ms {
		if m.Method != notifyIactStepDelta {
			continue
		}
		var d iactStepDelta
		if err := json.Unmarshal(m.Params, &d); err == nil && d.Delta.Type == kind {
			out = append(out, d)
		}
	}
	return out
}

// TestInteractionsStreamsGoogleSteps is one whole turn in Google's
// vocabulary: interaction.created, a user_input step carrying what the model
// was given, a thought step, a model_output step streaming text, and
// interaction.completed.
func TestInteractionsStreamsGoogleSteps(t *testing.T) {
	useDialect(t, NewInteractions())
	f := newFixture(t, answer("All done."))
	f.client.handshake(ClientCapabilities{})

	var created iactCreateResult
	if rerr := f.client.call(MethodInteractionsCreate, f.createParams("say something"), &created); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	if !strings.HasPrefix(created.Interaction.ID, "int_") {
		t.Errorf("interaction id = %q, want an int_ prefix", created.Interaction.ID)
	}
	if created.Interaction.Object != "interaction" {
		t.Errorf("object = %q, want %q", created.Interaction.Object, "interaction")
	}

	seen := f.client.waitFor(notifyIactCompleted)
	if got := seen[0].Method; got != notifyIactCreated {
		t.Fatalf("first notification = %q, want %q", got, notifyIactCreated)
	}

	var types []string
	for _, s := range iactSteps(seen) {
		types = append(types, s.Step.Type)
	}
	want := []string{iactStepUserInput, iactStepThought, iactStepModelOutput}
	if len(types) != len(want) {
		t.Fatalf("step types = %v, want %v", types, want)
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("step types = %v, want %v", types, want)
		}
	}

	// The user_input step carries what the model was actually given, and
	// says where it came from.
	first := iactSteps(seen)[0]
	if body := first.Step.Content[0].Text; !strings.Contains(body, "say something") {
		t.Errorf("user_input step does not carry the task: %q", body)
	}
	if first.Step.Harness == nil || first.Step.Harness.Source != "input" {
		t.Errorf("user_input harness = %+v, want source \"input\"", first.Step.Harness)
	}

	// Text arrives as `text` deltas, the thought summary as
	// `thought_summary` deltas. One delta method carries both, told apart by
	// delta.type — where the Responses dialect has a method per kind.
	var text string
	for _, d := range iactDeltas(seen, iactDeltaText) {
		text += d.Delta.Text
	}
	if text != "All done." {
		t.Errorf("model_output text = %q, want %q", text, "All done.")
	}
	var summary string
	for _, d := range iactDeltas(seen, iactDeltaThoughtSummary) {
		if d.Delta.Content != nil {
			summary += d.Delta.Content.Text
		}
	}
	if summary != "Thinking about it." {
		t.Errorf("thought summary = %q", summary)
	}

	var done iactEnvelope
	if err := json.Unmarshal(seen[len(seen)-1].Params, &done); err != nil {
		t.Fatalf("decode interaction.completed: %v", err)
	}
	if done.Interaction.Status != StatusCompleted {
		t.Errorf("final status = %q, want %q", done.Interaction.Status, StatusCompleted)
	}
	if done.Interaction.Usage == nil || done.Interaction.Usage.TotalTokens == 0 {
		t.Errorf("completed frame carries no usage: %+v", done.Interaction.Usage)
	}
	// Google's own completed frame does not carry the steps: they were
	// streamed, and a client that wants the assembled document asks for it.
	// The Responses dialect answers this the other way.
	if len(done.Interaction.Steps) != 0 {
		t.Errorf("interaction.completed carries %d steps, want none", len(done.Interaction.Steps))
	}
}

// TestInteractionsCarriesTheThoughtSignature is the capability the Responses
// dialect does not have.
//
// A thought signature is the receipt Google issues for a thinking step, and
// it has to be replayed verbatim on the next request or the model rejects
// the turn (docs/GEMINI-INTEGRATION.md). The loop keeps it either way — that
// is what makes a Gemini session work at all — but only this vocabulary has
// somewhere to put it, so only a client here can store a transcript it could
// replay somewhere else.
//
// It is one complete opaque value, not a fragment: it arrives once, on the
// thought step it belongs to, before that step is stopped.
func TestInteractionsCarriesTheThoughtSignature(t *testing.T) {
	useDialect(t, NewInteractions())
	f := newFixture(t, answer("All done."))
	f.client.handshake(ClientCapabilities{})

	var created iactCreateResult
	if rerr := f.client.call(MethodInteractionsCreate, f.createParams("think about it"), &created); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	seen := f.client.waitFor(notifyIactCompleted)

	sigs := iactDeltas(seen, iactDeltaThoughtSignature)
	if len(sigs) != 1 {
		t.Fatalf("thought_signature deltas = %d, want exactly 1", len(sigs))
	}
	if sigs[0].Delta.Signature == "" {
		t.Error("the thought_signature delta carries no signature")
	}

	// It lands on the thought step, and before that step is stopped.
	var thoughtIndex = -1
	for _, s := range iactSteps(seen) {
		if s.Step.Type == iactStepThought {
			thoughtIndex = s.Index
		}
	}
	if sigs[0].Index != thoughtIndex {
		t.Errorf("signature is on step %d, want the thought step %d", sigs[0].Index, thoughtIndex)
	}
	sigAt, stopAt := -1, -1
	for i, m := range seen {
		switch m.Method {
		case notifyIactStepDelta:
			var d iactStepDelta
			if json.Unmarshal(m.Params, &d) == nil && d.Delta.Type == iactDeltaThoughtSignature {
				sigAt = i
			}
		case notifyIactStepStop:
			var st iactStepStop
			if json.Unmarshal(m.Params, &st) == nil && st.Index == thoughtIndex && stopAt < 0 {
				stopAt = i
			}
		}
	}
	if sigAt < 0 || stopAt < 0 || sigAt > stopAt {
		t.Errorf("signature at frame %d, thought step stopped at %d", sigAt, stopAt)
	}

	// The assembled step keeps it, so interactions.get answers with it too.
	var got iactCreateResult
	if rerr := f.client.call(MethodInteractionsGet, iactIDParams{InteractionID: created.Interaction.ID}, &got); rerr != nil {
		t.Fatalf("interactions.get: %v", rerr)
	}
	var found string
	for _, s := range got.Interaction.Steps {
		if s.Type == iactStepThought {
			found = s.Signature
		}
	}
	if found == "" {
		t.Error("the assembled thought step carries no signature")
	}
}

// TestInteractionsRefusesAnAgent pins the field this surface has and the
// other does not. Google's create body may name a managed agent instead of a
// model; those run on Google's machines, and this process runs the loop on
// the parent's.
func TestInteractionsRefusesAnAgent(t *testing.T) {
	useDialect(t, NewInteractions())
	f := newFixture(t, answer("unused"))
	f.client.handshake(ClientCapabilities{})

	params := map[string]any{
		"agent":   "agents/some-managed-agent",
		"input":   "do a thing",
		"harness": map[string]any{"cwd": f.cwd},
	}
	rerr := f.client.call(MethodInteractionsCreate, params, nil)
	if rerr == nil {
		t.Fatal("a create naming an agent was accepted")
	}
	if rerr.Code != CodeUnsupported {
		t.Errorf("code = %d, want %d", rerr.Code, CodeUnsupported)
	}
}

// TestInteractionsRefusesMaxSubTurns pins that the Interactions vocabulary
// refuses harness.max_sub_turns the same way the Responses one does: a run
// has no sub-turn ceiling, and a client that asked for one must not get a
// run it believes is bounded.
func TestInteractionsRefusesMaxSubTurns(t *testing.T) {
	useDialect(t, NewInteractions())
	f := newFixture(t, answer("unused"))
	f.client.handshake(ClientCapabilities{})

	params := map[string]any{
		"model":   testModel,
		"input":   "do a thing",
		"harness": map[string]any{"cwd": f.cwd, "max_sub_turns": 200},
	}
	rerr := f.client.call(MethodInteractionsCreate, params, nil)
	if rerr == nil {
		t.Fatal("a create naming harness.max_sub_turns was accepted")
	}
	if rerr.Code != CodeUnsupported {
		t.Errorf("code = %d, want %d", rerr.Code, CodeUnsupported)
	}
}

// TestInteractionsHandshakeSpellsGoogle pins the three parts of the
// handshake the two vocabularies disagree about.
func TestInteractionsHandshakeSpellsGoogle(t *testing.T) {
	useDialect(t, NewInteractions())
	f := newFixture(t, answer("unused"))

	var res iactInitializeResult
	raw := f.client.handshakeRaw(ClientCapabilities{}, &res)

	if res.ServerInfo.Protocol != "google.interactions.v1beta" {
		t.Errorf("protocol = %q", res.ServerInfo.Protocol)
	}
	if !res.Capabilities.PreviousInteraction {
		t.Error("capabilities.previous_interaction is not set")
	}
	if len(res.ModelDetails) == 0 || len(res.ModelDetails[0].ThinkingLevels) == 0 {
		t.Errorf("model_details carries no thinking_levels: %+v", res.ModelDetails)
	}
	// The Responses spelling must not also be present: a client reading
	// either key would otherwise not learn which surface it is on.
	if strings.Contains(raw, "reasoning_efforts") || strings.Contains(raw, "previous_response") {
		t.Errorf("the Interactions handshake carries Responses keys: %s", raw)
	}
}
