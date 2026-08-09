package mcp

import (
	"fmt"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// permissionRank orders the three modes from least to most capable, so a
// requested mode can be compared against a ceiling. tools.Mode gates
// execution, never the tool array (docs/TOOLS.md), which is exactly the
// property that lets a ceiling clamp a request without touching anything
// DeepSeek sees.
var permissionRank = map[tools.Mode]int{
	tools.ModeReadOnly: 0,
	tools.ModeDefault:  1,
	tools.ModeFull:     2,
}

// resolvePermissionMode validates requested (the launch tool's optional
// permission_mode argument) and clamps it to ceiling. An empty requested
// value falls back to ceiling itself rather than to the harness's own
// configured default: this server is a permission ceiling, not just a
// default (docs/DESIGN.md's "Safety" section), so a caller that names
// nothing must not end up more permissive than one who explicitly asked for
// the maximum this server allows.
func resolvePermissionMode(requested string, ceiling tools.Mode) (tools.Mode, error) {
	if requested == "" {
		return ceiling, nil
	}
	mode := tools.Mode(requested)
	if _, ok := permissionRank[mode]; !ok {
		return "", fmt.Errorf("permission_mode must be readonly, default, or full, got %q", requested)
	}
	if permissionRank[mode] > permissionRank[ceiling] {
		return ceiling, nil
	}
	return mode, nil
}
