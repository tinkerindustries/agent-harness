package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/settings"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// The frozen head of every session must be byte-identical regardless of what
// a work request asked for. Concurrency only makes the prompt cache cheaper
// while the head is genuinely shared, so anything per-request that reaches
// the system prompt or the tool schema costs the whole prefix
// (docs/CACHE.md, docs/DESIGN.md §3.2).
func TestFrozenPrefixIsIdenticalAcrossRequests(t *testing.T) {
	if got, want := RenderSystemPrompt(), RenderSystemPrompt(); got != want {
		t.Fatal("RenderSystemPrompt is not stable across calls")
	}

	first, err := json.Marshal(tools.Definitions())
	if err != nil {
		t.Fatalf("encode tool schema: %v", err)
	}
	for i := 0; i < 8; i++ {
		next, err := json.Marshal(tools.Definitions())
		if err != nil {
			t.Fatalf("encode tool schema: %v", err)
		}
		if string(next) != string(first) {
			t.Fatalf("tool schema is not byte-stable across calls:\n first: %s\n  next: %s", first, next)
		}
	}
}

// Everything specific to a run belongs in the opening user message, where it
// appends rather than divides the prefix. This asserts the split holds: the
// workspace, the task, and the result schema all appear there and none of
// them appear in the system prompt.
func TestPerRequestDataStaysOutOfTheSystemPrompt(t *testing.T) {
	const (
		workspace = "/tmp/some-unlikely-workspace-path-42"
		task      = "an unlikely task string 42"
	)
	schema := json.RawMessage(`{"type":"object","properties":{"unlikelyField42":{"type":"string"}}}`)

	sys := RenderSystemPrompt()
	opening := RenderOpeningMessage(workspace, task, schema, "", "")

	for _, needle := range []string{workspace, task, "unlikelyField42"} {
		if !strings.Contains(opening, needle) {
			t.Errorf("opening message should carry %q, it does not", needle)
		}
		if strings.Contains(sys, needle) {
			t.Errorf("system prompt carries per-request value %q; it must stay in the opening message", needle)
		}
	}
}

// The system prompt tells the model where scratch output belongs: a scratch/
// directory at the workspace root, never /tmp (shared across concurrent
// sessions in the container) and never inside a cloned repository. Asserted by
// content, not exact wording, so a future rewording does not make the test
// brittle — it must keep naming scratch/ and ruling out /tmp.
func TestSystemPromptDirectsScratchFilesToScratchDirectory(t *testing.T) {
	sys := RenderSystemPrompt()
	for _, needle := range []string{"scratch/", "/tmp"} {
		if !strings.Contains(sys, needle) {
			t.Errorf("system prompt should mention %q, it does not", needle)
		}
	}
}

// fakeSettingStore is a settings.Store backed by a map, enough for the
// frozen-head test below to attach a real resolver.
type fakeSettingStore struct {
	values map[string]string
}

func (f *fakeSettingStore) Setting(ctx context.Context, key string) (string, bool, error) {
	v, ok := f.values[key]
	return v, ok, nil
}
func (f *fakeSettingStore) SetSetting(ctx context.Context, key, value string) error {
	f.values[key] = value
	return nil
}
func (f *fakeSettingStore) DeleteSetting(ctx context.Context, key string) error {
	delete(f.values, key)
	return nil
}

// TestToolSchemaDoesNotVaryWithSettings pins that a configurable tool limit
// never reaches the tool array: changing tools.reviewscreenshot_max_images
// through the settings table leaves the marshalled schema byte-identical.
// The tool description quotes no numbers — the model discovers a changed
// bound from the refusal message instead (docs/CACHE.md, "Never quote a
// configurable limit in a tool description").
func TestToolSchemaDoesNotVaryWithSettings(t *testing.T) {
	marshal := func() string {
		b, err := json.Marshal(tools.Definitions())
		if err != nil {
			t.Fatalf("encode tool schema: %v", err)
		}
		return string(b)
	}

	before := marshal()
	if strings.Contains(before, "at most 4") || strings.Contains(before, "5 MB") {
		t.Fatalf("tool schema quotes a configurable limit:\n%s", before)
	}

	res := settings.NewResolver(&fakeSettingStore{values: map[string]string{}})
	if err := res.Set(context.Background(), settings.KeyToolReviewScreenshotMaxImages, "2"); err != nil {
		t.Fatalf("set max images: %v", err)
	}
	if err := res.Set(context.Background(), settings.KeyToolReviewScreenshotMaxBytes, "1024"); err != nil {
		t.Fatalf("set max bytes: %v", err)
	}

	if after := marshal(); after != before {
		t.Fatalf("tool schema varies with settings:\n before: %s\n  after: %s", before, after)
	}
}
