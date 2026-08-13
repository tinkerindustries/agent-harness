package cache

import (
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// deepSeekSplit builds the Split the provider seam would produce for
// DeepSeek-shaped raw usage, where the API reports prompt_cache_hit_tokens
// and prompt_cache_miss_tokens separately and UsageSplit passes them
// through unchanged (internal/deepseek/client.go). The verdicts these
// produce are the ones the detector gave before the Split input existed —
// the "no behaviour change" half of the migration.
func deepSeekSplit(usage wire.Usage) Split {
	return Split{
		PromptTokens:     usage.PromptTokens,
		CacheHitTokens:   usage.PromptCacheHitTokens,
		CacheMissTokens:  usage.PromptCacheMissTokens,
		CompletionTokens: usage.CompletionTokens,
	}
}

// kimiSplit builds the Split the provider seam would produce for
// Kimi-shaped raw usage, where the API reports only cached_tokens and
// UsageSplit derives the miss as prompt_tokens - cached_tokens
// (internal/kimi/client.go). The raw usage's prompt_cache_miss_tokens field
// — what the detector used to read directly — stays zero, exactly as the
// API leaves it.
func kimiSplit(usage wire.Usage) Split {
	return Split{
		PromptTokens:     usage.PromptTokens,
		CacheHitTokens:   usage.CachedTokens,
		CacheMissTokens:  usage.PromptTokens - usage.CachedTokens,
		CompletionTokens: usage.CompletionTokens,
	}
}

func TestFirstObserveHasNoChurn(t *testing.T) {
	d := NewDetector()
	messages := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi")}
	report := d.Observe(messages, deepSeekSplit(wire.Usage{PromptTokens: 300, PromptCacheHitTokens: 0, PromptCacheMissTokens: 300}))
	if report.Churned {
		t.Fatal("first observation should never be reported as churn")
	}
	if report.ExpectedMissTokens != 300 {
		t.Fatalf("expected the whole cold prompt to be the expected miss, got %d", report.ExpectedMissTokens)
	}
}

func TestAppendOnlyGrowthDoesNotChurn(t *testing.T) {
	d := NewDetector()
	first := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi")}
	d.Observe(first, deepSeekSplit(wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000}))

	// Second request appends one message (a tool result) and the API
	// reports the full first request's floor(1000/128)*128 = 896 as a hit.
	second := append(append([]wire.Message{}, first...), wire.Message{Role: wire.RoleAssistant, Content: "ok"})
	report := d.Observe(second, deepSeekSplit(wire.Usage{PromptTokens: 1080, PromptCacheHitTokens: 896, PromptCacheMissTokens: 184}))
	if report.Churned {
		t.Fatalf("appending content should not churn: %+v", report)
	}
	if report.ExpectedMissTokens != 1080-896 {
		t.Fatalf("expected miss %d, got %d", 1080-896, report.ExpectedMissTokens)
	}
}

func TestMutatingAnEarlierMessageChurns(t *testing.T) {
	d := NewDetector()
	first := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi"), wire.UserMessage("more")}
	d.Observe(first, deepSeekSplit(wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000}))

	// Second request mutates message index 1 instead of only appending —
	// the whole conversation should now report as a near-total miss.
	mutated := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi, edited"), wire.UserMessage("more")}
	report := d.Observe(mutated, deepSeekSplit(wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000}))
	if !report.Churned {
		t.Fatal("mutating an earlier message should be reported as churn")
	}
	if report.ChurnPointIndex == nil || *report.ChurnPointIndex != 1 {
		t.Fatalf("expected churn point index 1, got %v", report.ChurnPointIndex)
	}
}

// TestPrimedDetectorContinuesAcrossResume asserts NewDetectorFrom puts a
// Detector in the same state Observe would have left it in, so a session
// resumed after a real terminal gap still gets a real churn check on its
// first post-resume sub-turn instead of the always-clean report a fresh
// Detector gives its first call.
func TestPrimedDetectorContinuesAcrossResume(t *testing.T) {
	priorRequest := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi")}
	d := NewDetectorFrom(1000, priorRequest)

	// A healthy continuation: the new request only appends past
	// priorRequest, so it must not churn.
	healthy := append(append([]wire.Message{}, priorRequest...), wire.UserMessage("continue"))
	report := d.Observe(healthy, deepSeekSplit(wire.Usage{PromptTokens: 1050, PromptCacheHitTokens: 896, PromptCacheMissTokens: 154}))
	if report.Churned {
		t.Fatalf("appending after a primed detector should not churn: %+v", report)
	}

	d2 := NewDetectorFrom(1000, priorRequest)
	mutated := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi, edited")}
	report2 := d2.Observe(mutated, deepSeekSplit(wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000}))
	if !report2.Churned {
		t.Fatal("mutating the message a primed detector remembers should churn")
	}
	if report2.ChurnPointIndex == nil || *report2.ChurnPointIndex != 1 {
		t.Fatalf("expected churn point index 1, got %v", report2.ChurnPointIndex)
	}
}

// TestMutateChurnsAndNamesTheIndex is the unit-level half of the exit
// criterion "a deliberately churned prefix is caught by the diagnostic and
// named to the specific message": Mutate is the debug hook, this proves the
// Detector catches what it produces and names the mutated index. The live
// demonstration against the real API drives the same hook through
// session.RunOptions.DebugChurnAtSubTurn (internal/session/turn.go).
func TestMutateChurnsAndNamesTheIndex(t *testing.T) {
	d := NewDetector()
	first := []wire.Message{
		wire.SystemMessage("sys"),
		wire.UserMessage("workspace and task"),
		{Role: wire.RoleAssistant, Content: "ok"},
	}
	d.Observe(first, deepSeekSplit(wire.Usage{PromptTokens: 2000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 2000}))

	churned := Mutate(first, 1)
	report := d.Observe(churned, deepSeekSplit(wire.Usage{PromptTokens: 2000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 2000}))
	if !report.Churned {
		t.Fatal("Mutate's output should be reported as churn")
	}
	if report.ChurnPointIndex == nil || *report.ChurnPointIndex != 1 {
		t.Fatalf("expected churn point index 1 (the mutated message), got %v", report.ChurnPointIndex)
	}
}

// TestKimiShapedUsageChurnsLikeDeepSeek is the case the old code got wrong:
// Kimi K3's raw usage reports only cached_tokens, leaving the
// prompt_cache_miss_tokens field the detector used to read directly at
// zero. A genuinely churned prefix — the API answers with a near-total miss
// — would therefore have read as "no churn" (actual 0 ≤ expected + slack)
// and the diagnostic would have stayed silent on every K3 session. Fed the
// seam's derived split instead, the same input must produce exactly the
// verdict and churn point the DeepSeek-shaped equivalent produces.
func TestKimiShapedUsageChurnsLikeDeepSeek(t *testing.T) {
	first := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi"), wire.UserMessage("more")}
	mutated := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi, edited"), wire.UserMessage("more")}

	// The churned request's raw usage, as Kimi reports it: prompt 1000,
	// cached_tokens 0 (the prefix collapsed, so nothing hit). The raw
	// prompt_cache_miss_tokens field is absent — zero — which is what the
	// detector used to compare against.
	raw := wire.Usage{PromptTokens: 1000, CachedTokens: 0}

	dk := NewDetector()
	dk.Observe(first, kimiSplit(wire.Usage{PromptTokens: 1000, CachedTokens: 1000}))
	kimiReport := dk.Observe(mutated, kimiSplit(raw))

	dd := NewDetector()
	dd.Observe(first, deepSeekSplit(wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000}))
	deepSeekReport := dd.Observe(mutated, deepSeekSplit(wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000}))

	if !kimiReport.Churned {
		t.Fatalf("kimi-shaped churned usage must be reported as churn, got %+v", kimiReport)
	}
	if kimiReport.ActualMissTokens != 1000 {
		t.Fatalf("kimi-shaped actual miss = %d, want 1000 (the derived miss, not the raw usage's zero)", kimiReport.ActualMissTokens)
	}
	if kimiReport.ChurnPointIndex == nil || *kimiReport.ChurnPointIndex != 1 {
		t.Fatalf("expected churn point index 1, got %v", kimiReport.ChurnPointIndex)
	}
	// Same verdicts as the DeepSeek-shaped equivalent: the split input makes
	// the detector provider-agnostic.
	if kimiReport.Churned != deepSeekReport.Churned ||
		kimiReport.ActualMissTokens != deepSeekReport.ActualMissTokens ||
		kimiReport.ExpectedMissTokens != deepSeekReport.ExpectedMissTokens {
		t.Fatalf("kimi-shaped report %+v differs from deepseek-shaped %+v", kimiReport, deepSeekReport)
	}
}

// TestKimiShapedHealthyUsageDoesNotChurn pins the healthy side of the same
// coin: an honest append on Kimi — cached_tokens covering the previous
// prefix, miss covering only the appended tail — must not churn, so the
// split input does not trade the old false negatives for new false
// positives.
func TestKimiShapedHealthyUsageDoesNotChurn(t *testing.T) {
	d := NewDetector()
	first := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi")}
	d.Observe(first, kimiSplit(wire.Usage{PromptTokens: 1000, CachedTokens: 0}))

	second := append(append([]wire.Message{}, first...), wire.Message{Role: wire.RoleAssistant, Content: "ok"})
	report := d.Observe(second, kimiSplit(wire.Usage{PromptTokens: 1080, CachedTokens: 896}))
	if report.Churned {
		t.Fatalf("kimi-shaped append-only growth should not churn: %+v", report)
	}
	if report.ExpectedMissTokens != 1080-896 {
		t.Fatalf("expected miss %d, got %d", 1080-896, report.ExpectedMissTokens)
	}
}
