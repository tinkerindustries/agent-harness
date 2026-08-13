package tools

import (
	"encoding/json"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
)

// TestToolArrayIdenticalAcrossModes is the invariant docs/CACHE.md names
// directly: permission modes gate execution, never the tool array. If this
// ever fails, every mode switch becomes a cold cache.
func TestToolArrayIdenticalAcrossModes(t *testing.T) {
	readonly, err := json.Marshal(Definitions())
	if err != nil {
		t.Fatal(err)
	}
	// Definitions() takes no mode argument at all, so the real assertion is
	// that nothing in the package ever branches its return value on mode;
	// re-marshalling after exercising every mode's Check path below proves
	// evaluating a policy has no observable effect on it.
	modes := []Mode{ModeReadOnly, ModeFull}
	for _, m := range modes {
		p := &Policy{Mode: m}
		for _, tool := range Definitions() {
			p.Check(tool.Function.Name, tool.Function.Name)
		}
	}
	after, err := json.Marshal(Definitions())
	if err != nil {
		t.Fatal(err)
	}
	if string(readonly) != string(after) {
		t.Fatal("tool array changed after exercising permission checks across modes")
	}
	if len(Definitions()) != 16 {
		t.Fatalf("expected 16 tools, got %d", len(Definitions()))
	}
}

func TestReadOnlyModeDenies(t *testing.T) {
	p := &Policy{Mode: ModeReadOnly}
	allowed := []string{"Read", "Glob", "Grep", "List", "WebFetch", "ReviewScreenshot", "Screenshot", "TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "Complete"}
	for _, name := range allowed {
		if d := p.Check(name, name); !d.Allow {
			t.Errorf("readonly mode should allow %s, got denied: %s", name, d.Rule)
		}
	}
	denied := []string{"Write", "Edit", "Bash", "Task"}
	for _, name := range denied {
		if d := p.Check(name, name); d.Allow {
			t.Errorf("readonly mode should deny %s", name)
		}
	}
}

// Mode has exactly two values. "default" was a third, gating Bash behind an
// allowlist; it was removed because that allowlist admitted go, npm, make,
// and python, every one of which runs arbitrary code (docs/TOOLS.md).
func TestModeValidAcceptsTwoModesAndRejectsRemovedDefault(t *testing.T) {
	if !ModeReadOnly.Valid() || !ModeFull.Valid() {
		t.Fatal("readonly and full must both be valid modes")
	}
	for _, bad := range []Mode{"", "default", "Full", "readwrite"} {
		if bad.Valid() {
			t.Errorf("Mode(%q).Valid() = true, want false", bad)
		}
	}
}

func TestFullModeAllowsEverything(t *testing.T) {
	p := &Policy{Mode: ModeFull}
	for _, tool := range Definitions() {
		if d := p.Check(tool.Function.Name, tool.Function.Name); !d.Allow {
			t.Errorf("full mode should allow %s, got denied: %s", tool.Function.Name, d.Rule)
		}
	}
	if d := p.Check("Bash", "anything at all"); !d.Allow {
		t.Error("full mode should allow an arbitrary Bash command")
	}
}

func TestDenyPatternsSubtractFromMode(t *testing.T) {
	p := &Policy{Mode: ModeFull, Deny: []string{"git push"}}
	if d := p.Check("Bash", "git push origin main"); d.Allow {
		t.Error("a deny pattern should refuse a call full mode would otherwise allow")
	}
	if d := p.Check("Bash", "git status"); !d.Allow {
		t.Error("a deny pattern should not affect unrelated commands")
	}
}

func TestResolverOverridesDenial(t *testing.T) {
	p := &Policy{Mode: ModeReadOnly, Resolver: func(tool, descriptor string) bool { return true }}
	d := p.Check("Bash", "git status")
	if !d.Allow {
		t.Fatal("resolver approving a call should allow it")
	}
	if d.Rule != "approved interactively" {
		t.Fatalf("unexpected rule: %s", d.Rule)
	}
}

func TestResolverDeclineKeepsDenial(t *testing.T) {
	p := &Policy{Mode: ModeReadOnly, Resolver: func(tool, descriptor string) bool { return false }}
	if d := p.Check("Bash", "git status"); d.Allow {
		t.Fatal("resolver declining a call should keep it denied")
	}
}

func TestExecuteDenialProducesReadableToolResult(t *testing.T) {
	e, err := NewExecutor(t.TempDir(), &Policy{Mode: ModeReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	call := deepseek.ToolCall{
		ID:   "call_00_x",
		Type: "function",
		Function: deepseek.ToolCallFunc{
			Name:      "Bash",
			Arguments: `{"command":"ls"}`,
		},
	}
	outcome := e.Execute(t.Context(), call)
	if !outcome.Denied {
		t.Fatal("expected the call to be denied")
	}
	if outcome.Result.Content == "" || !outcome.Result.IsError {
		t.Fatalf("expected a readable, error-flagged tool result, got %+v", outcome.Result)
	}
}
