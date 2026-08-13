package provider

import "testing"

func TestModelForResolvesKnownModels(t *testing.T) {
	for _, model := range []string{"deepseek-v4-pro", "deepseek-v4-flash"} {
		p, err := ModelFor(model)
		if err != nil {
			t.Errorf("ModelFor(%q): %v", model, err)
		}
		if p != DeepSeek {
			t.Errorf("ModelFor(%q) = %q, want %q", model, p, DeepSeek)
		}
	}
}

// An unknown model must fail loudly: the table has no default, so a typo is
// an error at validation rather than a silent run on the wrong provider.
func TestModelForRejectsUnknownModel(t *testing.T) {
	for _, model := range []string{"deepseek-v4-turbo", "kimi-k3", "gpt-4", ""} {
		if _, err := ModelFor(model); err == nil {
			t.Errorf("ModelFor(%q) = nil error, want one", model)
		}
	}
	if !Known("deepseek-v4-pro") {
		t.Error("Known(deepseek-v4-pro) = false, want true")
	}
	if Known("kimi-k3") {
		t.Error("Known(kimi-k3) = true, want false")
	}
}
