package provider

import "testing"

func TestModelForResolvesKnownModels(t *testing.T) {
	cases := []struct {
		model    string
		provider Name
	}{
		{"deepseek-v4-pro", DeepSeek},
		{"deepseek-v4-flash", DeepSeek},
		{"deepseek-v4-flash-vision-exp", DeepSeek},
		{"kimi-k3", Kimi},
		{"gemini-3.8-flash", Gemini},
		{"gemini-3.7-flash", Gemini},
		{"gemini-3.6-flash", Gemini},
		{"gemini-3.5-flash", Gemini},
		{"gemini-3.5-flash-lite", Gemini},
	}
	for _, tc := range cases {
		p, err := ModelFor(tc.model)
		if err != nil {
			t.Errorf("ModelFor(%q): %v", tc.model, err)
			continue
		}
		if p != tc.provider {
			t.Errorf("ModelFor(%q) = %q, want %q", tc.model, p, tc.provider)
		}
	}
}

// An unknown model must fail loudly: the table has no default, so a typo is
// an error at validation rather than a silent run on the wrong provider.
func TestModelForRejectsUnknownModel(t *testing.T) {
	for _, model := range []string{"deepseek-v4-turbo", "kimi-k2.6", "gpt-4", ""} {
		if _, err := ModelFor(model); err == nil {
			t.Errorf("ModelFor(%q) = nil error, want one", model)
		}
	}
	if !Known("deepseek-v4-pro") {
		t.Error("Known(deepseek-v4-pro) = false, want true")
	}
	if !Known("kimi-k3") {
		t.Error("Known(kimi-k3) = false, want true")
	}
	if !Known("gemini-3.7-flash") {
		t.Error("Known(gemini-3.7-flash) = false, want true")
	}
	if Known("gpt-4") {
		t.Error("Known(gpt-4) = true, want false")
	}
}

// TestSeesImages pins the model→capability table directly, keyed by model
// rather than by provider: deepseek-v4-flash-vision-exp is a DeepSeek model,
// but this table is what lets its capability diverge from DeepSeek's other
// two — it reads images natively, they don't (docs/DEEPSEEK-VISION.md).
// internal/session's own seesImages and internal/tools.DefinitionsFor both
// read through SeesImages, so a model added here without a matching entry
// changes what both of them do; this test is what catches that at the
// source rather than at either caller.
func TestSeesImages(t *testing.T) {
	cases := []struct {
		model string
		want  bool
	}{
		{"deepseek-v4-pro", false},
		{"deepseek-v4-flash", false},
		{"deepseek-v4-flash-vision-exp", true},
		{"kimi-k3", true},
		{"gemini-3.8-flash", true},
		{"gemini-3.7-flash", true},
		{"gemini-3.6-flash", true},
		{"gemini-3.5-flash", true},
		{"gemini-3.5-flash-lite", true},
		{"not-a-real-model", false},
	}
	for _, tc := range cases {
		if got := SeesImages(tc.model); got != tc.want {
			t.Errorf("SeesImages(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// TestKnownModelsListsTheVisionModel pins that deepseek-v4-flash-vision-exp
// appears in KnownModels(), the list the handshake publishes verbatim
// and a create body's model name is validated against. A model missing here cannot be
// hosted at all, even though ModelFor would resolve it.
func TestKnownModelsListsTheVisionModel(t *testing.T) {
	found := false
	for _, m := range KnownModels() {
		if m == "deepseek-v4-flash-vision-exp" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("KnownModels() = %v, want it to include deepseek-v4-flash-vision-exp", KnownModels())
	}
}

// TestVisionCapableCoversEveryKnownModel asserts models and visionCapable
// carry the same key set. SeesImages resolves a model missing from
// visionCapable to false, the same value an unknown model resolves to,
// because a run must not panic on an absent key. A model registered in
// models but missing from visionCapable relies on that fallback by
// accident: it compiles, it runs, and it never sees images, with no test
// failing to say so. This test names the model and the table it is missing
// from, the way internal/settings/registry_test.go pins its own matching
// pair of tables, so the person adding a model fills in both at once
// instead of trusting memory for the second one.
func TestVisionCapableCoversEveryKnownModel(t *testing.T) {
	for model := range models {
		if _, ok := visionCapable[model]; !ok {
			t.Errorf("%q is in models but missing from visionCapable", model)
		}
	}
	for model := range visionCapable {
		if _, ok := models[model]; !ok {
			t.Errorf("%q is in visionCapable but missing from models", model)
		}
	}
}
