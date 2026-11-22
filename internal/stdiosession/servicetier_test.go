package stdiosession

import (
	"slices"
	"strings"
	"testing"
)

func (f *fixture) lastRequest() string {
	f.t.Helper()
	f.script.mu.Lock()
	defer f.script.mu.Unlock()
	if len(f.script.requests) == 0 {
		f.t.Fatal("the model received no request")
	}
	return string(f.script.requests[len(f.script.requests)-1])
}

func TestHandshakeAdvertisesServiceTiers(t *testing.T) {
	f := newFixture(t, answer("hello"))
	res := f.client.handshake(ClientCapabilities{})

	got := map[string][]string{}
	for _, d := range res.ModelDetails {
		got[d.ID] = d.ServiceTiers
	}
	if !slices.Equal(got[testFlexModel], []string{"flex"}) {
		t.Errorf("%s service_tiers = %v, want [flex]", testFlexModel, got[testFlexModel])
	}
	for _, m := range []string{testModel, testAltModel, testDeepSeekModel} {
		if len(got[m]) != 0 {
			t.Errorf("%s service_tiers = %v, want none", m, got[m])
		}
	}
}

func TestCreateRefusesAServiceTierTheModelDoesNotOffer(t *testing.T) {
	for _, tc := range []struct{ name, model, tier, want string }{
		{"gemini model without flex", testModel, "flex", testModel},
		{"deepseek model", testDeepSeekModel, "flex", testDeepSeekModel},
		{"unknown value", testFlexModel, "turbo", "want"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, answer("hello"))
			f.client.handshake(ClientCapabilities{})

			p := f.createParams("do a thing")
			p.Model = tc.model
			p.ServiceTier = tc.tier
			rerr := f.client.call(MethodResponsesCreate, p, nil)
			if rerr == nil {
				t.Fatal("create accepted the service tier")
			}
			if rerr.Code != CodeInvalidParams {
				t.Errorf("code = %d, want %d", rerr.Code, CodeInvalidParams)
			}
			if !strings.Contains(rerr.Message, tc.tier) || !strings.Contains(rerr.Message, tc.want) {
				t.Errorf("message should name %q and %q, got: %s", tc.tier, tc.want, rerr.Message)
			}
		})
	}
}

func TestCreateSendsTheServiceTierToTheModel(t *testing.T) {
	for _, tc := range []struct {
		name, model, tier string
		sent              bool
	}{
		{"flex", testFlexModel, "flex", true},
		{"standard is not sent", testFlexModel, "standard", false},
		{"none is not sent", testFlexModel, "", false},
		{"standard on a model with no flex", testModel, "standard", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, answer("hello"))
			f.client.handshake(ClientCapabilities{})

			p := f.createParams("do a thing")
			p.Model = tc.model
			p.ServiceTier = tc.tier
			if rerr := f.client.call(MethodResponsesCreate, p, &CreateResult{}); rerr != nil {
				t.Fatalf("create: %v", rerr)
			}
			f.client.waitFor(NotifyResponseCompleted)

			has := strings.Contains(f.lastRequest(), `"service_tier":"flex"`)
			if has != tc.sent {
				t.Errorf("request carries service_tier flex = %v, want %v: %s", has, tc.sent, f.lastRequest())
			}
			if strings.Contains(f.lastRequest(), `"service_tier":"standard"`) {
				t.Errorf("request carries service_tier standard: %s", f.lastRequest())
			}
		})
	}
}

func TestInteractionsCreateSendsTheServiceTier(t *testing.T) {
	useDialect(t, NewInteractions())
	f := newFixture(t, answer("hello"))
	res := f.client.handshake(ClientCapabilities{})
	for _, d := range res.ModelDetails {
		if d.ID == testFlexModel && !slices.Equal(d.ServiceTiers, []string{"flex"}) {
			t.Errorf("%s service_tiers = %v, want [flex]", d.ID, d.ServiceTiers)
		}
	}

	p := f.createParams("do a thing")
	p.Model = testFlexModel
	p.ServiceTier = "flex"
	if rerr := f.client.call(MethodInteractionsCreate, p, &iactCreateResult{}); rerr != nil {
		t.Fatalf("interactions.create: %v", rerr)
	}
	f.client.waitFor(notifyIactCompleted)
	if !strings.Contains(f.lastRequest(), `"service_tier":"flex"`) {
		t.Errorf("request does not carry service_tier flex: %s", f.lastRequest())
	}
}

func TestResumeSendsTheServiceTierItWasGiven(t *testing.T) {
	dir, cwd := t.TempDir(), t.TempDir()

	a := newFixtureIn(t, dir, cwd, answer("first"))
	a.client.handshake(ClientCapabilities{})
	first := a.createParams("first task")
	first.Model = testFlexModel
	var one CreateResult
	if rerr := a.client.call(MethodResponsesCreate, first, &one); rerr != nil {
		t.Fatalf("first create: %v", rerr)
	}
	a.client.waitFor(NotifyResponseCompleted)
	sessionID := one.Response.Harness.SessionID
	if rerr := a.client.call(MethodShutdown, map[string]any{}, nil); rerr != nil {
		t.Fatalf("shutdown: %v", rerr)
	}
	a.close()

	b := newFixtureIn(t, dir, cwd, answer("second"))
	b.client.handshake(ClientCapabilities{})
	resume := resumeParams(sessionID, "second task")
	resume.Model = testFlexModel
	resume.ServiceTier = "flex"
	if rerr := b.client.call(MethodResponsesCreate, resume, &CreateResult{}); rerr != nil {
		t.Fatalf("resume: %v", rerr)
	}
	b.client.waitFor(NotifyResponseCompleted)
	if !strings.Contains(b.lastRequest(), `"service_tier":"flex"`) {
		t.Errorf("the resumed run's request does not carry service_tier flex: %s", b.lastRequest())
	}
}
