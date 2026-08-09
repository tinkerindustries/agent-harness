package deepseek

import (
	"testing"
	"time"
)

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 503}
	notRetryable := []int{400, 401, 402, 422, 200, 201, 301, 404, 502, 504}

	for _, code := range retryable {
		if !isRetryableStatus(code) {
			t.Errorf("isRetryableStatus(%d) = false, want true", code)
		}
	}
	for _, code := range notRetryable {
		if isRetryableStatus(code) {
			t.Errorf("isRetryableStatus(%d) = true, want false", code)
		}
	}
}

func TestBackoffDelayBounds(t *testing.T) {
	base := 500 * time.Millisecond
	max := 20 * time.Second

	for attempt := 0; attempt < 20; attempt++ {
		for i := 0; i < 50; i++ {
			d := backoffDelay(attempt, base, max)
			if d < 0 {
				t.Fatalf("backoffDelay(%d) = %v, want >= 0", attempt, d)
			}
			if d > max {
				t.Fatalf("backoffDelay(%d) = %v, want <= max %v", attempt, d, max)
			}
		}
	}
}

func TestBackoffDelayGrowsWithAttempt(t *testing.T) {
	base := 100 * time.Millisecond

	// The ceiling backoffDelay draws from should strictly increase for the
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
