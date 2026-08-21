package tools

import "encoding/json"

// descriptorFor builds the string a Policy matches deny patterns against
// and shows the model on denial. Bash uses the literal command, so a deny
// pattern like "git push" reads naturally; other tools use "Name arg".
//
// An MCP call has no case of its own here: it falls through to the bare
// name below, which for an MCP call is already its full
// "mcp__<server>__<tool>" name — so a deny pattern naming a server's prefix
// ("mcp__blender__") matches every tool call reaches over that prefix, and
// "-deny mcp__blender__" keeps a run off one server entirely (docs/MCP.md,
// "Permissions").
func descriptorFor(name string, argsRaw json.RawMessage) string {
	var args map[string]any
	_ = json.Unmarshal(argsRaw, &args)

	switch name {
	case "Bash":
		if cmd, ok := args["command"].(string); ok {
			return cmd
		}
	case "Read", "Write", "List":
		if p := stringArg(args, "file_path", "path"); p != "" {
			return name + " " + p
		}
	case "Edit":
		if p, _ := args["file_path"].(string); p != "" {
			return name + " " + p
		}
	case "Glob":
		if p, _ := args["pattern"].(string); p != "" {
			return name + " " + p
		}
	case "Grep":
		if p, _ := args["pattern"].(string); p != "" {
			return name + " " + p
		}
	case "Task":
		if d, _ := args["description"].(string); d != "" {
			return name + " " + d
		}
	case "WebFetch":
		if u, _ := args["url"].(string); u != "" {
			return name + " " + u
		}
	case "MCPReadResource", "MCPGetPrompt", "MCPListResources", "MCPListPrompts":
		// The server, because these four reach a server the same way an
		// mcp__<server>__<tool> call does and an operator denying one
		// server should be able to deny it here too — and because the
		// permission gate for the two that dial reads the server back out
		// of this string (policy.go, mcpAccessServerOf), the server name
		// being unreachable from the tool name alone.
		if srv, _ := args["server"].(string); srv != "" {
			return name + " " + srv
		}
	case "Screenshot":
		// The URL, so a deny pattern can keep a session off a host the same
		// way it can for WebFetch — the browser reaches the network too.
		if u, _ := args["url"].(string); u != "" {
			return name + " " + u
		}
	case "Glance":
		if paths, ok := args["image_paths"].([]any); ok && len(paths) > 0 {
			if p, ok := paths[0].(string); ok && p != "" {
				return name + " " + p
			}
		}
	case "Ground", "Detect", "Crop", "Transcribe":
		if p := stringArg(args, "image_path"); p != "" {
			return name + " " + p
		}
	}
	return name
}

func stringArg(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
