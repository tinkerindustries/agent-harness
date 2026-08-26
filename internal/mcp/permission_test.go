package mcp

import (
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// permission_mode is mandatory. An absent value is the case that used to
// resolve to the ceiling, which made raising the ceiling for one run
// silently raise it for every caller that named nothing.
func TestResolvePermissionModeRequiresTheField(t *testing.T) {
	for _, ceiling := range []tools.Mode{tools.ModeReadOnly, tools.ModeFull} {
		_, err := resolvePermissionMode("", ceiling)
		if err == nil {
			t.Fatalf("empty permission_mode must be an error under ceiling %s", ceiling)
		}
		if !strings.Contains(err.Error(), "required") {
			t.Errorf("error should say the field is required, got: %v", err)
		}
	}
}

func TestResolvePermissionModePassesBothModesUnderAFullCeiling(t *testing.T) {
	for _, requested := range []string{"readonly", "full"} {
		got, err := resolvePermissionMode(requested, tools.ModeFull)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", requested, err)
		}
		if got != tools.Mode(requested) {
			t.Fatalf("expected %q to pass through, got %s", requested, got)
		}
	}
}

// A request above the ceiling fails rather than running at the ceiling. A
// silent downgrade would hand a caller a more restricted run than it asked
// for, and it would learn that only from the denials in its transcript.
func TestResolvePermissionModeRefusesFullAboveAReadonlyCeiling(t *testing.T) {
	_, err := resolvePermissionMode("full", tools.ModeReadOnly)
	if err == nil {
		t.Fatal("full must be refused when the ceiling is readonly")
	}
	if !strings.Contains(err.Error(), "CEILING") {
		t.Errorf("error should name the setting that refused it, got: %v", err)
	}
}

func TestResolvePermissionModeAllowsReadonlyUnderAReadonlyCeiling(t *testing.T) {
	got, err := resolvePermissionMode("readonly", tools.ModeReadOnly)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != tools.ModeReadOnly {
		t.Fatalf("expected readonly, got %s", got)
	}
}

func TestResolvePermissionModeRejectsUnknownValue(t *testing.T) {
	for _, bad := range []string{"root", "default", "Full"} {
		if _, err := resolvePermissionMode(bad, tools.ModeFull); err == nil {
			t.Errorf("expected an error for permission_mode %q", bad)
		}
	}
}
