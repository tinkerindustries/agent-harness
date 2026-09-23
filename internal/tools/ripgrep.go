package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ripgrepMaxColumns is the argument Grep passes as --max-columns: a matching
// or context line at least this many bytes long is printed as an omission
// marker instead of its text, so one minified file cannot fill a result. The
// fallback applies the same rule (grepText) so both paths answer a call alike.
const ripgrepMaxColumns = 500

// RipgrepPath decides which ripgrep binary Grep runs. explicit is the -rg
// flag, which wins; then AGENT_HARNESS_RG; then the PATH. An empty return
// means no ripgrep was found and Grep falls back to its own walk.
//
// A binary named by the flag or the environment must be there: a caller that
// names one is stating where ripgrep is, and falling back to the walk would
// hide the typo behind output that looks like a real answer.
func RipgrepPath(explicit string) (string, error) {
	if explicit != "" {
		if err := checkRipgrep(explicit); err != nil {
			return "", fmt.Errorf("-rg: %w", err)
		}
		return explicit, nil
	}
	if env := os.Getenv("AGENT_HARNESS_RG"); env != "" {
		if err := checkRipgrep(env); err != nil {
			return "", fmt.Errorf("AGENT_HARNESS_RG: %w", err)
		}
		return env, nil
	}
	found, err := exec.LookPath("rg")
	if err != nil {
		return "", nil
	}
	return found, nil
}

func checkRipgrep(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory", path)
	}
	return nil
}

// ripgrepArgs is the argument list for one Grep call. mode is the resolved
// output_mode; path is the search path as the caller spelled it, empty for
// the workspace root, which ripgrep then searches as its own directory.
//
// --sort=path is what makes head_limit mean the same thing twice: ripgrep
// walks in parallel by default, so which lines survive a first-n cut would
// otherwise vary between identical calls, and the fallback's sorted walk
// could not agree with it.
//
// --path-separator / is what keeps a path the same on every platform:
// ripgrep on Windows joins the rest of a path onto the caller's spelling with
// a backslash, so "src" would come back as "src\main\x.md" while the
// fallback, and the same call on any other platform, says "src/main/x.md".
// Elsewhere it is ripgrep's default already.
func ripgrepArgs(args grepArgs, mode, path string) []string {
	argv := []string{"--hidden", "--max-columns", strconv.Itoa(ripgrepMaxColumns), "--sort=path", "--path-separator", "/"}
	for _, dir := range grepSkipDirs {
		// --hidden lifts ripgrep's own dot-directory skipping, so each
		// version-control directory has to be excluded by name.
		argv = append(argv, "--glob", "!"+dir)
	}

	switch mode {
	case "files_with_matches":
		// --null frames each path with a NUL, so a path containing a colon
		// cannot be read as two fields.
		argv = append(argv, "-l", "--null")
	case "count":
		argv = append(argv, "-c", "-H", "--null")
	case "content":
		// -H always, so every mode names the file each line came from
		// whatever kind of root was searched. ripgrep prints no path itself
		// when one file was named, and a caller that has to remember which
		// shape a mode returns for which kind of root is the bug this is
		// here to remove.
		argv = append(argv, "-H")
		if args.lineNumbers() {
			argv = append(argv, "-n")
		}
		if args.Context > 0 {
			argv = append(argv, "-C", strconv.Itoa(args.Context))
		} else {
			if args.ContextBefore > 0 {
				argv = append(argv, "-B", strconv.Itoa(args.ContextBefore))
			}
			if args.ContextAfter > 0 {
				argv = append(argv, "-A", strconv.Itoa(args.ContextAfter))
			}
		}
	}

	if args.CaseInsensitive {
		argv = append(argv, "-i")
	}
	if args.Multiline {
		argv = append(argv, "-U", "--multiline-dotall")
	}
	if args.Glob != "" {
		argv = append(argv, "--glob", args.Glob)
	}
	if args.FileType != "" {
		argv = append(argv, "--type", args.FileType)
	}
	// A pattern that starts with "-" would be read as a flag; -e is the way
	// to say it is the pattern, whatever it looks like.
	if strings.HasPrefix(args.Pattern, "-") {
		argv = append(argv, "-e", args.Pattern)
	} else {
		argv = append(argv, args.Pattern)
	}
	if path != "" {
		argv = append(argv, "--", path)
	}
	return argv
}

// execRipgrep runs ripgrep and hands its stdout back as the call's result.
// ripgrep already prints the shape this tool returns — path:line:text for
// content, one path per line for files_with_matches, path:count for count —
// so nothing here re-labels or re-formats a match; only head_limit and the
// output cap apply on top (finishGrepResult).
//
// ripgrep's exit codes carry the outcome: 0 with matches, 1 with none, 2 on
// a real error. The error's message is ripgrep's own stderr, which names the
// path it could not open — a missing path becomes a refusal that says so
// rather than an empty "no matches".
func execRipgrep(ctx context.Context, e *Executor, args grepArgs, mode, path string) Result {
	cmd := exec.CommandContext(ctx, e.RG, ripgrepArgs(args, mode, path)...)
	cmd.Dir = e.Workspace
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return errorResult("search %s: %v", searchedName(path), ctx.Err())
		}
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			switch exit.ExitCode() {
			case 1:
				return Result{Content: "no matches"}
			case 2:
				return errorResult("%s", strings.TrimSpace(stderr.String()))
			}
		}
		return errorResult("ripgrep %s: %v", e.RG, err)
	}

	return finishGrepResult(ctx, e, renderRipgrepOutput(mode, stdout.String()), args.HeadLimit)
}

// renderRipgrepOutput is the one place ripgrep's stdout is touched, and only
// where the frame its own stdout carries is not the frame this tool returns.
//
// content mode is passed through as printed, less the trailing newline the
// fallback's own rendering does not carry.
//
// The two list modes ask ripgrep for --null, which frames every path with a
// NUL byte, so neither has to find the end of a path by looking for a ":"
// that the path itself may contain. files_with_matches returns the paths one
// per line, the shape a model hands back to Read. count splits each entry at
// ripgrep's own NUL and writes the pair as path:count, which is the shape the
// mode is documented with.
func renderRipgrepOutput(mode, out string) string {
	switch mode {
	case "files_with_matches":
		var paths []string
		for _, p := range strings.Split(out, "\x00") {
			if p != "" {
				paths = append(paths, p)
			}
		}
		return joinLines(paths)

	case "count":
		var lines []string
		for _, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
			if line == "" {
				continue
			}
			path, count, _ := strings.Cut(line, "\x00")
			lines = append(lines, path+":"+count)
		}
		return joinLines(lines)
	}

	return strings.TrimSuffix(out, "\n")
}

// searchedName is how a refusal names the search it could not run.
func searchedName(path string) string {
	if path == "" {
		return "."
	}
	return path
}
