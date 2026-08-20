package tools

import "testing"

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
			toolName: "Read",
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
