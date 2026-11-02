package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// apply_patch carries a whole Codex-format patch in its input argument. The
// grammar is the one Codex and dsh use, so a model that has written a patch
// elsewhere can write one here:
//
//	*** Begin Patch
//	*** Add File: path
//	+line
//	*** Update File: path
//	*** Move to: newpath
//	@@ context
//	 unchanged
//	-removed
//	+added
//	*** End of File
//	*** Delete File: path
//	*** End Patch
//
// A patch is all or nothing: every hunk is applied to an in-memory copy of
// every file it names before a single byte is written, so a hunk that does
// not match leaves the workspace exactly as it was. That is what lets a
// change spanning several files go in one call.
//
// A file the patch changes must have been read this session, the same gate
// write and edit carry. Two hunks against remembered bytes are how a patch
// silently reverts someone else's change, and the gate is the only check that
// catches a patch written against a stale copy of a file whose context still
// happens to match. A file the patch creates needs no read, and a file an
// earlier operation in the same patch created is treated the same way.

// patchBegin, patchEnd and the section markers are the whole vocabulary of
// the format. Each section header carries its path after the marker.
const (
	patchBegin      = "*** Begin Patch"
	patchEnd        = "*** End Patch"
	patchAddFile    = "*** Add File: "
	patchDeleteFile = "*** Delete File: "
	patchUpdateFile = "*** Update File: "
	patchMoveTo     = "*** Move to: "
	patchEndOfFile  = "*** End of File"
)

type patchKind int

const (
	patchAdd patchKind = iota
	patchUpdate
	patchDelete
)

// patchOp is one file operation. A patch is a list of them, applied in the
// order written.
type patchOp struct {
	kind patchKind
	path string
	// moveTo is the "*** Move to:" destination of an update, as written.
	moveTo string
	// addLines is an Add File's body, without the leading '+'.
	addLines []string
	// hunks is an Update File's hunks, in order.
	hunks []patchHunk
}

// patchHunk is one @@ section of an update. context is the text after the
// "@@" marker, which anchors the hunk after the first line that matches it;
// empty means the hunk is searched for from the end of the previous one.
type patchHunk struct {
	context   string
	lines     []patchLine
	endOfFile bool
}

// patchLine is one line of a hunk. op is ' ' for context, '+' for an added
// line and '-' for a removed one; text is the line without that prefix.
type patchLine struct {
	op   byte
	text string
}

// patchText is a file's content as the lines a hunk matches against, plus
// whether the file ended with a newline. Keeping the terminator apart is what
// lets an update rewrite a file without adding or dropping one.
type patchText struct {
	lines           []string
	trailingNewline bool
}

// patchTextOf splits content into the lines a hunk matches against. The
// terminator is carried separately, so a file that does not end with a
// newline is put back exactly as it was found.
func patchTextOf(content string) patchText {
	if content == "" {
		return patchText{}
	}
	trailing := strings.HasSuffix(content, "\n")
	if trailing {
		content = content[:len(content)-1]
	}
	return patchText{lines: strings.Split(content, "\n"), trailingNewline: trailing}
}

// String renders the lines back into file content.
func (t patchText) String() string {
	if len(t.lines) == 0 {
		return ""
	}
	out := strings.Join(t.lines, "\n")
	if t.trailingNewline {
		out += "\n"
	}
	return out
}

// applyPatchArgs is the tool's whole argument set: one string, named the way
// Codex names it.
type applyPatchArgs struct {
	Input string `json:"input"`
}

// parsePatch reads a patch's text into the operations it names. It validates
// the envelope, every section header and every hunk line; applying anything
// is the caller's job, after the whole patch has parsed.
func parsePatch(input string) ([]patchOp, error) {
	// A patch written on Windows arrives with CRLF line endings, and a
	// trailing '\r' on every line would make every context line fail to
	// match.
	input = strings.ReplaceAll(input, "\r\n", "\n")
	lines := strings.Split(input, "\n")

	start, end := 0, len(lines)
	for start < end && strings.TrimSpace(lines[start]) == "" {
		start++
	}
	for end > start && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	if start == end {
		return nil, fmt.Errorf("the patch is empty")
	}
	if strings.TrimSpace(lines[start]) != patchBegin {
		return nil, fmt.Errorf("the first line of a patch must be %q, got %q", patchBegin, lines[start])
	}
	if strings.TrimSpace(lines[end-1]) != patchEnd {
		return nil, fmt.Errorf("the last line of a patch must be %q, got %q", patchEnd, lines[end-1])
	}

	body := lines[start+1 : end-1]
	lineNo := func(i int) int { return start + i + 2 }

	var ops []patchOp
	for i := 0; i < len(body); {
		trimmed := strings.TrimSpace(body[i])
		switch {
		case trimmed == "":
			i++

		case strings.HasPrefix(trimmed, patchAddFile):
			op := patchOp{kind: patchAdd, path: strings.TrimSpace(strings.TrimPrefix(trimmed, patchAddFile))}
			if op.path == "" {
				return nil, fmt.Errorf("line %d: an Add File header needs a path", lineNo(i))
			}
			i++
			for i < len(body) && strings.HasPrefix(body[i], "+") {
				op.addLines = append(op.addLines, body[i][1:])
				i++
			}
			if len(op.addLines) == 0 {
				return nil, fmt.Errorf("%s: an Add File needs at least one line starting with +", op.path)
			}
			ops = append(ops, op)

		case strings.HasPrefix(trimmed, patchDeleteFile):
			op := patchOp{kind: patchDelete, path: strings.TrimSpace(strings.TrimPrefix(trimmed, patchDeleteFile))}
			if op.path == "" {
				return nil, fmt.Errorf("line %d: a Delete File header needs a path", lineNo(i))
			}
			ops = append(ops, op)
			i++

		case strings.HasPrefix(trimmed, patchUpdateFile):
			op := patchOp{kind: patchUpdate, path: strings.TrimSpace(strings.TrimPrefix(trimmed, patchUpdateFile))}
			if op.path == "" {
				return nil, fmt.Errorf("line %d: an Update File header needs a path", lineNo(i))
			}
			i++
			if i < len(body) {
				if dest, ok := markerPath(body[i], patchMoveTo); ok {
					if dest == "" {
						return nil, fmt.Errorf("line %d: a Move to header needs a path", lineNo(i))
					}
					op.moveTo = dest
					i++
				}
			}
			for i < len(body) {
				hunkLine := strings.TrimSpace(body[i])
				if strings.HasPrefix(hunkLine, "***") {
					if !isPatchSection(hunkLine) {
						return nil, unrecognisedHeader(body[i], lineNo(i))
					}
					break
				}
				if hunkLine == "" {
					// A blank line between hunks, or after *** End of File,
					// is spacing. Inside a hunk the same line was already
					// consumed as an empty context line.
					i++
					continue
				}
				if !strings.HasPrefix(hunkLine, "@@") {
					return nil, fmt.Errorf("line %d: a hunk of %s must start with @@, got %q", lineNo(i), op.path, body[i])
				}
				hunk := patchHunk{}
				if hunkLine != "@@" {
					hunk.context = strings.TrimSpace(strings.TrimPrefix(hunkLine, "@@"))
				}
				i++
				for i < len(body) {
					line := body[i]
					trimmedLine := strings.TrimSpace(line)
					if strings.HasPrefix(trimmedLine, "***") {
						if trimmedLine == patchEndOfFile {
							hunk.endOfFile = true
							i++
						}
						break
					}
					if strings.HasPrefix(trimmedLine, "@@") {
						break
					}
					// A bare empty line is a context line. Codex's parser
					// is lenient the same way, and a model that stripped the
					// leading space from a blank source line means the same
					// thing by it.
					if line == "" {
						hunk.lines = append(hunk.lines, patchLine{op: ' ', text: ""})
						i++
						continue
					}
					switch line[0] {
					case ' ', '+', '-':
						hunk.lines = append(hunk.lines, patchLine{op: line[0], text: line[1:]})
						i++
					default:
						return nil, fmt.Errorf("line %d: a hunk line of %s must start with a space, + or -, got %q",
							lineNo(i), op.path, line)
					}
				}
				if len(hunk.lines) == 0 {
					return nil, fmt.Errorf("%s: a hunk has no lines", op.path)
				}
				op.hunks = append(op.hunks, hunk)
			}
			if len(op.hunks) == 0 && op.moveTo == "" {
				return nil, fmt.Errorf("%s: an Update File needs a hunk starting with @@, or a %q line",
					op.path, patchMoveTo)
			}
			ops = append(ops, op)

		default:
			return nil, unrecognisedHeader(body[i], lineNo(i))
		}
	}
	if len(ops) == 0 {
		return nil, fmt.Errorf("the patch names no files")
	}
	return ops, nil
}

// isPatchSection reports whether line starts one of the sections an Update
// File's hunks can be followed by.
func isPatchSection(line string) bool {
	for _, marker := range []string{patchAddFile, patchUpdateFile, patchDeleteFile, patchMoveTo} {
		if strings.HasPrefix(line, marker) {
			return true
		}
	}
	return false
}

// unrecognisedHeader is the one error for a line that is not a section the
// format knows, wherever the parser meets it.
func unrecognisedHeader(line string, lineNo int) error {
	return fmt.Errorf("line %d: unrecognised patch header %q; a section starts with "+
		"%q, %q, %q or %q", lineNo, line, patchAddFile, patchUpdateFile, patchDeleteFile, patchMoveTo)
}

// markerPath reports whether line carries marker and returns what follows it.
func markerPath(line, marker string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, marker) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(trimmed, marker)), true
}

// execApplyPatch implements apply_patch: parse the whole patch, stage every
// operation against an in-memory copy, and write only once each one has
// succeeded.
func execApplyPatch(ctx context.Context, e *Executor, argsRaw json.RawMessage) Result {
	var args applyPatchArgs
	if err := json.Unmarshal(argsRaw, &args); err != nil {
		return errorResult("invalid arguments: %v", err)
	}
	if strings.TrimSpace(args.Input) == "" {
		return errorResult("input is required: it carries the patch, from %q to %q", patchBegin, patchEnd)
	}
	ops, err := parsePatch(args.Input)
	if err != nil {
		return errorResult("invalid patch: %v", err)
	}

	staged := make(map[string]*stagedPatchFile)
	// destinations tracks the paths an operation will create, so two
	// operations in one patch cannot claim the same new file.
	destinations := make(map[string]bool)
	var order []*stagedPatchFile

	stage := func(display string) (*stagedPatchFile, error) {
		resolved, err := ResolvePath(e.Workspace, display)
		if err != nil {
			return nil, err
		}
		if f, ok := staged[resolved]; ok {
			return f, nil
		}
		f := &stagedPatchFile{display: display, resolved: resolved}
		info, statErr := os.Stat(resolved)
		switch {
		case statErr == nil:
			if info.IsDir() {
				return nil, fmt.Errorf("%s is a directory, not a file", display)
			}
			size, binary, err := isBinaryFile(resolved)
			if err != nil {
				return nil, fmt.Errorf("open %s: %w", display, err)
			}
			data, err := os.ReadFile(resolved)
			if err != nil {
				return nil, fmt.Errorf("open %s: %w", display, err)
			}
			f.existed, f.size, f.binary = true, size, binary
			f.mode = info.Mode().Perm()
			f.text = patchTextOf(string(data))
		case os.IsNotExist(statErr):
			// A file the patch creates.
		default:
			return nil, fmt.Errorf("stat %s: %w", display, statErr)
		}
		staged[resolved] = f
		order = append(order, f)
		return f, nil
	}

	// changedRefuses runs the checks every operation against an existing
	// file shares: the prior read that write and edit also require, and the
	// binary guard that keeps a patch from rewriting a file as text.
	changedRefuses := func(f *stagedPatchFile) Result {
		if f.existed && !e.wasRead(f.resolved) {
			return errorResult("%s has not been read in this session; read it before patching it", f.display)
		}
		if f.existed && f.binary {
			return binaryFileError(f.display, f.size)
		}
		return Result{}
	}

	for _, op := range ops {
		switch op.kind {
		case patchAdd:
			f, err := stage(op.path)
			if err != nil {
				return errorResult("%v", err)
			}
			if f.existed || f.added {
				return errorResult("%s already exists; use %q to change a file that is already there", op.path, patchUpdateFile)
			}
			if destinations[f.resolved] {
				return errorResult("the patch creates %s twice", op.path)
			}
			destinations[f.resolved] = true
			f.added = true
			f.text = patchText{lines: op.addLines, trailingNewline: true}

		case patchDelete:
			f, err := stage(op.path)
			if err != nil {
				return errorResult("%v", err)
			}
			if !f.existed && !f.added {
				return errorResult("file not found: %s", op.path)
			}
			if f.deleted {
				return errorResult("%s is deleted earlier in this patch", op.path)
			}
			if res := changedRefuses(f); res.IsError {
				return res
			}
			f.deleted = true

		case patchUpdate:
			f, err := stage(op.path)
			if err != nil {
				return errorResult("%v", err)
			}
			if !f.existed && !f.added {
				return errorResult("file not found: %s", op.path)
			}
			if f.deleted {
				return errorResult("%s is deleted earlier in this patch", op.path)
			}
			if res := changedRefuses(f); res.IsError {
				return res
			}
			f.text, err = applyPatchHunks(f.text, op)
			if err != nil {
				return errorResult("%v", err)
			}
			if op.moveTo != "" {
				resolved, err := ResolvePath(e.Workspace, op.moveTo)
				if err != nil {
					return errorResult("%v", err)
				}
				if resolved == f.resolved {
					return errorResult("%s: %q names the file being updated", op.path, patchMoveTo+op.moveTo)
				}
				if _, statErr := os.Stat(resolved); statErr == nil {
					return errorResult("cannot move %s to %s: the destination already exists", op.path, op.moveTo)
				} else if !os.IsNotExist(statErr) {
					return errorResult("stat %s: %v", op.moveTo, statErr)
				}
				if destinations[resolved] {
					return errorResult("the patch creates %s twice", op.moveTo)
				}
				destinations[resolved] = true
				f.moveTo, f.moveName = resolved, op.moveTo
			}
		}
	}

	// Every hunk applied and every check passed. Writes come first so a
	// failed write leaves the old files in place, and deletes last so a
	// failed delete leaves a file that may be new but is never lost.
	for _, f := range order {
		if f.deleted {
			continue
		}
		target := f.resolved
		if f.moveTo != "" {
			target = f.moveTo
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return errorResult("create directory for %s: %v", f.display, err)
		}
		if err := os.WriteFile(target, []byte(f.text.String()), 0o644); err != nil {
			return errorResult("write %s: %v", f.display, err)
		}
		// A move creates a new file, and os.WriteFile's mode only applies
		// when it does. Carry the source's own mode across so a patch that
		// renames an executable script does not leave it unrunnable.
		if f.existed && f.mode != 0 {
			if err := os.Chmod(target, f.mode); err != nil {
				return errorResult("set mode on %s: %v", f.display, err)
			}
		}
		e.markRead(target)
	}
	for _, f := range order {
		if !f.deleted && f.moveTo == "" {
			continue
		}
		if err := os.Remove(f.resolved); err != nil && !os.IsNotExist(err) {
			return errorResult("remove %s: %v", f.display, err)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Applied a patch to %d file", len(order))
	if len(order) != 1 {
		b.WriteString("s")
	}
	b.WriteString(":\n")
	for _, f := range order {
		switch {
		case f.deleted:
			fmt.Fprintf(&b, "- deleted %s\n", f.display)
		case f.added:
			fmt.Fprintf(&b, "- added %s\n", f.display)
		case f.moveTo != "":
			fmt.Fprintf(&b, "- moved %s to %s\n", f.display, f.moveName)
		default:
			fmt.Fprintf(&b, "- updated %s\n", f.display)
		}
	}
	out, truncated := truncate(strings.TrimRight(b.String(), "\n"), e.outputCap(ctx))
	return Result{Content: out, Truncated: truncated}
}

// stagedPatchFile is one file a patch changes, held in memory until every
// operation in the patch has applied.
type stagedPatchFile struct {
	display  string
	resolved string
	text     patchText
	existed  bool
	binary   bool
	size     int64
	mode     os.FileMode
	added    bool
	deleted  bool
	moveTo   string
	moveName string
}

// applyPatchHunks applies one Update File's hunks to text in order. Each hunk
// is located from the end of the previous one, so a patch that changes two
// identical regions of a file changes the first then the second.
func applyPatchHunks(text patchText, op patchOp) (patchText, error) {
	lines := text.lines
	at := 0
	for i, hunk := range op.hunks {
		if hunk.context != "" {
			found := seekPatchContext(lines, hunk.context, at)
			if found < 0 {
				return text, fmt.Errorf("%s: hunk %d: the @@ context %q does not appear at or after line %d",
					op.path, i+1, hunk.context, at+1)
			}
			at = found + 1
		}

		old := make([]string, 0, len(hunk.lines))
		new := make([]string, 0, len(hunk.lines))
		for _, l := range hunk.lines {
			if l.op != '+' {
				old = append(old, l.text)
			}
			if l.op != '-' {
				new = append(new, l.text)
			}
		}
		found := locatePatchHunk(lines, old, at, hunk.endOfFile)
		if found < 0 {
			hint := nearestCandidateHint(strings.Join(lines, "\n"), strings.Join(old, "\n"))
			if hunk.endOfFile {
				return text, fmt.Errorf("%s: hunk %d does not apply: its lines are not at the end of the file:%s\n%s",
					op.path, i+1, hint, strings.Join(old, "\n"))
			}
			return text, fmt.Errorf("%s: hunk %d does not apply: no such lines at or after line %d:%s\n%s",
				op.path, i+1, at+1, hint, strings.Join(old, "\n"))
		}
		lines = splicePatchLines(lines, found, len(old), new)
		at = found + len(new)
	}
	// A file that had no lines at all gains a terminating newline with its
	// first content: there is no terminator to preserve, and text files end
	// with one.
	trailing := text.trailingNewline
	if len(text.lines) == 0 && len(lines) > 0 {
		trailing = true
	}
	return patchText{lines: lines, trailingNewline: trailing}, nil
}

// seekPatchContext returns the index of the first line at or after from that
// matches the "@@" context text. The comparison tolerates surrounding
// whitespace on the anchor alone: the anchor names a region rather than
// carrying bytes that have to survive, unlike a hunk's own context lines.
func seekPatchContext(lines []string, context string, from int) int {
	if from < 0 {
		from = 0
	}
	for i := from; i < len(lines); i++ {
		if lines[i] == context || strings.TrimSpace(lines[i]) == strings.TrimSpace(context) {
			return i
		}
	}
	return -1
}

// locatePatchHunk returns the index at which a hunk's old-side lines match,
// or -1. An endOfFile hunk matches only at the end of the file.
func locatePatchHunk(lines, old []string, from int, endOfFile bool) int {
	if len(old) == 0 {
		if endOfFile {
			return len(lines)
		}
		if from > len(lines) {
			return len(lines)
		}
		return from
	}
	if endOfFile {
		at := len(lines) - len(old)
		if at >= from && at >= 0 && linesMatchAt(lines, at, old) {
			return at
		}
		return -1
	}
	for i := from; i+len(old) <= len(lines); i++ {
		if linesMatchAt(lines, i, old) {
			return i
		}
	}
	return -1
}

func linesMatchAt(lines []string, at int, old []string) bool {
	for i, want := range old {
		if lines[at+i] != want {
			return false
		}
	}
	return true
}

// splicePatchLines returns lines with remove lines at at replaced by add.
func splicePatchLines(lines []string, at, remove int, add []string) []string {
	out := make([]string, 0, len(lines)-remove+len(add))
	out = append(out, lines[:at]...)
	out = append(out, add...)
	out = append(out, lines[at+remove:]...)
	return out
}
