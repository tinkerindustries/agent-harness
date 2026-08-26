package main

import (
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/deepseek"
	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/kimi"
)

// TestClientForModelRoutesGemini pins that a gemini-3.7-flash request is
// handed the *gemini.Client cmd/harness built, the same per-model routing
// run, resume, and serve all resolve through (docs/GEMINI-INTEGRATION.md §7
// Phase 5). No network call is made: this only proves the composition point
// picks the right client, not that the client works — Phase 8 is a live run.
func TestClientForModelRoutesGemini(t *testing.T) {
	deepSeekClient := deepseek.NewClient("http://127.0.0.1:0", "sk-test")
	kimiClient := kimi.NewClient("http://127.0.0.1:0", "sk-test")
	geminiClient := gemini.NewClient(gemini.DefaultBaseURL)

	got := clientForModel("gemini-3.7-flash", deepSeekClient, kimiClient, geminiClient)
	if got != geminiClient {
		t.Errorf("clientForModel(gemini-3.7-flash) did not return the Gemini client")
	}
}

// TestClientForModelStillRoutesDeepSeekAndKimi is the pre-existing routing,
// re-pinned alongside the Gemini case above so the three-way switch in
// clientForModel is read as one table rather than the Kimi special case it
// used to be.
func TestClientForModelStillRoutesDeepSeekAndKimi(t *testing.T) {
	deepSeekClient := deepseek.NewClient("http://127.0.0.1:0", "sk-test")
	kimiClient := kimi.NewClient("http://127.0.0.1:0", "sk-test")
	geminiClient := gemini.NewClient(gemini.DefaultBaseURL)

	if got := clientForModel("kimi-k3", deepSeekClient, kimiClient, geminiClient); got != kimiClient {
		t.Errorf("clientForModel(kimi-k3) did not return the Kimi client")
	}
	for _, model := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "no-such-model"} {
		if got := clientForModel(model, deepSeekClient, kimiClient, geminiClient); got != deepSeekClient {
			t.Errorf("clientForModel(%q) did not return the DeepSeek client", model)
		}
	}
}
