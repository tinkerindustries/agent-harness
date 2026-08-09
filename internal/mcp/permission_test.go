package mcp

import (
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

func TestResolvePermissionModeClampsRequestAboveCeiling(t *testing.T) {
	got, err := resolvePermissionMode("full", tools.ModeDefault)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != tools.ModeDefault {
		t.Fatalf("expected a request for full to be clamped to the default ceiling, got %s", got)
	}
}

func TestResolvePermissionModeAllowsRequestAtOrBelowCeiling(t *testing.T) {
	for _, requested := range []string{"readonly", "default"} {
		got, err := resolvePermissionMode(requested, tools.ModeDefault)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", requested, err)
		}
		if got != tools.Mode(requested) {
			t.Fatalf("expected %q to pass through unclamped, got %s", requested, got)
		}
	}
}

// TestResolvePermissionModeEmptyRequestUsesCeilingNotDefault is the safety
// requirement stated as a ceiling, not just a default: a caller who never
// names a permission_mode must not end up more permissive than this
// server's configured ceiling, whatever the harness's own default mode
// happens to be.
func TestResolvePermissionModeEmptyRequestUsesCeilingNotDefault(t *testing.T) {
	got, err := resolvePermissionMode("", tools.ModeReadOnly)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != tools.ModeReadOnly {
		t.Fatalf("expected an empty request to resolve to the ceiling (readonly), got %s", got)
	}
}

func TestResolvePermissionModeFullCeilingAllowsFull(t *testing.T) {
	got, err := resolvePermissionMode("full", tools.ModeFull)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != tools.ModeFull {
		t.Fatalf("expected full to pass through when the ceiling itself is full, got %s", got)
	}
}

func TestResolvePermissionModeRejectsUnknownValue(t *testing.T) {
	if _, err := resolvePermissionMode("root", tools.ModeFull); err == nil {
		t.Fatal("expected an error for an unrecognised permission_mode")
	}
}
