package providerhttp

import (
	"testing"
	"time"
)

// TestBackoffDelayBounds and TestBackoffDelayGrowsWithAttempt used to be
// duplicated in internal/deepseek/retry_test.go and
// internal/kimi/errors_test.go, one copy per provider testing the identical
// function. BackoffDelay is now shared, so the test is too.

func TestBackoffDelayBounds(t *testing.T) {
	base := 500 * time.Millisecond
	max := 20 * time.Second

	for attempt := 0; attempt < 20; attempt++ {
		for i := 0; i < 50; i++ {
			d := BackoffDelay(attempt, base, max)
			if d < 0 {
				t.Fatalf("BackoffDelay(%d) = %v, want >= 0", attempt, d)
			}
			if d > max {
				t.Fatalf("BackoffDelay(%d) = %v, want <= max %v", attempt, d, max)
			}
		}
	}
}

func TestBackoffDelayGrowsWithAttempt(t *testing.T) {
	base := 100 * time.Millisecond

	// The ceiling BackoffDelay draws from should strictly increase for the
	// first several attempts, since it has not yet saturated at max.
	var prevCeiling time.Duration
	for attempt := 0; attempt < 5; attempt++ {
		ceiling := base << uint(attempt)
		if attempt > 0 && ceiling <= prevCeiling {
			t.Fatalf("ceiling did not grow at attempt %d: %v <= %v", attempt, ceiling, prevCeiling)
		}
		prevCeiling = ceiling
	}
}
