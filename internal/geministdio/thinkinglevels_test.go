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

	var detail ModelDetail
	found := false
	for _, d := range res.ModelDetails {
		if d.ID == testModel {
			detail, found = d, true
			break
		}
	}
	if !found {
		t.Fatalf("model_details has no entry for %s: %v", testModel, res.ModelDetails)
	}
	if slices.Contains(detail.ThinkingLevels, gemini.ThinkingLevelMinimal) {
		t.Errorf("%s levels = %v, must not offer minimal", testModel, detail.ThinkingLevels)
	}
	if !slices.Contains(detail.ThinkingLevels, gemini.ThinkingLevelHigh) {
		t.Errorf("%s levels = %v, want high in it", testModel, detail.ThinkingLevels)
	}
	if detail.ContextWindowTokens <= 0 {
		t.Errorf("%s context_window_tokens = %d, want a positive figure", testModel, detail.ContextWindowTokens)
	}
	// One entry per name in Models, same order, nothing extra.
	if len(res.ModelDetails) != len(res.Models) {
		t.Fatalf("model_details has %d entries, models has %d", len(res.ModelDetails), len(res.Models))
	}
	for i, m := range res.Models {
		if res.ModelDetails[i].ID != m {
			t.Errorf("model_details[%d].id = %q, want %q (models order)", i, res.ModelDetails[i].ID, m)
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
