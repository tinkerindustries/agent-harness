package tools

import "testing"

// TestMCPAccessToolsAreInTheDispatchTable covers the registry entry each of
// the fixed MCP tools needs: a definition the model is offered but no
// toolFuncs entry is a name the executor answers "unknown tool" to, which
// reads to the model as a tool that does not exist. The count check is what
// fails when a tool joins the slice and MCPAccessToolCount is not moved with
// it.
func TestMCPAccessToolsAreInTheDispatchTable(t *testing.T) {
	if len(mcpAccessTools) != MCPAccessToolCount {
		t.Fatalf("mcpAccessTools carries %d tools, MCPAccessToolCount says %d", len(mcpAccessTools), MCPAccessToolCount)
	}
	for _, def := range mcpAccessTools {
		name := def.Function.Name
		if _, ok := toolFuncs[name]; !ok {
			t.Errorf("%s is offered to the model but has no toolFuncs entry", name)
		}
	}
	// The fifth one by name, so a rename or a drop shows up here rather than
	// only in the array a session is sent.
	found := false
	for _, def := range mcpAccessTools {
		if def.Function.Name == "list_mcp_resource_templates" {
			found = true
		}
	}
	if !found {
		t.Error("mcpAccessTools no longer carries list_mcp_resource_templates")
	}
}

func TestMCPServerOf(t *testing.T) {
	tests := []struct {
		name       string
		toolName   string
		wantServer string
		wantOK     bool
	}{
		{
			name:       "prefix present",
			toolName:   "mcp__blender__get_objects_summary",
			wantServer: "blender",
			wantOK:     true,
		},
		{
			name:     "prefix absent",
			toolName: "read",
			wantOK:   false,
		},
		{
			name:     "prefix absent, no double underscore either",
			toolName: "read_file",
			wantOK:   false,
		},
		{
			name:       "server name containing an underscore",
			toolName:   "mcp__my_server__do_thing",
			wantServer: "my_server",
			wantOK:     true,
		},
		{
			name:       "tool name containing __",
			toolName:   "mcp__blender__get__objects__summary",
			wantServer: "blender",
			wantOK:     true,
		},
		{
			name:     "prefix with nothing after it",
			toolName: "mcp__",
			wantOK:   false,
		},
		{
			name:     "prefix and server but no tool delimiter",
			toolName: "mcp__blender",
			wantOK:   false,
		},
		{
			name:     "prefix, server, delimiter, but empty tool",
			toolName: "mcp__blender__",
			wantOK:   false,
		},
		{
			name:     "empty name",
			toolName: "",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotServer, gotOK := MCPServerOf(tt.toolName)
			if gotOK != tt.wantOK {
				t.Fatalf("MCPServerOf(%q) ok = %v, want %v", tt.toolName, gotOK, tt.wantOK)
			}
			if gotOK && gotServer != tt.wantServer {
				t.Fatalf("MCPServerOf(%q) server = %q, want %q", tt.toolName, gotServer, tt.wantServer)
			}
		})
	}
}
