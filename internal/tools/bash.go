package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
)

type bashArgs struct {
	Command     string `json:"command"`
	TimeoutMS   int    `json:"timeout"`
	Description string `json:"description"`
}

// execBash implements Bash: foreground only, wall-clock timeout from ctx
// (set in Executor.timeoutFor), output capped and labelled on truncation
// (docs/TOOLS.md).
func execBash(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args bashArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.Command == "" {
		return errorResult("command is required")
	}

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", args.Command)
	cmd.Dir = e.Workspace

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	runErr := cmd.Run()

	text, truncated := truncate(out.String(), e.outputCap())
	result := Result{Content: text, Truncated: truncated}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Content = fmt.Sprintf("command timed out\n\npartial output:\n%s", text)
		result.IsError = true
		return result
	}

	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.Content = fmt.Sprintf("%s\n\n[exit code %d]", text, exitErr.ExitCode())
		result.IsError = true
		return result
	}
	if runErr != nil {
		return errorResult("run command: %v", runErr)
	}
	if result.Content == "" {
		result.Content = "(no output)"
	}
	return result
}
