package cache

import (
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

func TestFirstObserveHasNoChurn(t *testing.T) {
	d := NewDetector()
	messages := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi")}
	report := d.Observe(messages, wire.Usage{PromptTokens: 300, PromptCacheHitTokens: 0, PromptCacheMissTokens: 300})
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
	d.Observe(first, wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000})

	// Second request appends one message (a tool result) and the API
	// reports the full first request's floor(1000/128)*128 = 896 as a hit.
	second := append(append([]wire.Message{}, first...), wire.Message{Role: wire.RoleAssistant, Content: "ok"})
	report := d.Observe(second, wire.Usage{PromptTokens: 1080, PromptCacheHitTokens: 896, PromptCacheMissTokens: 184})
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
	d.Observe(first, wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000})

	// Second request mutates message index 1 instead of only appending —
	// the whole conversation should now report as a near-total miss.
	mutated := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi, edited"), wire.UserMessage("more")}
	report := d.Observe(mutated, wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000})
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
	report := d.Observe(healthy, wire.Usage{PromptTokens: 1050, PromptCacheHitTokens: 896, PromptCacheMissTokens: 154})
	if report.Churned {
		t.Fatalf("appending after a primed detector should not churn: %+v", report)
	}

	d2 := NewDetectorFrom(1000, priorRequest)
	mutated := []wire.Message{wire.SystemMessage("sys"), wire.UserMessage("hi, edited")}
	report2 := d2.Observe(mutated, wire.Usage{PromptTokens: 1000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 1000})
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
	d.Observe(first, wire.Usage{PromptTokens: 2000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 2000})

	churned := Mutate(first, 1)
	report := d.Observe(churned, wire.Usage{PromptTokens: 2000, PromptCacheHitTokens: 0, PromptCacheMissTokens: 2000})
	if !report.Churned {
		t.Fatal("Mutate's output should be reported as churn")
	}
	if report.ChurnPointIndex == nil || *report.ChurnPointIndex != 1 {
		t.Fatalf("expected churn point index 1 (the mutated message), got %v", report.ChurnPointIndex)
	}
}
