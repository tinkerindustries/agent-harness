package mcp

import (
	"errors"
	"fmt"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// resolvePermissionMode validates the launch tool's permission_mode
// argument against ceiling. The argument is mandatory: there is no fallback
// value, so raising or lowering the ceiling cannot change what a caller who
// named nothing ends up with.
//
// A request above the ceiling is refused rather than quietly lowered. A
// caller therefore gets either the mode it asked for or an error naming the
// limit, never a run that is more restricted than it believes.
func resolvePermissionMode(requested string, ceiling tools.Mode) (tools.Mode, error) {
	if requested == "" {
		return "", errors.New("permission_mode is required: readonly or full")
	}
	mode := tools.Mode(requested)
	if !mode.Valid() {
		return "", fmt.Errorf("permission_mode must be readonly or full, got %q", requested)
	}
	if mode == tools.ModeFull && ceiling == tools.ModeReadOnly {
		return "", errors.New("permission_mode full is refused: this server's DEEPSEEK_MCP_PERMISSION_CEILING is readonly")
	}
	return mode, nil
}
