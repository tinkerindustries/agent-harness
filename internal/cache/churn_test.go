package cache

import (
	"fmt"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// deepSeekSplit builds the Split the provider seam would produce for
// DeepSeek-shaped raw usage, where the API reports prompt_cache_hit_tokens
// and prompt_cache_miss_tokens separately and UsageSplit passes them
// through unchanged (internal/deepseek/client.go). The verdicts these
// produce are the ones the detector gave before the Split input existed —
// the "no behaviour change" half of the migration. Slack is the provider's
// tolerance: the trailing partial 128-token block, 127.
func deepSeekSplit(usage wire.Usage) Split {
	return Split{
		PromptTokens:     usage.PromptTokens,
		CacheHitTokens:   usage.PromptCacheHitTokens,
		CacheMissTokens:  usage.PromptCacheMissTokens,
		CompletionTokens: usage.CompletionTokens,
		Slack:            127,
	}
}

// kimiSplit builds the Split the provider seam would produce for
// Kimi-shaped raw usage, where the API reports only cached_tokens and
// UsageSplit derives the miss as prompt_tokens - cached_tokens
// (internal/kimi/client.go). The raw usage's prompt_cache_miss_tokens field
// — what the detector used to read directly — stays zero, exactly as the
// API leaves it. Slack is the provider's tolerance: 512, the largest
// over-prediction across the thirteen observed kimi-k3 sub-turns
// (docs/OBSERVED.md).
func kimiSplit(usage wire.Usage) Split {
	return Split{
		PromptTokens:     usage.PromptTokens,
		CacheHitTokens:   usage.CachedTokens,
		CacheMissTokens:  usage.PromptTokens - usage.CachedTokens,
		CompletionTokens: usage.CompletionTokens,
		Slack:            512,
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
	second := append(append([]wire.Message{}, first...), wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("ok")})
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
		{Role: wire.RoleAssistant, Content: wire.TextContent("ok")},
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

// kimiObservation is one row of the thirteen live kimi-k3 sub-turns measured
// across three sessions and recorded in docs/OBSERVED.md ("Kimi K3
// over-predicts the cache hit"). expectedHit is what the detector predicted
// (floor of the previous request's cacheable tokens, which the priming
// observation below reconstructs exactly), actualHit what the API reported.
type kimiObservation struct {
	run, subTurn int
	prompt       int
	expectedHit  int
	actualHit    int
	residual     int
}

var kimiObservations = []kimiObservation{
	{1, 2, 4120, 3712, 3584, -128},
	{1, 3, 4282, 4224, 4096, -128},
	{1, 4, 4399, 4352, 4096, -256},
	{2, 2, 4100, 3712, 3584, -128},
	{2, 3, 4498, 4224, 4096, -128},
	{2, 4, 4600, 4480, 4352, -128},
	{3, 2, 4088, 3712, 3584, -128},
	{3, 3, 4462, 4352, 3840, -512},
	{3, 4, 4749, 4480, 4352, -128},
	{3, 5, 5120, 4992, 4608, -384},
	{3, 6, 5448, 5120, 5120, 0},
	{3, 7, 5726, 5504, 5376, -128},
	{3, 8, 5828, 5632, 5632, 0},
}

// TestKimiThirteenObservedSubTurnsDoNotChurn replays every one of the
// thirteen live kimi-k3 sub-turns from docs/OBSERVED.md through the
// detector: none may report churn. This is the whole point of the widened
// Kimi tolerance — with the old 127-token slack, eleven of the thirteen
// would have fired (a residual of −128 alone exceeds it), which is the
// churn noise the detector actually reported on live sessions. Each row is
// replayed as its own fresh session: a priming observation whose cacheable
// tokens are exactly the predicted hit, then the measured sub-turn.
func TestKimiThirteenObservedSubTurnsDoNotChurn(t *testing.T) {
	for _, tc := range kimiObservations {
		name := fmt.Sprintf("run%d/subturn%d", tc.run, tc.subTurn)
		t.Run(name, func(t *testing.T) {
			d := NewDetector()
			first := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi")}
			// Prime so that floor(prevCacheableTokens/128)*128 reproduces
			// the row's predicted hit exactly: a cacheable total of
			// expectedHit tokens with nothing appended yet.
			d.Observe(first, kimiSplit(wire.Usage{PromptTokens: tc.expectedHit, CachedTokens: tc.expectedHit}))

			second := append(append([]wire.Message{}, first...), wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("ok")})
			report := d.Observe(second, kimiSplit(wire.Usage{PromptTokens: tc.prompt, CachedTokens: tc.actualHit}))

			if report.Churned {
				t.Fatalf("observed healthy sub-turn reported as churn: %+v", report)
			}
			// The fixture's residual — how far the API undershot the
			// prediction — is the docs table's residual column; pin it so a
			// transcription error in either hit column fails here rather
			// than silently.
			if residual := tc.actualHit - tc.expectedHit; residual != tc.residual {
				t.Fatalf("residual = %d, want %d (docs/OBSERVED.md table)", residual, tc.residual)
			}
			if want := tc.prompt - tc.actualHit; report.ActualMissTokens != want {
				t.Fatalf("actual miss = %d, want %d (prompt %d - hit %d)", report.ActualMissTokens, want, tc.prompt, tc.actualHit)
			}
		})
	}
}

// TestDeepSeekToleranceBoundaryUnchanged pins that DeepSeek-shaped usage
// keeps exactly the verdicts it produced before the tolerance became
// provider-specific: the slack is still the trailing partial 128-token
// block, so a miss of expected+127 is quiet and expected+128 churns. This
// is the "no behaviour change" half of the migration, at the precise
// boundary rather than in the middle of the range where both tolerances
// would agree.
func TestDeepSeekToleranceBoundaryUnchanged(t *testing.T) {
	prime := func(d *Detector) {
		first := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi")}
		d.Observe(first, deepSeekSplit(wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000}))
	}

	second := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi"), wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("ok")}}

	// Miss of expected + 127: the full trailing block's worth, quiet.
	d := NewDetector()
	prime(d)
	report := d.Observe(second, deepSeekSplit(wire.Usage{PromptTokens: 1080, PromptCacheHitTokens: 1080 - 311, PromptCacheMissTokens: 311}))
	if report.Churned {
		t.Fatalf("miss of expected+127 must not churn on DeepSeek: %+v", report)
	}

	// Miss of expected + 128: one token past the tolerance, churned.
	d2 := NewDetector()
	prime(d2)
	report2 := d2.Observe(second, deepSeekSplit(wire.Usage{PromptTokens: 1080, PromptCacheHitTokens: 1080 - 312, PromptCacheMissTokens: 312}))
	if !report2.Churned {
		t.Fatalf("miss of expected+128 must churn on DeepSeek: %+v", report2)
	}
}

// TestKimiChurnedPrefixStillChurns is the test that matters for the widened
// tolerance: a genuinely churned Kimi prefix misses by thousands of tokens,
// not hundreds, and must still be reported — with the correct churn point —
// even though healthy Kimi sub-turns now get 512 tokens of slack. A 512
// tolerance that hid a broken prefix would be worse than the noise it
// silences.
func TestKimiChurnedPrefixStillChurns(t *testing.T) {
	first := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi"), wire.UserMessage("more")}

	for _, tc := range []struct {
		name       string
		prompt     int
		cached     int
		churnPoint int
	}{
		{"near-total miss", 5000, 0, 1},
		{"head-only hit", 5100, 2000, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A healthy first sub-turn, fully cached, so the detector
			// predicts almost the whole next prompt as a hit.
			d := NewDetector()
			d.Observe(first, kimiSplit(wire.Usage{PromptTokens: 5000, CachedTokens: 5000}))

			// The churned request: message 1 rewritten, so the prefix
			// collapses and only a small head (or nothing) hits.
			churned := Mutate(first, tc.churnPoint)
			report := d.Observe(churned, kimiSplit(wire.Usage{PromptTokens: tc.prompt, CachedTokens: tc.cached}))

			if !report.Churned {
				t.Fatalf("churned Kimi prefix must be reported: %+v", report)
			}
			if report.ChurnPointIndex == nil || *report.ChurnPointIndex != tc.churnPoint {
				t.Fatalf("expected churn point %d, got %v", tc.churnPoint, report.ChurnPointIndex)
			}
			// The miss is of the order a broken prefix produces: thousands
			// of tokens beyond the prediction, nowhere near the 512 slack.
			if report.ActualMissTokens < 2000 {
				t.Fatalf("churned miss = %d, want thousands of tokens", report.ActualMissTokens)
			}
		})
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

	second := append(append([]wire.Message{}, first...), wire.Message{Role: wire.RoleAssistant, Content: wire.TextContent("ok")})
	report := d.Observe(second, kimiSplit(wire.Usage{PromptTokens: 1080, CachedTokens: 896}))
	if report.Churned {
		t.Fatalf("kimi-shaped append-only growth should not churn: %+v", report)
	}
	if report.ExpectedMissTokens != 1080-896 {
		t.Fatalf("expected miss %d, got %d", 1080-896, report.ExpectedMissTokens)
	}
}
