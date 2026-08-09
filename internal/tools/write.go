package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
)

type writeArgs struct {
	FilePath string `json:"file_path"`
	Content  string `json:"content"`
}

// execWrite implements Write. Overwriting an existing file requires a prior
// Read of it this session, so the model edits real bytes rather than
// remembered ones; creating a new file needs no prior read
// (docs/TOOLS.md).
func execWrite(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args writeArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.FilePath == "" {
		return errorResult("file_path is required")
	}
	path, err := ResolvePath(e.Workspace, args.FilePath)
	if err != nil {
		return errorResult("%v", err)
	}

	existed := false
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			return errorResult("%s is a directory, not a file", args.FilePath)
		}
		existed = true
	}
	if existed && !e.wasRead(path) {
		return errorResult("%s exists and has not been read in this session; Read it before overwriting", args.FilePath)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errorResult("create directory for %s: %v", args.FilePath, err)
	}
	if err := os.WriteFile(path, []byte(args.Content), 0o644); err != nil {
		return errorResult("write %s: %v", args.FilePath, err)
	}
	e.markRead(path)

	verb := "Created"
	if existed {
		verb = "Overwrote"
	}
	return Result{Content: verb + " " + args.FilePath}
}
