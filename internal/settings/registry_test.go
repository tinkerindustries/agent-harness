package settings_test

import (
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/session"
	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// TestRegistryDefaultsMatchTheConstantsTheyReplaced is the "no behaviour
// change with nothing set" property in test form: every limit that moved
// into the registry keeps its old compile-time constant as its default, so
// an installation that stores nothing resolves to exactly today's values.
// The consumer packages keep their exported constants as the fallback for a
// nil settings resolver (the test path); the direct comparisons below keep
// registry and fallback from drifting apart.
func TestRegistryDefaultsMatchTheConstantsTheyReplaced(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{settings.KeyRunMaxTokens, "48000"},
		{settings.KeyRunMaxSubTurns, "400"},
		{settings.KeyRunDeadline, "1h"},
		{settings.KeyRunCompactionThreshold, "786432"},
		{settings.KeyToolOutputCap, "200000"},
		{settings.KeyToolBashTimeout, "2m"},
		{settings.KeyToolBashTimeoutMax, "10m"},
		{settings.KeyToolTimeout, "30s"},
		{settings.KeyToolWebFetchTimeout, "45s"},
		{settings.KeyToolTaskTimeout, "10m"},
		{settings.KeyToolReviewScreenshotTimeout, "60s"},
		{settings.KeyToolWebFetchMaxBody, "4194304"},
		{settings.KeyToolWebFetchMaxExtract, "40000"},
		{settings.KeyToolReviewScreenshotMaxImages, "4"},
		{settings.KeyToolReviewScreenshotMaxBytes, "5242880"},
		{settings.KeyDefaultModel, "deepseek-v4-pro"},
		{settings.KeyDefaultFlashModel, "deepseek-v4-flash"},
		{settings.KeyDefaultEffort, "high"},
		{settings.KeyGoogleVisionModel, "gemini-3.5-flash"},
		{settings.KeyWorkerPoolSize, "4"},
		{settings.KeyWorkerConcurrencyPro, "500"},
		{settings.KeyWorkerConcurrencyFlash, "2500"},
		{settings.KeyQueueResultsMaxAge, "168h"},
		{settings.KeyHTTPEventsLimitDefault, "500"},
		{settings.KeyHTTPEventsLimitMax, "5000"},
	}
	for _, tc := range cases {
		d, ok := settings.Lookup(tc.key)
		if !ok {
			t.Errorf("registry has no entry for %s", tc.key)
			continue
		}
		if d.Default != tc.want {
			t.Errorf("%s default = %q, want %q", tc.key, d.Default, tc.want)
		}
	}

	// The fallback constants the consumer packages still carry for a nil
	// settings resolver must equal the registry defaults.
	if got := tools.DefaultOutputCap; got != 200_000 {
		t.Errorf("tools.DefaultOutputCap = %d, want 200000", got)
	}
	if got := session.DefaultMaxSubTurns; got != 400 {
		t.Errorf("session.DefaultMaxSubTurns = %d, want 400", got)
	}
	if got := session.CompactionThresholdTokens; got != 768*1024 {
		t.Errorf("session.CompactionThresholdTokens = %d, want 786432", got)
	}
}
