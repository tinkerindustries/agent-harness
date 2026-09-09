package responsesstdio

import (
	"slices"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// detailFor pulls one model's handshake entry, failing when it is absent.
func detailFor(t *testing.T, res InitializeResult, model string) ModelDetail {
	t.Helper()
	for _, d := range res.ModelDetails {
		if d.ID == model {
			return d
		}
	}
	t.Fatalf("model_details has no entry for %s: %v", model, res.ModelDetails)
	return ModelDetail{}
}

// The handshake answers for a DeepSeek model out of DeepSeek's own tables,
// not Google's. A client asks one question — what may I send this model —
// and does not have to know whose model it is to get an answer.
func TestHandshakeAdvertisesTheDeepSeekModel(t *testing.T) {
	f := newFixture(t, answer("hello"))
	res := f.client.handshake(ClientCapabilities{})

	detail := detailFor(t, res, testDeepSeekModel)
	if detail.DisplayName == "" {
		t.Errorf("%s has no display_name", testDeepSeekModel)
	}
	if detail.ContextWindowTokens <= 0 {
		t.Errorf("%s context_window_tokens = %d, want a positive figure", testDeepSeekModel, detail.ContextWindowTokens)
	}
	// DeepSeek's reasoning efforts, not Google's thinking levels: "max" is
	// real here and is not one of Google's four, and "minimal" is one of
	// Google's four and is not real here.
	if !slices.Contains(detail.ReasoningEfforts, wire.EffortMax) {
		t.Errorf("%s levels = %v, want max in it", testDeepSeekModel, detail.ReasoningEfforts)
	}
	if slices.Contains(detail.ReasoningEfforts, gemini.ThinkingLevelMinimal) {
		t.Errorf("%s levels = %v, must not offer minimal", testDeepSeekModel, detail.ReasoningEfforts)
	}
	// The Gemini entries are unchanged by the DeepSeek one's presence.
	if g := detailFor(t, res, testModel); slices.Contains(g.ReasoningEfforts, wire.EffortMax) {
		t.Errorf("%s levels = %v, max is DeepSeek's spelling", testModel, g.ReasoningEfforts)
	}
}

// A model this repository can route but this process did not advertise is
// refused by name, with the accepted set in the message. DeepSeek's
// text-only models are the case that matters: hosting one would mean a
// session carrying vision tools that reach Google, so the refusal is the
// feature (docs/DEEPSEEK-VISION.md, cmd/harness's deepSeekSessionModel).
func TestCreateRefusesARoutableButUnhostedModel(t *testing.T) {
	f := newFixture(t, answer("hello"))
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("do a thing")
	p.Model = testUnhostedModel

	rerr := f.client.call(MethodResponsesCreate, p, nil)
	if rerr == nil {
		t.Fatal("create accepted a model this process does not host")
	}
	if rerr.Code != CodeInvalidParams {
		t.Errorf("code = %d, want %d", rerr.Code, CodeInvalidParams)
	}
	for _, want := range []string{testUnhostedModel, testDeepSeekModel} {
		if !strings.Contains(rerr.Message, want) {
			t.Errorf("message should name %q, got: %s", want, rerr.Message)
		}
	}
}

// "minimal" is one of Google's levels and not one of DeepSeek's, so a create
// naming it on the DeepSeek model is refused here rather than 400ing
// mid-run — the same rule the Gemini models get, answered from the other
// provider's table.
func TestCreateRefusesMinimalOnTheDeepSeekModel(t *testing.T) {
	f := newFixture(t, answer("hello"))
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("do a thing")
	p.Model = testDeepSeekModel
	p.Reasoning = &ReasoningConfig{Effort: gemini.ThinkingLevelMinimal}

	rerr := f.client.call(MethodResponsesCreate, p, nil)
	if rerr == nil {
		t.Fatal("create accepted a level DeepSeek does not document")
	}
	if rerr.Code != CodeInvalidParams {
		t.Errorf("code = %d, want %d", rerr.Code, CodeInvalidParams)
	}
	for _, want := range []string{"minimal", testDeepSeekModel, wire.EffortMax} {
		if !strings.Contains(rerr.Message, want) {
			t.Errorf("message should name %q, got: %s", want, rerr.Message)
		}
	}
}

// "medium" is one of Google's four levels, and DeepSeek accepts it and maps
// it onto high rather than rejecting it. A client offering Google's set must
// not have its create refused on a level the API would have taken, which is
// why acceptance is asked of the provider rather than derived from the
// advertised list.
func TestCreateAcceptsMediumOnTheDeepSeekModel(t *testing.T) {
	f := newFixture(t, answer("hello"))
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("do a thing")
	p.Model = testDeepSeekModel
	p.Reasoning = &ReasoningConfig{Effort: gemini.ThinkingLevelMedium}

	var created CreateResult
	if rerr := f.client.call(MethodResponsesCreate, p, &created); rerr != nil {
		t.Fatalf("create refused a level DeepSeek maps onto high: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)
}

// The missing-credential error names the variable the host actually forgot.
// One process can hold both providers' keys, so a message naming Google's
// two for a DeepSeek run would send the host to fix the wrong thing.
func TestMissingCredentialsNameTheModelsOwnProvider(t *testing.T) {
	f := newFixture(t, answer("hi"))
	f.srv.opts.HasAPIKey = func(model string) bool { return model != testDeepSeekModel }
	f.client.handshake(ClientCapabilities{})

	p := f.createParams("go")
	p.Model = testDeepSeekModel
	rerr := f.client.call(MethodResponsesCreate, p, nil)
	if rerr == nil || rerr.Code != CodeCredentialsMissing {
		t.Fatalf("a create with no DeepSeek key was accepted or misreported: %v", rerr)
	}
	if !strings.Contains(rerr.Message, "DEEPSEEK_API_KEY") {
		t.Errorf("the error does not name DeepSeek's variable: %q", rerr.Message)
	}
	if strings.Contains(rerr.Message, "GEMINI_API_KEY") {
		t.Errorf("the error sends the host to Google's variable: %q", rerr.Message)
	}

	// The Gemini models are unaffected by the other provider's key being
	// the missing one.
	if rerr := f.client.call(MethodResponsesCreate, f.createParams("go"), &CreateResult{}); rerr != nil {
		t.Fatalf("a Gemini create was refused for DeepSeek's missing key: %v", rerr)
	}
	f.client.waitFor(NotifyResponseCompleted)
}
