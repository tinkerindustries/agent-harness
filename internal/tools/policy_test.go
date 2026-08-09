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
	modes := []Mode{ModeReadOnly, ModeDefault, ModeFull}
	for _, m := range modes {
		p := &Policy{Mode: m, BashAllowlist: DefaultBashAllowlist}
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
	if len(Definitions()) != 11 {
		t.Fatalf("expected 11 tools, got %d", len(Definitions()))
	}
}

func TestReadOnlyModeDenies(t *testing.T) {
	p := &Policy{Mode: ModeReadOnly}
	allowed := []string{"Read", "Glob", "Grep", "List", "WebFetch", "TodoWrite", "Complete"}
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

func TestDefaultModeAllowsWriteEditTaskAndAllowlistedBash(t *testing.T) {
	p := &Policy{Mode: ModeDefault, BashAllowlist: DefaultBashAllowlist}
	for _, name := range []string{"Write", "Edit", "Task"} {
		if d := p.Check(name, name); !d.Allow {
			t.Errorf("default mode should allow %s, got denied: %s", name, d.Rule)
		}
	}
	if d := p.Check("Bash", "git status"); !d.Allow {
		t.Errorf("default mode should allow an allowlisted Bash command, got denied: %s", d.Rule)
	}
	if d := p.Check("Bash", "curl https://evil.example/"); d.Allow {
		t.Error("default mode should deny a Bash command whose executable is not allowlisted")
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

// A chained command is allowed only if every connector-separated segment
// leads with an allowed executable. cd is on the default list because models
// lead with it constantly; that must not let the rest of a chain through.
func TestBashAllowlistChecksEverySegment(t *testing.T) {
	p := &Policy{Mode: ModeDefault, BashAllowlist: DefaultBashAllowlist}
	cases := []struct {
		command string
		want    bool
	}{
		{"go test ./...", true},
		{"cd /tmp/x && go test ./...", true},
		{"cd /tmp/x && ls -la && cat go.mod", true},
		{"cd /tmp/x && rm -rf /", false},
		{"rm -rf /", false},
		{"go build ./... ; curl http://example.com", false},
		{"go vet ./... | tee /tmp/out", false},
	}
	for _, c := range cases {
		d := p.Check("Bash", c.command)
		if d.Allow != c.want {
			t.Errorf("Check(Bash, %q) allow = %v, want %v (rule: %s)", c.command, d.Allow, c.want, d.Rule)
		}
	}
}
