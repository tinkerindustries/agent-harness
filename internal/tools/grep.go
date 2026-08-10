package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
)

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	OutputMode string `json:"output_mode"`
}

type grepMatch struct {
	path string
	line int
	text string
}

// execGrep implements Grep with a Go fallback (docs/TOOLS.md; ripgrep
// acceleration is deferred). Defaults to
// files_with_matches so the model orients cheaply instead of pulling large
// content into a context that gets re-sent every sub-turn.
func execGrep(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args grepArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.Pattern == "" {
		return errorResult("pattern is required")
	}
	mode := args.OutputMode
	if mode == "" {
		mode = "files_with_matches"
	}
	if mode != "files_with_matches" && mode != "content" && mode != "count" {
		return errorResult("invalid output_mode %q: must be files_with_matches, content, or count", mode)
	}

	re, err := regexp.Compile(args.Pattern)
	if err != nil {
		return errorResult("invalid pattern: %v", err)
	}

	root := e.Workspace
	if args.Path != "" {
		resolved, err := ResolvePath(e.Workspace, args.Path)
		if err != nil {
			return errorResult("%v", err)
		}
		root = resolved
	}

	var globRe *regexp.Regexp
	if args.Glob != "" {
		globRe, err = globToRegexp(args.Glob)
		if err != nil {
			return errorResult("invalid glob: %v", err)
		}
	}

	var matches []grepMatch
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if path != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if globRe != nil && !globRe.MatchString(d.Name()) {
			return nil
		}
		fileMatches, ok := grepFile(path, root, re, mode == "files_with_matches")
		if !ok {
			return nil
		}
		matches = append(matches, fileMatches...)
		return nil
	})
	if walkErr != nil {
		return errorResult("search %s: %v", args.Path, walkErr)
	}

	return formatGrepMatches(matches, mode, e.outputCap(ctx))
}

// grepFile scans one file for re, returning nil, false for files it skips
// (binary-looking or unreadable). stopAtFirst short-circuits once a file
// has one match, which is all files_with_matches needs.
func grepFile(path, root string, re *regexp.Regexp, stopAtFirst bool) ([]grepMatch, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	defer f.Close()

	head := make([]byte, 8000)
	n, _ := f.Read(head)
	if bytes.IndexByte(head[:n], 0) >= 0 {
		return nil, false
	}
	if _, err := f.Seek(0, 0); err != nil {
		return nil, false
	}

	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}
	rel = filepath.ToSlash(rel)

	var out []grepMatch
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		if re.MatchString(line) {
			out = append(out, grepMatch{path: rel, line: lineNo, text: line})
			if stopAtFirst {
				return out, true
			}
		}
	}
	return out, len(out) > 0
}

func formatGrepMatches(matches []grepMatch, mode string, cap int) Result {
	if len(matches) == 0 {
		return Result{Content: "no matches"}
	}

	var text string
	switch mode {
	case "files_with_matches":
		seen := map[string]bool{}
		var files []string
		for _, m := range matches {
			if !seen[m.path] {
				seen[m.path] = true
				files = append(files, m.path)
			}
		}
		sort.Strings(files)
		text = joinLines(files)

	case "count":
		counts := map[string]int{}
		for _, m := range matches {
			counts[m.path]++
		}
		var files []string
		for f := range counts {
			files = append(files, f)
		}
		sort.Strings(files)
		lines := make([]string, len(files))
		for i, f := range files {
			lines[i] = fmt.Sprintf("%s:%d", f, counts[f])
		}
		text = joinLines(lines)

	case "content":
		sort.Slice(matches, func(i, j int) bool {
			if matches[i].path != matches[j].path {
				return matches[i].path < matches[j].path
			}
			return matches[i].line < matches[j].line
		})
		lines := make([]string, len(matches))
		for i, m := range matches {
			lines[i] = fmt.Sprintf("%s:%d:%s", m.path, m.line, m.text)
		}
		text = joinLines(lines)
	}

	out, truncated := truncate(text, cap)
	return Result{Content: out, Truncated: truncated}
}
