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
	"strings"
)

type grepArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	FileType        string `json:"type"`
	OutputMode      string `json:"output_mode"`
	CaseInsensitive bool   `json:"-i"`
	ShowLineNumbers bool   `json:"-n"`
	ContextAfter    int    `json:"-A"`
	ContextBefore   int    `json:"-B"`
	Context         int    `json:"-C"`
	Multiline       bool   `json:"multiline"`
	HeadLimit       int    `json:"head_limit"`
}

// grepMatch is one match against a file's lines, already resolved to a
// workspace-relative path. startLine and endLine are the same value except
// for a multiline match spanning more than one line.
type grepMatch struct {
	path      string
	startLine int
	endLine   int
}

// grepTypeExtensions is a Go-fallback subset of ripgrep's own, much larger,
// --type-list (docs/TOOLS.md, "Grep and Glob": ripgrep acceleration is
// deferred and this package matches by extension instead). It covers the
// languages and formats this harness's own sessions actually search; an
// unrecognised type name is refused rather than silently matching nothing.
var grepTypeExtensions = map[string][]string{
	"js":     {"js", "mjs", "cjs", "jsx"},
	"ts":     {"ts", "tsx", "mts", "cts"},
	"py":     {"py", "pyi"},
	"go":     {"go"},
	"rust":   {"rs"},
	"java":   {"java"},
	"kotlin": {"kt", "kts"},
	"c":      {"c", "h"},
	"cpp":    {"cpp", "cc", "cxx", "hpp", "hh", "hxx"},
	"csharp": {"cs"},
	"ruby":   {"rb"},
	"php":    {"php"},
	"swift":  {"swift"},
	"scala":  {"scala"},
	"lua":    {"lua"},
	"sh":     {"sh", "bash", "zsh"},
	"sql":    {"sql"},
	"html":   {"html", "htm"},
	"css":    {"css", "scss", "sass", "less"},
	"vue":    {"vue"},
	"md":     {"md", "markdown"},
	"json":   {"json"},
	"yaml":   {"yaml", "yml"},
	"toml":   {"toml"},
	"xml":    {"xml"},
	"txt":    {"txt"},
	"proto":  {"proto"},
}

// execGrep implements Grep with a Go fallback (docs/TOOLS.md; ripgrep
// acceleration is deferred). Defaults to files_with_matches so the model
// orients cheaply instead of pulling large content into a context that gets
// re-sent every sub-turn.
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

	pattern := args.Pattern
	var flags string
	if args.CaseInsensitive {
		flags += "i"
	}
	if args.Multiline {
		flags += "s"
	}
	if flags != "" {
		pattern = "(?" + flags + ")" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return errorResult("invalid pattern: %v", err)
	}

	root := e.Workspace
	// fileLabel is the caller's own spelling of args.Path, set when that path
	// names a single file: a file is its own search root, and a match's path
	// is relative to the search root, so filepath.Rel of the file to itself
	// is ".". The caller's path is the one label that names something it can
	// open.
	fileLabel := ""
	if args.Path != "" {
		resolved, err := ResolvePath(e.Workspace, args.Path)
		if err != nil {
			return errorResult("%v", err)
		}
		root = resolved
		if info, err := os.Stat(resolved); err == nil && !info.IsDir() {
			fileLabel = filepath.ToSlash(filepath.Clean(args.Path))
		}
	}

	// matchPath is the path a match is reported under: relative to the search
	// root, so a directory search reports what the caller sees from there,
	// and a single-file search reports the file itself.
	matchPath := func(path string) string {
		if fileLabel != "" {
			return fileLabel
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return filepath.ToSlash(path)
		}
		return filepath.ToSlash(rel)
	}

	var globRe *regexp.Regexp
	if args.Glob != "" {
		globRe, err = globToRegexp(args.Glob)
		if err != nil {
			return errorResult("invalid glob: %v", err)
		}
	}

	var typeExts []string
	if args.FileType != "" {
		exts, ok := grepTypeExtensions[args.FileType]
		if !ok {
			return errorResult("unrecognized type %q", args.FileType)
		}
		typeExts = exts
	}

	before, after := args.ContextBefore, args.ContextAfter
	if args.Context > 0 {
		before, after = args.Context, args.Context
	}

	var matches []grepMatch
	fileLines := map[string][]string{}
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
		if typeExts != nil && !hasExtension(d.Name(), typeExts) {
			return nil
		}
		lines, ok := readTextLines(path)
		if !ok {
			return nil
		}
		rel := matchPath(path)
		fileMatches := grepFile(rel, re, lines, args.Multiline, mode == "files_with_matches")
		if len(fileMatches) == 0 {
			return nil
		}
		matches = append(matches, fileMatches...)
		if mode == "content" {
			fileLines[rel] = lines
		}
		return nil
	})
	if walkErr != nil {
		return errorResult("search %s: %v", args.Path, walkErr)
	}

	text := formatGrepMatches(matches, mode, fileLines, args.ShowLineNumbers, before, after)
	if args.HeadLimit > 0 {
		text = headLimitLines(text, args.HeadLimit)
	}
	out, truncated := truncate(text, e.outputCap(ctx))
	return Result{Content: out, Truncated: truncated}
}

// hasExtension reports whether name's extension (lowercased, without the
// leading dot) is one of exts.
func hasExtension(name string, exts []string) bool {
	got := strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	for _, e := range exts {
		if got == e {
			return true
		}
	}
	return false
}

// readTextLines reads path's lines, or reports false for a binary-looking or
// unreadable file.
func readTextLines(path string) (lines []string, ok bool) {
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

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, true
}

// grepFile matches re against lines, one line at a time by default. In
// multiline mode it matches against the whole file joined by newlines
// instead, where a match can span more than one line — the shape "." matches
// newlines and a pattern can cross lines" (docs/TOOLS.md's Grep entry)
// requires, since a per-line scan could never find such a match at all.
// stopAtFirst short-circuits once a file has one match, all
// files_with_matches needs.
func grepFile(rel string, re *regexp.Regexp, lines []string, multiline, stopAtFirst bool) []grepMatch {
	if !multiline {
		var out []grepMatch
		for i, line := range lines {
			if re.MatchString(line) {
				out = append(out, grepMatch{path: rel, startLine: i + 1, endLine: i + 1})
				if stopAtFirst {
					return out
				}
			}
		}
		return out
	}

	content := strings.Join(lines, "\n")
	var out []grepMatch
	for _, span := range re.FindAllStringIndex(content, -1) {
		start := lineOf(content, span[0])
		end := lineOf(content, span[1])
		if span[1] > span[0] && content[span[1]-1] == '\n' {
			end--
		}
		out = append(out, grepMatch{path: rel, startLine: start, endLine: end})
		if stopAtFirst {
			return out
		}
	}
	return out
}

// lineOf is the 1-based line number containing byte offset pos of content.
func lineOf(content string, pos int) int {
	return 1 + strings.Count(content[:pos], "\n")
}

// formatGrepMatches renders matches the way ripgrep's own CLI output does,
// since that is the shape a session trained against a real rg-backed
// harness expects: ":" between a match line's path, line number and text,
// "-" for a context line's, and a bare "--" between two blocks of the same
// file that are not contiguous.
func formatGrepMatches(matches []grepMatch, mode string, fileLines map[string][]string, showLineNumbers bool, before, after int) string {
	if len(matches) == 0 {
		return "no matches"
	}

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
		return joinLines(files)

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
		return joinLines(lines)

	case "content":
		return formatGrepContent(matches, fileLines, showLineNumbers, before, after)
	}
	return ""
}

// formatGrepContent renders content-mode matches with -A/-B/-C context and
// -n line numbers. lastPrintedLine tracks, per file, how far the previous
// match's context already reached, so two matches whose windows overlap do
// not repeat a line, and a "--" separator marks a real gap between them.
func formatGrepContent(matches []grepMatch, fileLines map[string][]string, showLineNumbers bool, before, after int) string {
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].path != matches[j].path {
			return matches[i].path < matches[j].path
		}
		return matches[i].startLine < matches[j].startLine
	})

	var out []string
	lastPath := ""
	lastPrintedLine := 0
	for _, m := range matches {
		lines := fileLines[m.path]
		from := m.startLine - before
		if from < 1 {
			from = 1
		}
		to := m.endLine + after
		if to > len(lines) {
			to = len(lines)
		}
		if m.path != lastPath {
			lastPrintedLine = 0
		}
		if lastPrintedLine > 0 && from > lastPrintedLine+1 {
			out = append(out, "--")
		}
		if lastPrintedLine > 0 && from <= lastPrintedLine {
			from = lastPrintedLine + 1
		}
		for ln := from; ln <= to; ln++ {
			sep := "-"
			if ln >= m.startLine && ln <= m.endLine {
				sep = ":"
			}
			if showLineNumbers {
				out = append(out, fmt.Sprintf("%s%s%d%s%s", m.path, sep, ln, sep, lines[ln-1]))
			} else {
				out = append(out, fmt.Sprintf("%s%s%s", m.path, sep, lines[ln-1]))
			}
		}
		lastPath = m.path
		if to > lastPrintedLine {
			lastPrintedLine = to
		}
	}
	return joinLines(out)
}

// headLimitLines keeps only the first n lines of text, the way head -n
// does. Applied last, after a mode's own formatting, so it works the same
// way across every output_mode: file paths for files_with_matches, matched
// lines for content, count entries for count.
func headLimitLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[:n], "\n")
}
