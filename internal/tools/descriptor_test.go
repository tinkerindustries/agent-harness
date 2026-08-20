package tools

import "testing"

// TestDescriptorForMCPCallIsTheFullPrefixedName pins the comment in
// descriptor.go: an MCP call carries no case of its own in the switch, so
// its descriptor falls through to the bare tool name — which, for an MCP
// call, is already the full "mcp__<server>__<tool>" name Definitions
// offered the model. That is what lets "-deny mcp__blender__" keep a run
// off one server entirely (docs/MCP.md, "Permissions").
func TestDescriptorForMCPCallIsTheFullPrefixedName(t *testing.T) {
	const name = "mcp__blender__get_objects_summary"
	got := descriptorFor(name, []byte(`{"foo":"bar"}`))
	if got != name {
		t.Fatalf("descriptorFor(%q) = %q, want the name unchanged", name, got)
	}
}
