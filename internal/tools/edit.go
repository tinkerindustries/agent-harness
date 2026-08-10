package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

type editArgs struct {
	FilePath   string `json:"file_path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// lineNumberPrefix matches the "  12\t" style prefix Read prepends to every
// line. old_string must be the file's raw bytes, not a copy-paste of Read's
// display output (docs/TOOLS.md).
var lineNumberPrefix = regexp.MustCompile(`(?m)^\s*[0-9]+\t`)

// execEdit implements Edit: exact-match replacement, requiring a prior Read
// and a unique match unless replace_all is set (docs/TOOLS.md).
func execEdit(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args editArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if args.FilePath == "" {
		return errorResult("file_path is required")
	}
	if args.OldString == "" {
		return errorResult("old_string is required and must be non-empty")
	}
	if args.OldString == args.NewString {
		return errorResult("old_string and new_string are identical; nothing to change")
	}
	if lineNumberPrefix.MatchString(args.OldString) {
		return errorResult("old_string looks like it includes Read's line-number prefix (e.g. \"   12\\t\"). " +
			"That prefix is display only. Copy the text after the tab, not the numbered listing.")
	}

	path, err := ResolvePath(e.Workspace, args.FilePath)
	if err != nil {
		return errorResult("%v", err)
	}
	size, binary, err := isBinaryFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: %s", args.FilePath)
		}
		return errorResult("open %s: %v", args.FilePath, err)
	}
	if binary {
		return binaryFileError(args.FilePath, size)
	}
	if !e.wasRead(path) {
		return errorResult("%s has not been read in this session; Read it before editing", args.FilePath)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return errorResult("file not found: %s", args.FilePath)
		}
		return errorResult("open %s: %v", args.FilePath, err)
	}
	content := string(raw)

	count := strings.Count(content, args.OldString)
	switch {
	case count == 0:
		return errorResult("old_string not found in %s.%s", args.FilePath, nearestCandidateHint(content, args.OldString))
	case count > 1 && !args.ReplaceAll:
		return errorResult("old_string matches %d times in %s; pass replace_all to change every occurrence, "+
			"or add more surrounding context to old_string to make it unique", count, args.FilePath)
	}

	var newContent string
	if args.ReplaceAll {
		newContent = strings.ReplaceAll(content, args.OldString, args.NewString)
	} else {
		newContent = strings.Replace(content, args.OldString, args.NewString, 1)
	}

	if err := os.WriteFile(path, []byte(newContent), 0o644); err != nil {
		return errorResult("write %s: %v", args.FilePath, err)
	}
	e.markRead(path)

	replacements := 1
	if args.ReplaceAll {
		replacements = count
	}
	diff := store.ComputeDiff(args.OldString, args.NewString)
	summary := fmt.Sprintf("Edited %s (%d replacement(s))\n\n%s", args.FilePath, replacements, renderDiffText(diff))
	out, truncated := truncate(summary, e.outputCap(ctx))
	return Result{Content: out, Truncated: truncated, Diff: diff}
}

// renderDiffText is the textual form of diff, for the tool message the model
// reads. The structured form travels alongside on Result.Diff for the
// browser to render as a table without recomputing anything (docs/DESIGN.md
// §5.4); both come from the same store.ComputeDiff call, so the two views
// never disagree.
func renderDiffText(diff []store.DiffLine) string {
	var b strings.Builder
	for _, l := range diff {
		switch l.Kind {
		case store.DiffRemove:
			b.WriteString("- " + l.Text + "\n")
		case store.DiffAdd:
			b.WriteString("+ " + l.Text + "\n")
		default:
			b.WriteString("  " + l.Text + "\n")
		}
	}
	return b.String()
}

// nearestCandidateHint looks for the line in content closest to
// old_string's first non-blank line, so a failed match points at a
// plausible location instead of leaving the model to search blind.
func nearestCandidateHint(content, oldString string) string {
	oldLines := strings.Split(oldString, "\n")
	var needle string
	for _, l := range oldLines {
		if strings.TrimSpace(l) != "" {
			needle = strings.TrimSpace(l)
			break
		}
	}
	if needle == "" {
		return ""
	}

	lines := strings.Split(content, "\n")
	for i, l := range lines {
		if strings.Contains(l, needle) {
			start := i + 1
			end := start + len(oldLines) - 1
			if end > len(lines) {
				end = len(lines)
			}
			return fmt.Sprintf(" The nearest match to old_string's first line is around lines %d-%d.", start, end)
		}
	}
	return ""
}
