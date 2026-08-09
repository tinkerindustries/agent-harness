package tools

import (
	"context"
	"encoding/json"
	"strings"
)

type taskArgs struct {
	Description  string `json:"description"`
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type"`
}

// execTask implements Task by delegating to Executor.RunSubagent, which
// internal/session wires up to a nested, flash-backed session loop. The
// subagent's own transcript never joins this session's message array —
// only the text this returns does (docs/TOOLS.md).
func execTask(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args taskArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Description) == "" {
		return errorResult("description is required")
	}
	if strings.TrimSpace(args.Prompt) == "" {
		return errorResult("prompt is required")
	}
	if strings.TrimSpace(args.SubagentType) == "" {
		return errorResult("subagent_type is required")
	}
	if e.RunSubagent == nil {
		return errorResult("Task is not available in this context: no subagent runner configured")
	}

	summary, err := e.RunSubagent(ctx, args.Description, args.Prompt, args.SubagentType)
	if err != nil {
		return errorResult("subagent run failed: %v", err)
	}
	out, truncated := truncate(summary, e.outputCap())
	return Result{Content: out, Truncated: truncated}
}
