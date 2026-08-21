package provider

import "testing"

func TestModelForResolvesKnownModels(t *testing.T) {
	cases := []struct {
		model    string
		provider Name
	}{
		{"deepseek-v4-pro", DeepSeek},
		{"deepseek-v4-flash", DeepSeek},
		{"kimi-k3", Kimi},
		{"gemini-3.7-flash", Gemini},
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
