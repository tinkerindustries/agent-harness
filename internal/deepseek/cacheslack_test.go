package deepseek

import "testing"

// TestCacheSlackIsOneBlock pins DeepSeek's tolerance at one 128-token block
// less one, the value the churn detector shipped with (docs/CACHE.md). The
// detector's own tests use a fixture, so without this nothing ties them to
// the real client.
func TestCacheSlackIsOneBlock(t *testing.T) {
	if got := (&Client{}).CacheSlack(); got != 127 {
		t.Fatalf("CacheSlack() = %d, want 127 (docs/CACHE.md)", got)
	}
}
