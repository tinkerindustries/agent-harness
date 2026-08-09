package cache

import (
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
)

func TestFirstObserveHasNoChurn(t *testing.T) {
	d := NewDetector()
	messages := []deepseek.Message{deepseek.SystemMessage("sys"), deepseek.UserMessage("hi")}
	report := d.Observe(messages, deepseek.Usage{PromptTokens: 300, PromptCacheHitTokens: 0, PromptCacheMissTokens: 300})
	if report.Churned {
		t.Fatal("first observation should never be reported as churn")
	}
	if report.ExpectedMissTokens != 300 {
		t.Fatalf("expected the whole cold prompt to be the expected miss, got %d", report.ExpectedMissTokens)
	}
}

func TestAppendOnlyGrowthDoesNotChurn(t *testing.T) {
	d := NewDetector()
	first := []deepseek.Message{deepseek.SystemMessage("sys"), deepseek.UserMessage("hi")}
	d.Observe(first, deepseek.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000})

	// Second request appends one message (a tool result) and the API
	// reports the full first request's floor(1000/128)*128 = 896 as a hit.
	second := append(append([]deepseek.Message{}, first...), deepseek.Message{Role: deepseek.RoleAssistant, Content: "ok"})
	report := d.Observe(second, deepseek.Usage{PromptTokens: 1080, PromptCacheHitTokens: 896, PromptCacheMissTokens: 184})
	if report.Churned {
		t.Fatalf("appending content should not churn: %+v", report)
	}
	if report.ExpectedMissTokens != 1080-896 {
		t.Fatalf("expected miss %d, got %d", 1080-896, report.ExpectedMissTokens)
	}
}

func TestMutatingAnEarlierMessageChurns(t *testing.T) {
	d := NewDetector()
	first := []deepseek.Message{deepseek.SystemMessage("sys"), deepseek.UserMessage("hi"), deepseek.UserMessage("more")}
	d.Observe(first, deepseek.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000})

	// Second request mutates message index 1 instead of only appending —
	// the whole conversation should now report as a near-total miss.
	mutated := []deepseek.Message{deepseek.SystemMessage("sys"), deepseek.UserMessage("hi, edited"), deepseek.UserMessage("more")}
	report := d.Observe(mutated, deepseek.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000})
	if !report.Churned {
		t.Fatal("mutating an earlier message should be reported as churn")
	}
	if report.ChurnPointIndex == nil || *report.ChurnPointIndex != 1 {
		t.Fatalf("expected churn point index 1, got %v", report.ChurnPointIndex)
	}
}
