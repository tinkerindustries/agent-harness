package tools

import (
	"context"
	"encoding/json"
	"os"
	"regexp"
	"sort"
)

type listArgs struct {
	Path   string   `json:"path"`
	Ignore []string `json:"ignore"`
}

// execList implements List: the immediate entries of a directory, files and
// subdirectories both, sorted, honouring glob ignore patterns.
func execList(_ context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args listArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.Path == "" {
		return errorResult("path is required")
	}
	path, err := ResolvePath(e.Workspace, args.Path)
	if err != nil {
		return errorResult("%v", err)
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("directory not found: %s", args.Path)
		}
		return errorResult("list %s: %v", args.Path, err)
	}

	var ignoreRes []*regexp.Regexp
	for _, pattern := range args.Ignore {
		re, err := globToRegexp(pattern)
		if err != nil {
			return errorResult("invalid ignore pattern %q: %v", pattern, err)
		}
		ignoreRes = append(ignoreRes, re)
	}

	var names []string
	for _, entry := range entries {
		name := entry.Name()
		ignored := false
		for _, re := range ignoreRes {
			if re.MatchString(name) {
				ignored = true
				break
			}
		}
		if ignored {
			continue
		}
		if entry.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)

	if len(names) == 0 {
		return Result{Content: "(empty directory)"}
	}
	out, truncated := truncate(joinLines(names), e.outputCap())
	return Result{Content: out, Truncated: truncated}
}
