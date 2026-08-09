package tools

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type readArgs struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
}

// execRead implements Read: cat -n style output, because that is the shape
// the target harnesses return and the model reads offsets out of it
// (docs/TOOLS.md).
func execRead(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args readArgs
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

	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: %s", args.FilePath)
		}
		return errorResult("open %s: %v", args.FilePath, err)
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil && info.IsDir() {
		return errorResult("%s is a directory, not a file", args.FilePath)
	}

	start := args.Offset
	if start < 1 {
		start = 1
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 2000
	}

	var b strings.Builder
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	line := 0
	written := 0
	for scanner.Scan() {
		line++
		if line < start {
			continue
		}
		if written >= limit {
			break
		}
		fmt.Fprintf(&b, "%6d\t%s\n", line, scanner.Text())
		written++
	}
	if err := scanner.Err(); err != nil {
		return errorResult("read %s: %v", args.FilePath, err)
	}
	if written == 0 {
		if line == 0 {
			return Result{Content: "(file is empty)"}
		}
		return errorResult("offset %d is past the end of the file (%d lines)", start, line)
	}

	e.markRead(path)
	out, truncated := truncate(b.String(), e.outputCap())
	return Result{Content: out, Truncated: truncated}
}
