package tools

import (
	"context"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"sort"
)

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

// skipDirs are noise directories excluded from Glob and Grep walks.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, ".hg": true, ".svn": true,
}

// execGlob implements Glob: path matching by pattern, walked from path or
// the workspace root.
func execGlob(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args globArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.Pattern == "" {
		return errorResult("pattern is required")
	}

	root := e.Workspace
	if args.Path != "" {
		resolved, err := ResolvePath(e.Workspace, args.Path)
		if err != nil {
			return errorResult("%v", err)
		}
		root = resolved
	}

	re, err := globToRegexp(args.Pattern)
	if err != nil {
		return errorResult("invalid pattern: %v", err)
	}

	var matches []string
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if re.MatchString(rel) {
			matches = append(matches, rel)
		}
		return nil
	})
	if walkErr != nil {
		return errorResult("walk %s: %v", args.Path, walkErr)
	}
	sort.Strings(matches)

	if len(matches) == 0 {
		return Result{Content: "no files matched"}
	}
	out, truncated := truncate(joinLines(matches), e.outputCap(ctx))
	return Result{Content: out, Truncated: truncated}
}

func joinLines(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}
