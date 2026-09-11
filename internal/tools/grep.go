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
	"slices"
	"sort"
	"strconv"
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

// grepMatch is one match against a file's lines, already resolved to the path
// it is reported under. startLine and endLine are the same value except for a
// multiline match spanning more than one line.
type grepMatch struct {
	path      string
	startLine int
	endLine   int
}

// grepSkipDirs are the version-control directories a Grep never searches, at
// any depth. ripgrep's --hidden lifts its own dot-directory skipping, so
// ripgrepArgs excludes each one by name; the fallback walk skips each one by
// name. One list, because the same call has to answer the same way whichever
// path serves it.
var grepSkipDirs = []string{".git", ".svn", ".hg", ".bzr", ".jj", ".sl"}

// execGrep implements Grep: ripgrep when the session has one (Executor.RG),
// the Go walk when it does not. Defaults to files_with_matches so the model
// orients cheaply instead of pulling large content into a context that gets
// re-sent every sub-turn (docs/TOOLS.md, "Grep and Glob").
//
// The search root is resolved once, for the confinement check every tool path
// goes through, and the caller's own spelling of it is kept: ripgrep prints a
// path as the caller wrote it — "./src/x.md" stays "./src/x.md", an absolute
// path stays absolute — and a directory root reports the rest of the path
// under that spelling rather than relative to the root, which would name a
// file the model cannot open without re-prefixing it.
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

	root := e.Workspace
	spelled := ""
	if args.Path != "" {
		resolved, err := ResolvePath(e.Workspace, args.Path)
		if err != nil {
			return errorResult("%v", err)
		}
		root = resolved
		spelled = strings.TrimRight(filepath.ToSlash(args.Path), "/")
		if spelled == "" {
			spelled = "/"
		}
	}
	var rootIsFile bool
	if info, err := os.Stat(root); err == nil && !info.IsDir() {
		rootIsFile = true
	}

	if e.RG != "" {
		return execRipgrep(ctx, e, args, mode, spelled)
	}
	return execGrepWalk(ctx, e, args, mode, root, spelled, rootIsFile)
}

// execGrepWalk is the Go fallback: it walks the search root and matches each
// readable text file itself. It answers a call the way execRipgrep does — the
// same labels, the same separators, the same long-line omissions
// (TestGrepFallbackMatchesRipgrep pins that) — for a machine with no ripgrep
// on it.
//
// type filtering is the one thing it cannot do. ripgrep's type names are
// ripgrep's own table of extensions, and an approximate list here would
// answer a call differently from the ripgrep path, silently. It refuses
// instead, and says which binary is missing.
func execGrepWalk(ctx context.Context, e *Executor, args grepArgs, mode, root, spelled string, rootIsFile bool) Result {
	if args.FileType != "" {
		return errorResult("type %q: filtering by file type needs ripgrep, and none was found (pass -rg PATH or set AGENT_HARNESS_RG)", args.FileType)
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

	var globRe *regexp.Regexp
	if args.Glob != "" {
		globRe, err = globToRegexp(args.Glob)
		if err != nil {
			return errorResult("invalid glob: %v", err)
		}
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
			if path != root && slices.Contains(grepSkipDirs, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		reported := reportedPath(root, spelled, rootIsFile, path)
		// ripgrep matches a --glob carrying a directory against the whole
		// path and one without against the file's name alone; trying both
		// covers the two the same way.
		if globRe != nil && !globRe.MatchString(d.Name()) && !globRe.MatchString(strings.TrimPrefix(reported, "./")) {
			return nil
		}
		lines, ok := readTextLines(path)
		if !ok {
			return nil
		}
		fileMatches := grepFile(reported, re, lines, args.Multiline, mode == "files_with_matches")
		if len(fileMatches) == 0 {
			return nil
		}
		matches = append(matches, fileMatches...)
		if mode == "content" {
			fileLines[reported] = lines
		}
		return nil
	})
	if walkErr != nil {
		return errorResult("search %s: %v", searchedName(args.Path), walkErr)
	}

	text := formatGrepMatches(matches, mode, fileLines, !rootIsFile, args.ShowLineNumbers, before, after)
	return finishGrepResult(ctx, e, text, args.HeadLimit)
}

// reportedPath is the path a match found at path is reported under: the
// search path as the caller spelled it, with the file's own path relative to
// the search root appended. A single-file search reports that file's own
// path, which is what the caller named and what it can open.
func reportedPath(root, spelled string, rootIsFile bool, path string) string {
	if rootIsFile {
		return spelled
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return filepath.ToSlash(path)
	}
	rel = filepath.ToSlash(rel)
	switch {
	case spelled == "":
		return rel
	case spelled == ".":
		return "./" + rel
	case strings.HasSuffix(spelled, "/"):
		return spelled + rel
	}
	return spelled + "/" + rel
}

// finishGrepResult applies the two caps that sit on top of whichever path
// produced the text: head_limit's first-n-lines cut, then the output byte cap.
func finishGrepResult(ctx context.Context, e *Executor, text string, headLimit int) Result {
	if headLimit > 0 {
		text = headLimitLines(text, headLimit)
	}
	out, truncated := truncate(text, e.outputCap(ctx))
	return Result{Content: out, Truncated: truncated}
}

// readTextLines reads path's lines, or reports false for a binary-looking or
// unreadable file.
//
// Lines are split on "\n" alone. bufio.ScanLines also drops a trailing
// carriage return, and ripgrep does not: a CRLF file's line is reported with
// its \r on the end by the path that runs ripgrep, so the fallback has to
// keep it to answer the same call the same way.
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
	scanner.Split(splitOnNewline)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return lines, true
}

// splitOnNewline is bufio.ScanLines without its trailing-carriage-return
// strip, so a line's bytes reach the pattern and the result exactly as they
// are in the file.
func splitOnNewline(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if i := bytes.IndexByte(data, '\n'); i >= 0 {
		return i + 1, data[:i], nil
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
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
//
// printPath is off for a content search rooted at a single file, which is
// where ripgrep prints no path either: the caller named the file, and a path
// is shown only when more than one file is searched.
func formatGrepMatches(matches []grepMatch, mode string, fileLines map[string][]string, printPath, showLineNumbers bool, before, after int) string {
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
		return formatGrepContent(matches, fileLines, printPath, showLineNumbers, before, after)
	}
	return ""
}

// formatGrepContent renders content-mode matches with -A/-B/-C context and
// -n line numbers. lastPrintedLine tracks, per file, how far the previous
// match's context already reached, so two matches whose windows overlap do
// not repeat a line, and a "--" separator marks a real gap between them.
//
// The separator appears only when context was asked for — ripgrep prints
// none in a plain content search however far apart the matches are — and a
// block opening in another file is never contiguous with the one before it,
// so it takes one whatever the line numbers say.
//
// A line that is itself part of a match is labelled as one even when an
// earlier match's context already reached it, which is how ripgrep prints
// two matches one line apart: the second is a match line, not context.
func formatGrepContent(matches []grepMatch, fileLines map[string][]string, printPath, showLineNumbers bool, before, after int) string {
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].path != matches[j].path {
			return matches[i].path < matches[j].path
		}
		return matches[i].startLine < matches[j].startLine
	})

	covered := map[string]map[int]bool{}
	for _, m := range matches {
		if covered[m.path] == nil {
			covered[m.path] = map[int]bool{}
		}
		for ln := m.startLine; ln <= m.endLine; ln++ {
			covered[m.path][ln] = true
		}
	}

	withContext := before > 0 || after > 0
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
			if withContext && lastPath != "" {
				out = append(out, "--")
			}
			lastPath = m.path
			lastPrintedLine = 0
		} else if withContext && lastPrintedLine > 0 && from > lastPrintedLine+1 {
			out = append(out, "--")
		}
		if lastPrintedLine > 0 && from <= lastPrintedLine {
			from = lastPrintedLine + 1
		}
		for ln := from; ln <= to; ln++ {
			sep := "-"
			isMatch := covered[m.path][ln]
			if isMatch {
				sep = ":"
			}
			out = append(out, grepText(m.path, lines[ln-1], ln, sep, printPath, showLineNumbers, isMatch))
		}
		if to > lastPrintedLine {
			lastPrintedLine = to
		}
	}
	return joinLines(out)
}

// grepText renders one line: the path when the caller is shown one, the line
// number when -n asked for it, then the text. A line at least
// ripgrepMaxColumns bytes long is replaced by ripgrep's own omission marker,
// so a single minified file cannot fill the result.
func grepText(path, text string, line int, sep string, printPath, showLineNumbers, isMatch bool) string {
	if len(text) >= ripgrepMaxColumns {
		text = "[Omitted long context line]"
		if isMatch {
			text = "[Omitted long matching line]"
		}
	}
	var b strings.Builder
	if printPath {
		b.WriteString(path)
		b.WriteString(sep)
	}
	if showLineNumbers {
		b.WriteString(strconv.Itoa(line))
		b.WriteString(sep)
	}
	b.WriteString(text)
	return b.String()
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
