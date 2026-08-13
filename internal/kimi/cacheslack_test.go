package kimi

import "testing"

// TestCacheSlackMatchesTheMeasurement pins the tolerance the churn detector
// is given for Kimi. 512 is the largest over-prediction measured across
// thirteen sub-turns of three live sessions (docs/OBSERVED.md); it is an
// empirical bound, not a property of Kimi's cache. The detector's own tests
// use a fixture, so without this nothing ties them to the real client.
func TestCacheSlackMatchesTheMeasurement(t *testing.T) {
	if got := (&Client{}).CacheSlack(); got != 512 {
		t.Fatalf("CacheSlack() = %d, want 512 (docs/OBSERVED.md)", got)
	}
}
