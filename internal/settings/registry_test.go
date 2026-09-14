package settings_test

import (
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/pricing"
	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
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
		{settings.KeyToolReviewScreenshotTimeout, "120s"},
		{settings.KeyToolWebFetchMaxBody, "4194304"},
		{settings.KeyToolWebFetchMaxExtract, "40000"},
		{settings.KeyToolReviewScreenshotMaxImages, "4"},
		{settings.KeyToolReviewScreenshotMaxBytes, "5242880"},
		{settings.KeyToolAttachmentsMaxCount, "8"},
		{settings.KeyToolAttachmentsMaxBytes, "5242880"},
		{settings.KeyToolMCPTimeout, "120s"},
		{settings.KeyDefaultModel, "deepseek-flash"},
		{settings.KeyDefaultFlashModel, "deepseek-flash"},
		{settings.KeyDefaultEffort, "high"},
		{settings.KeyGoogleVisionModel, "gemini-3.7-flash"},
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
	if got := tools.DefaultMCPTimeout; got != 120*time.Second {
		t.Errorf("tools.DefaultMCPTimeout = %s, want 120s", got)
	}
	if got := session.CompactionThresholdTokens; got != 768*1024 {
		t.Errorf("session.CompactionThresholdTokens = %d, want 786432", got)
	}
	// The per-model constant is pinned the same way: kimi-k3's own
	// threshold lives in both the registry and the session package's nil-path
	// fallback, and the two must not drift.
	if got := session.KimiK3CompactionThresholdTokens; got != 128*1024 {
		t.Errorf("session.KimiK3CompactionThresholdTokens = %d, want 131072", got)
	}
}

// TestCompactionKeyForModel pins the model→override-key table: kimi-k3 has
// its own compaction threshold key, every other model resolves the global key.
func TestCompactionKeyForModel(t *testing.T) {
	key, ok := settings.CompactionKeyForModel("kimi-k3")
	if !ok || key != settings.KeyRunCompactionThresholdKimiK3 {
		t.Errorf("CompactionKeyForModel(kimi-k3) = (%q, %v), want (%q, true)", key, ok, settings.KeyRunCompactionThresholdKimiK3)
	}
	for _, model := range []string{"deepseek-v4-pro", "deepseek-flash", "no-such-model"} {
		if _, ok := settings.CompactionKeyForModel(model); ok {
			t.Errorf("CompactionKeyForModel(%q) = ok, want no override", model)
		}
	}
}

// TestNoSubTurnSettings pins that the registry carries no sub-turn ceiling.
// A run ends when the model finishes, the caller cancels, or a budget that
// does not count turns stops it.
func TestNoSubTurnSettings(t *testing.T) {
	for _, key := range []string{"run.max_sub_turns", "run.max_sub_turns_kimi_k3"} {
		if _, ok := settings.Lookup(key); ok {
			t.Errorf("registry has an entry for %s", key)
		}
	}
}

// TestEveryDefaultModelIsPriced pins that every model this registry will
// reach for without being told to has an entry in the shipped price table.
//
// The invariant lives here rather than in internal/pricing because the
// registry is where the default is declared, and because internal/pricing
// depends on nothing internal (internal/CLAUDE.md) — a test there naming a
// setting would be the first thing to break that.
//
// It matters most for the vision model. Table.Cost errors on a model it has
// no entry for, and the vision tools' caller keeps zero when it does
// (internal/tools/vision.go), so a default naming an unpriced model does not
// fail loudly: the call runs, bills real money, and reports $0 for it in
// every figure the UI shows — the stat strip, the session row, the Finished
// table. Nothing else in the build connects these two files, so without this
// nothing would notice.
func TestEveryDefaultModelIsPriced(t *testing.T) {
	table, err := pricing.Load("../../configs/prices.json")
	if err != nil {
		t.Fatalf("load the shipped price table: %v", err)
	}
	for _, key := range []string{
		settings.KeyDefaultModel,
		settings.KeyDefaultFlashModel,
		settings.KeyGoogleVisionModel,
	} {
		d, ok := settings.Lookup(key)
		if !ok {
			t.Errorf("registry has no %q", key)
			continue
		}
		if _, ok := table.Models[d.Default]; !ok {
			t.Errorf("%s defaults to %q, which configs/prices.json has no entry for — its spend would silently report as $0", key, d.Default)
		}
	}
}

// TestGeminiDefaultModelMatchesTheRegistry pins the two copies of the vision
// default together. internal/gemini holds one as a constant, for the vision
// tools' (Glance, Ground, Detect) fallback path when there is no settings
// resolver to ask; this registry holds the other, which is what every
// ordinary call resolves through. gemini's own doc comment already claims
// they agree, and a claim
// in a comment is not a claim anything checks — so when they drift, the
// runs that take the fallback quietly switch to a different model, and
// those are the runs with the least context available to notice.
func TestGeminiDefaultModelMatchesTheRegistry(t *testing.T) {
	d, ok := settings.Lookup(settings.KeyGoogleVisionModel)
	if !ok {
		t.Fatalf("registry has no %q", settings.KeyGoogleVisionModel)
	}
	if gemini.DefaultModel != d.Default {
		t.Errorf("gemini.DefaultModel = %q, registry default = %q — they must agree", gemini.DefaultModel, d.Default)
	}
}
