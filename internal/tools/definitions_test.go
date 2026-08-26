package tools_test

import (
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

func mcpTool(name string) wire.Tool {
	return wire.Tool{Type: "function", Function: wire.ToolFunction{Name: name}}
}

// TestWithMCPCopiesAndDoesNotCorruptTheFrozenArray is the invariant
// WithMCP's own doc comment states: two calls with different MCP slices
// must not disagree about what the frozen base array holds, in either
// direction — a naive append into the package-level array's spare capacity
// would let a second call's MCP tools bleed into the first call's result,
// or corrupt DefinitionsFor's own return value for every other caller.
func TestWithMCPCopiesAndDoesNotCorruptTheFrozenArray(t *testing.T) {
	base := tools.DefinitionsFor("deepseek-v4-pro")
	baseLen := len(base)

	first := tools.WithMCP(base, []wire.Tool{mcpTool("mcp__blender__get_objects_summary")})
	second := tools.WithMCP(base, []wire.Tool{mcpTool("mcp__chrome__navigate"), mcpTool("mcp__chrome__click")})

	// The frozen array itself must be unchanged: same length, same names, in
	// order. DefinitionsFor keeps returning the same backing array on every
	// call, so re-fetching it and comparing is what actually exercises
	// whether WithMCP wrote into it.
	again := tools.DefinitionsFor("deepseek-v4-pro")
	if len(again) != baseLen {
		t.Fatalf("the frozen array grew from %d to %d entries after WithMCP", baseLen, len(again))
	}
	for i, tool := range again {
		if tool.Function.Name != base[i].Function.Name {
			t.Fatalf("frozen array entry %d changed: got %q, want %q", i, tool.Function.Name, base[i].Function.Name)
		}
	}

	// The two results must be independent: first must carry only blender's
	// tool, second only chrome's, and neither must have picked up the
	// other's. Both also carry the four fixed MCP access tools, which sit
	// between the base array and the servers' own (docs/MCP.md,
	// "Resources") — hence the offset on every index below.
	access := tools.MCPAccessToolCount
	if len(first) != baseLen+access+1 {
		t.Fatalf("first result has %d tools, want %d (base + access + 1)", len(first), baseLen+access+1)
	}
	if len(second) != baseLen+access+2 {
		t.Fatalf("second result has %d tools, want %d (base + access + 2)", len(second), baseLen+access+2)
	}
	if got := first[baseLen+access].Function.Name; got != "mcp__blender__get_objects_summary" {
		t.Fatalf("first result's MCP tool = %q, want blender's", got)
	}
	for _, name := range []string{"mcp__chrome__navigate", "mcp__chrome__click"} {
		found := false
		for _, tool := range first {
			if tool.Function.Name == name {
				found = true
			}
		}
		if found {
			t.Fatalf("first result carries %q, which only the second call's MCP slice named", name)
		}
	}
	if got := second[baseLen+access].Function.Name; got != "mcp__chrome__navigate" {
		t.Fatalf("second result's first MCP tool = %q, want chrome's navigate", got)
	}
	if got := second[baseLen+access+1].Function.Name; got != "mcp__chrome__click" {
		t.Fatalf("second result's second MCP tool = %q, want chrome's click", got)
	}
}

// TestWithMCPDoesNotWriteIntoSpareCapacity forces the exact bug the doc
// comment warns about: base is built with spare capacity, the shape a naive
// append(base, mcp...) would grow into in place rather than reallocating.
// Appending to base afterward must not be visible through the slice WithMCP
// already returned — if it were, WithMCP wrote into base's own backing
// array instead of copying.
func TestWithMCPDoesNotWriteIntoSpareCapacity(t *testing.T) {
	base := make([]wire.Tool, 2, 5)
	base[0] = mcpTool("Read")
	base[1] = mcpTool("Write")

	access := tools.MCPAccessToolCount
	first := tools.WithMCP(base, []wire.Tool{mcpTool("mcp__blender__a")})
	// The append below reuses base's spare capacity (len 2, cap 5) exactly
	// the way an in-place WithMCP would have already written its own MCP
	// tool — so if WithMCP failed to copy, this overwrites what first[2]
	// holds.
	base = append(base, mcpTool("mcp__intruder__b"))
	_ = base

	if len(first) != 2+access+1 {
		t.Fatalf("got %d tools, want %d", len(first), 2+access+1)
	}
	if first[2+access].Function.Name != "mcp__blender__a" {
		t.Fatalf("first result's last tool = %q, want blender's — a later append to base corrupted it", first[2+access].Function.Name)
	}
}

// TestWithMCPNoServersReturnsBasePlusNothing pins the empty case: a run
// with no MCP servers configured gets an array that still starts with the
// frozen base, unaltered, and carries nothing after it.
func TestWithMCPNoServersReturnsBasePlusNothing(t *testing.T) {
	base := tools.DefinitionsFor("deepseek-v4-pro")
	got := tools.WithMCP(base, nil)
	if len(got) != len(base) {
		t.Fatalf("got %d tools, want exactly the base array's %d", len(got), len(base))
	}
	for i, tool := range got {
		if tool.Function.Name != base[i].Function.Name {
			t.Fatalf("entry %d = %q, want %q", i, tool.Function.Name, base[i].Function.Name)
		}
	}
}
