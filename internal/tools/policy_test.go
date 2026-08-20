package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
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
	if len(Definitions()) != 20 {
		t.Fatalf("expected 20 tools, got %d", len(Definitions()))
	}
}

func TestReadOnlyModeDenies(t *testing.T) {
	p := &Policy{Mode: ModeReadOnly}
	// Crop is in the allowed set despite writing a file: its output is
	// confined to scratch/ exactly as Screenshot's is
	// (resolveScratchImageOutput), so it cannot reach the deliverable or a
	// cloned repository. Denying it put the whole Ground-Crop-Glance pipeline
	// behind full permissions, the mode that also carries the host docker
	// socket.
	allowed := []string{"Read", "Glob", "Grep", "List", "WebFetch", "Glance", "Ground", "Detect", "Screenshot", "Crop", "TaskCreate", "TaskGet", "TaskList", "TaskUpdate", "Complete"}
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

// TestMCPPermissionTable pins the four combinations docs/MCP.md's
// "Permissions" table names: full always allows an MCP call regardless of
// the server's own read-only allowance, readonly denies it unless the
// server was probed with allow_readonly set, and a denial names the server
// so the model can tell which one to route around.
func TestMCPPermissionTable(t *testing.T) {
	const tool = "mcp__blender__get_objects_summary"

	full := &Policy{Mode: ModeFull, MCPReadOnlyServers: map[string]bool{"blender": false}}
	if d := full.Check(tool, tool); !d.Allow {
		t.Errorf("full mode should allow an MCP call from a non-read-only server, got denied: %s", d.Rule)
	}

	fullReadOnlyServer := &Policy{Mode: ModeFull, MCPReadOnlyServers: map[string]bool{"blender": true}}
	if d := fullReadOnlyServer.Check(tool, tool); !d.Allow {
		t.Errorf("full mode should allow an MCP call from a read-only server too, got denied: %s", d.Rule)
	}

	readOnlyDenied := &Policy{Mode: ModeReadOnly, MCPReadOnlyServers: map[string]bool{"blender": false}}
	if d := readOnlyDenied.Check(tool, tool); d.Allow {
		t.Error("readonly mode should deny an MCP call from a server not marked read-only")
	} else if !strings.Contains(d.Rule, "blender") {
		t.Errorf("denial rule should name the server, got: %s", d.Rule)
	}

	// A server absent from the map at all — no MCP configured, or one this
	// policy's snapshot never saw — behaves the same as false.
	readOnlyAbsent := &Policy{Mode: ModeReadOnly}
	if d := readOnlyAbsent.Check(tool, tool); d.Allow {
		t.Error("readonly mode should deny an MCP call whose server is entirely absent from the map")
	}

	readOnlyAllowed := &Policy{Mode: ModeReadOnly, MCPReadOnlyServers: map[string]bool{"blender": true}}
	if d := readOnlyAllowed.Check(tool, tool); !d.Allow {
		t.Errorf("readonly mode should allow an MCP call from a server marked read-only, got denied: %s", d.Rule)
	}
}

// TestMCPDenyPatternMatchesFullPrefixedName pins the descriptor contract
// for MCP calls (docs/MCP.md, "Deny patterns match against the descriptor
// exactly as they do for built-in tools; the descriptor for an MCP call is
// its full prefixed name"): "-deny mcp__blender__" keeps a full-mode
// session off every tool that server offers, without touching another
// server's tools.
func TestMCPDenyPatternMatchesFullPrefixedName(t *testing.T) {
	p := &Policy{Mode: ModeFull, Deny: []string{"mcp__blender__"}}
	if d := p.Check("mcp__blender__get_objects_summary", "mcp__blender__get_objects_summary"); d.Allow {
		t.Error("a deny pattern naming the server prefix should refuse every one of its tools")
	}
	if d := p.Check("mcp__chrome__navigate", "mcp__chrome__navigate"); !d.Allow {
		t.Errorf("a deny pattern for one server must not affect another server's tools, got denied: %s", d.Rule)
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
	call := wire.ToolCall{
		ID:   "call_00_x",
		Type: "function",
		Function: wire.ToolCallFunc{
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
