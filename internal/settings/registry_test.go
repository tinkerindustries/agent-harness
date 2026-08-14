package settings_test

import (
	"testing"
	"time"

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
		{settings.KeyRunMaxSubTurnsKimiK3, "100"},
		{settings.KeyRunDeadline, "1h"},
		{settings.KeyRunCompactionThreshold, "786432"},
		{settings.KeyRunCompactionThresholdKimiK3, "131072"},
		{settings.KeyRunStopGracePeriod, "30s"},
		{settings.KeyToolOutputCap, "200000"},
		{settings.KeyToolBashTimeout, "2m"},
		{settings.KeyToolBashTimeoutMax, "10m"},
		{settings.KeyToolBashWaitDelay, "2s"},
		{settings.KeyToolTimeout, "30s"},
		{settings.KeyToolWebFetchTimeout, "45s"},
		{settings.KeyToolTaskTimeout, "10m"},
		{settings.KeyToolReviewScreenshotTimeout, "60s"},
		{settings.KeyToolWebFetchMaxBody, "4194304"},
		{settings.KeyToolWebFetchMaxExtract, "40000"},
		{settings.KeyToolReviewScreenshotMaxImages, "4"},
		{settings.KeyToolReviewScreenshotMaxBytes, "5242880"},
		{settings.KeyToolAttachmentsMaxCount, "8"},
		{settings.KeyToolAttachmentsMaxBytes, "5242880"},
		{settings.KeyDefaultModel, "deepseek-v4-pro"},
		{settings.KeyDefaultFlashModel, "deepseek-v4-flash"},
		{settings.KeyDefaultEffort, "high"},
		// The judge defaults to kimi-k3, not model.default: the judge runs on
		// the provider's account, and K3 is the outside-the-family judge the
		// eval exists to get (docs/EVALS.md).
		{settings.KeyJudgeModel, "kimi-k3"},
		{settings.KeyGoogleVisionModel, "gemini-3.5-flash"},
		{settings.KeyWorkerPoolSize, "4"},
		{settings.KeyWorkerConcurrencyPro, "500"},
		{settings.KeyWorkerConcurrencyFlash, "2500"},
		{settings.KeyQueueResultsMaxAge, "168h"},
		{settings.KeyHTTPEventsLimitDefault, "500"},
		{settings.KeyHTTPEventsLimitMax, "5000"},
		// The control token defaults to empty: it is generated at startup when
		// unset, which is run control's job, not the registry's.
		{settings.KeyHTTPControlToken, ""},
		// The operator name defaults to empty: unset means runs from the web
		// UI are recorded as started by an unnamed person (D7).
		{settings.KeyIdentityOperator, ""},
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
	if got := tools.DefaultBashWaitDelay; got != 2*time.Second {
		t.Errorf("tools.DefaultBashWaitDelay = %s, want 2s", got)
	}
	if got := session.DefaultMaxSubTurns; got != 400 {
		t.Errorf("session.DefaultMaxSubTurns = %d, want 400", got)
	}
	if got := session.CompactionThresholdTokens; got != 768*1024 {
		t.Errorf("session.CompactionThresholdTokens = %d, want 786432", got)
	}
	// The per-model constants are pinned the same way: kimi-k3's own
	// ceilings live in both the registry and the session package's nil-path
	// fallback, and the two must not drift.
	if got := session.KimiK3MaxSubTurns; got != 100 {
		t.Errorf("session.KimiK3MaxSubTurns = %d, want 100", got)
	}
	if got := session.KimiK3CompactionThresholdTokens; got != 128*1024 {
		t.Errorf("session.KimiK3CompactionThresholdTokens = %d, want 131072", got)
	}
}

// TestRunBudgetKeysForModel pins the model→override-key table: kimi-k3 has
// per-model run budget keys, every other model resolves the global keys.
func TestRunBudgetKeysForModel(t *testing.T) {
	key, _, ok := settings.RunBudgetKeysForModel("kimi-k3")
	if !ok || key != settings.KeyRunMaxSubTurnsKimiK3 {
		t.Errorf("RunBudgetKeysForModel(kimi-k3) = (%q, %v), want (%q, true)", key, ok, settings.KeyRunMaxSubTurnsKimiK3)
	}
	for _, model := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "no-such-model"} {
		if _, _, ok := settings.RunBudgetKeysForModel(model); ok {
			t.Errorf("RunBudgetKeysForModel(%q) = ok, want no override", model)
		}
	}
}
