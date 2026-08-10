package tools

import "encoding/json"

// descriptorFor builds the string a Policy matches deny patterns against
// and shows the model on denial. Bash uses the literal command, so a deny
// pattern like "git push" reads naturally; other tools use "Name arg".
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
	case "ReviewScreenshot":
		if paths, ok := args["image_paths"].([]any); ok && len(paths) > 0 {
			if p, ok := paths[0].(string); ok && p != "" {
				return name + " " + p
			}
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
