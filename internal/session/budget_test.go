package session

import (
	"context"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/settings"
)

// fakeSettingsStore is a settings.Store backed by a map, so the budget
// resolution tests can attach a real resolver without a database (the same
// shape internal/tools uses).
type fakeSettingsStore struct {
	values map[string]string
}

func (f *fakeSettingsStore) Setting(ctx context.Context, key string) (string, bool, error) {
	v, ok := f.values[key]
	return v, ok, nil
}

func (f *fakeSettingsStore) SetSetting(ctx context.Context, key, value string) error {
	f.values[key] = value
	return nil
}

func (f *fakeSettingsStore) DeleteSetting(ctx context.Context, key string) error {
	delete(f.values, key)
	return nil
}

func budgetRunner(store *fakeSettingsStore) *Runner {
	r := &Runner{}
	if store != nil {
		r.Settings = settings.NewResolver(store)
	}
	return r
}

// TestPerModelRunBudgetResolution pins the resolution order for both budget
// keys: kimi-k3 resolves its own ceilings (the registry defaults when
// nothing is stored), every other model — known or not — resolves the
// global values, and a stored per-model value beats the model's default
// while never leaking onto other models.
func TestPerModelRunBudgetResolution(t *testing.T) {
	ctx := context.Background()

	// Nothing stored: kimi-k3 gets its own registry defaults, the rest get
	// the global ones.
	r := budgetRunner(&fakeSettingsStore{values: map[string]string{}})
	if got := r.maxSubTurns(ctx, "kimi-k3"); got != KimiK3MaxSubTurns {
		t.Errorf("maxSubTurns(kimi-k3) with nothing stored = %d, want %d", got, KimiK3MaxSubTurns)
	}
	if got := r.maxSubTurns(ctx, "deepseek-v4-pro"); got != DefaultMaxSubTurns {
		t.Errorf("maxSubTurns(deepseek-v4-pro) with nothing stored = %d, want %d", got, DefaultMaxSubTurns)
	}
	// A model with no override — including one nobody has heard of —
	// resolves the global default, unchanged.
	if got := r.maxSubTurns(ctx, "no-such-model"); got != DefaultMaxSubTurns {
		t.Errorf("maxSubTurns(no-such-model) with nothing stored = %d, want %d", got, DefaultMaxSubTurns)
	}
	if got := r.compactionThreshold(ctx, "kimi-k3"); got != KimiK3CompactionThresholdTokens {
		t.Errorf("compactionThreshold(kimi-k3) with nothing stored = %d, want %d", got, KimiK3CompactionThresholdTokens)
	}
	if got := r.compactionThreshold(ctx, "deepseek-v4-pro"); got != CompactionThresholdTokens {
		t.Errorf("compactionThreshold(deepseek-v4-pro) with nothing stored = %d, want %d", got, CompactionThresholdTokens)
	}
	if got := r.compactionThreshold(ctx, "no-such-model"); got != CompactionThresholdTokens {
		t.Errorf("compactionThreshold(no-such-model) with nothing stored = %d, want %d", got, CompactionThresholdTokens)
	}

	// A stored per-model value applies to kimi-k3 and to nobody else, and a
	// stored global value applies to everyone without an override.
	r = budgetRunner(&fakeSettingsStore{values: map[string]string{
		settings.KeyRunMaxSubTurnsKimiK3:         "50",
		settings.KeyRunCompactionThresholdKimiK3: "65536",
		settings.KeyRunMaxSubTurns:               "300",
		settings.KeyRunCompactionThreshold:       "524288",
	}})
	if got := r.maxSubTurns(ctx, "kimi-k3"); got != 50 {
		t.Errorf("maxSubTurns(kimi-k3) with per-model value = %d, want 50", got)
	}
	if got := r.maxSubTurns(ctx, "deepseek-v4-pro"); got != 300 {
		t.Errorf("maxSubTurns(deepseek-v4-pro) with global value = %d, want 300", got)
	}
	if got := r.maxSubTurns(ctx, "no-such-model"); got != 300 {
		t.Errorf("maxSubTurns(no-such-model) with global value = %d, want 300", got)
	}
	if got := r.compactionThreshold(ctx, "kimi-k3"); got != 65536 {
		t.Errorf("compactionThreshold(kimi-k3) with per-model value = %d, want 65536", got)
	}
	if got := r.compactionThreshold(ctx, "deepseek-v4-pro"); got != 524288 {
		t.Errorf("compactionThreshold(deepseek-v4-pro) with global value = %d, want 524288", got)
	}

	// The Runner-level fields keep their precedence: an explicit override
	// wins for every model, exactly as before the per-model resolution
	// existed.
	r = budgetRunner(&fakeSettingsStore{values: map[string]string{
		settings.KeyRunMaxSubTurnsKimiK3: "50",
	}})
	r.MaxSubTurns = 40
	r.CompactionThresholdTokens = 500
	if got := r.maxSubTurns(ctx, "kimi-k3"); got != 40 {
		t.Errorf("maxSubTurns(kimi-k3) with Runner override = %d, want 40", got)
	}
	if got := r.maxSubTurns(ctx, "deepseek-v4-pro"); got != 40 {
		t.Errorf("maxSubTurns(deepseek-v4-pro) with Runner override = %d, want 40", got)
	}
	if got := r.compactionThreshold(ctx, "kimi-k3"); got != 500 {
		t.Errorf("compactionThreshold(kimi-k3) with Runner override = %d, want 500", got)
	}

	// Nil settings resolver (the test path): the per-model constants still
	// apply — the registry defaults are pinned equal to them by
	// internal/settings/registry_test.go.
	r = budgetRunner(nil)
	if got := r.maxSubTurns(ctx, "kimi-k3"); got != KimiK3MaxSubTurns {
		t.Errorf("maxSubTurns(kimi-k3) with nil resolver = %d, want %d", got, KimiK3MaxSubTurns)
	}
	if got := r.maxSubTurns(ctx, "deepseek-v4-pro"); got != DefaultMaxSubTurns {
		t.Errorf("maxSubTurns(deepseek-v4-pro) with nil resolver = %d, want %d", got, DefaultMaxSubTurns)
	}
	if got := r.compactionThreshold(ctx, "kimi-k3"); got != KimiK3CompactionThresholdTokens {
		t.Errorf("compactionThreshold(kimi-k3) with nil resolver = %d, want %d", got, KimiK3CompactionThresholdTokens)
	}
	if got := r.compactionThreshold(ctx, "deepseek-v4-pro"); got != CompactionThresholdTokens {
		t.Errorf("compactionThreshold(deepseek-v4-pro) with nil resolver = %d, want %d", got, CompactionThresholdTokens)
	}
}
