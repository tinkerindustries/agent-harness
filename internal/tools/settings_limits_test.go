package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
)

// fakeSettingsStore is a settings.Store backed by a map, so a test can give
// an Executor a real settings resolver without a database.
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

// TestExecutorResolvesLimitsFromSettings pins the configurable tool limits:
// with a settings resolver attached, Glance's image count and per-file size
// caps come from the registry, and the refusal messages state the actual
// limit — the shape the model discovers a changed bound from (docs/CACHE.md:
// the tool description itself names no numbers). The keys keep the names of
// the tool they originally bounded (tools.reviewscreenshot_max_images,
// tools.reviewscreenshot_max_bytes) — vision.go, docs/VISION-TOOLKIT.md §5.
func TestExecutorResolvesLimitsFromSettings(t *testing.T) {
	res := settings.NewResolver(&fakeSettingsStore{values: map[string]string{
		settings.KeyToolReviewScreenshotMaxImages: "2",
		settings.KeyToolReviewScreenshotMaxBytes:  "1000",
	}})
	e, err := NewExecutor(t.TempDir(), &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.Settings = res

	// Three image paths against a configured cap of two.
	paths := make([]string, 3)
	for i := range paths {
		paths[i] = "shot.png"
	}
	res1 := runTool(t, e, "Glance", glanceArgs{
		ImagePaths: paths,
	})
	if !res1.IsError || !strings.Contains(res1.Content, "at most 2 images") {
		t.Fatalf("expected a refusal naming the configured cap of 2, got: %s", res1.Content)
	}

	// A 1001-byte file against a configured cap of 1000.
	if err := os.WriteFile(filepath.Join(e.Workspace, "big.png"), make([]byte, 1001), 0o644); err != nil {
		t.Fatal(err)
	}
	res2 := runTool(t, e, "Glance", glanceArgs{
		ImagePaths: []string{"big.png"},
	})
	if !res2.IsError || !strings.Contains(res2.Content, "over the 1000-byte per-file limit") {
		t.Fatalf("expected a refusal naming the configured byte cap, got: %s", res2.Content)
	}
}
