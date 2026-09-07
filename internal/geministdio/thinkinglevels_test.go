package geministdio

import (
	"slices"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/gemini"
)

// The handshake carries the levels each model takes, because the answer is
// per-model: gemini-3.7-flash refuses "minimal" and its siblings accept it.
// A client that reads this never has to find out from a 400.
func TestHandshakeAdvertisesThinkingLevels(t *testing.T) {
	f := newFixture(t, answer("hello"))
	res := f.client.handshake(ClientCapabilities{})

	levels, ok := res.ThinkingLevels[testModel]
	if !ok {
		t.Fatalf("thinking_levels has no entry for %s: %v", testModel, res.ThinkingLevels)
	}
	if slices.Contains(levels, gemini.ThinkingLevelMinimal) {
		t.Errorf("%s levels = %v, must not offer minimal", testModel, levels)
	}
	if !slices.Contains(levels, gemini.ThinkingLevelHigh) {
		t.Errorf("%s levels = %v, want high in it", testModel, levels)
	}
	// Only models this process accepts are described.
	for model := range res.ThinkingLevels {
		if !slices.Contains(res.Models, model) {
			t.Errorf("thinking_levels describes %q, which is not in models %v", model, res.Models)
		}
	}
}

// A level the model refuses is refused here, before the run starts. Left to
// Google it is a 400 in the middle of a started interaction: the create is
// answered, steps stream, and the run ends failed carrying a message about a
// request the client never made itself.
func TestCreateRefusesAnUnsupportedThinkingLevel(t *testing.T) {
	f := newFixture(t, answer("hello"))
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("do a thing")
	p.GenerationConfig = &GenerationConfig{ThinkingLevel: gemini.ThinkingLevelMinimal}

	rerr := f.client.call(MethodInteractionsCreate, p, nil)
	if rerr == nil {
		t.Fatal("create accepted a thinking level the model refuses")
	}
	if rerr.Code != CodeInvalidParams {
		t.Errorf("code = %d, want %d", rerr.Code, CodeInvalidParams)
	}
	for _, want := range []string{"minimal", testModel, "low"} {
		if !strings.Contains(rerr.Message, want) {
			t.Errorf("message should name %q, got: %s", want, rerr.Message)
		}
	}
}

// A level the model does take is accepted, so the check cannot pass by
// refusing everything.
func TestCreateAcceptsASupportedThinkingLevel(t *testing.T) {
	f := newFixture(t, answer("hello"))
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("do a thing")
	p.GenerationConfig = &GenerationConfig{ThinkingLevel: gemini.ThinkingLevelLow}

	var created CreateResult
	if rerr := f.client.call(MethodInteractionsCreate, p, &created); rerr != nil {
		t.Fatalf("create refused a supported level: %v", rerr)
	}
	f.client.waitFor(NotifyInteractionCompleted)
}
