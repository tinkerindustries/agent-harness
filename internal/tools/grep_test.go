package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// lineNumbers spells an explicit -n argument. Absent means on
// (grepArgs.lineNumbers), so only a caller that wants the lines alone needs
// this.
func lineNumbers(on bool) *bool { return &on }

// ripgrepForTest returns the ripgrep binary the Grep path would run, or skips
// the test naming what it could not find. A machine with no ripgrep falls
// back to the Go walk, which is the path these tests compare against, so
// there is nothing to compare.
func ripgrepForTest(t *testing.T) string {
	t.Helper()
	path, err := RipgrepPath("")
	if err != nil {
		t.Fatalf("RipgrepPath: %v", err)
	}
	if path == "" {
		t.Skip("no ripgrep found (set AGENT_HARNESS_RG or put rg on the PATH); the Go fallback is the only path here")
	}
	return path
}

func executorAt(t *testing.T, root, rg string) *Executor {
	t.Helper()
	e, err := NewExecutor(root, &Policy{Mode: ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	e.RG = rg
	return e
}

// newGrepFixture lays out the tree both paths are run against. It carries
// what a search has to get right and nothing that only one of the two can
// answer: nested directories, a hidden file, a file whose name holds a colon,
// a CRLF file, a line past ripgrep's column cap, a binary file (which both
// paths skip in a walk), and a version-control directory.
//
// No .git directory is created here on purpose. A tree containing one is a
// git repository and ripgrep then reads the machine's ignore files, which
// would make this fixture's answer depend on the machine it runs on.
func newGrepFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, dir := range []string{"sub", ".svn", "node_modules"} {
		mustMkdirAll(t, root, dir)
	}
	writeFile(t, root, "a.md", "needle one\nplain\nNEEDLE two\n")
	writeFile(t, root, "sub/b.go", "package b\n\n// needle here\n")
	writeFile(t, root, "sub/c.py", "needle python\n")
	writeFile(t, root, "sub/notes.md", "needle notes\n")
	writeFile(t, root, "ml.md", "alpha\nbeta\ngamma\n")
	writeFile(t, root, "long.md", "needle "+strings.Repeat("x", 600)+"\nshort needle\n")
	writeFile(t, root, "a:b.md", "needle colon\nneedle again\n")
	writeFile(t, root, "crlf.md", "needle\r\nother\r\n")
	writeFile(t, root, ".hidden.md", "needle hidden\n")
	writeFile(t, root, ".svn/entries.md", "needle svn\n")
	writeFile(t, root, "node_modules/x.js", "needle modules\n")
	if err := os.WriteFile(filepath.Join(root, "bin.dat"), []byte("needle\x00binary\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestGrepRipgrepArguments pins the flag each Grep argument becomes. A
// dropped or renamed flag changes what a search returns without changing its
// shape, which no comparison of output against the fallback would catch.
func TestGrepRipgrepArguments(t *testing.T) {
	fixed := []string{"--hidden", "--max-columns", "500", "--sort=path", "--path-separator", "/"}
	for _, dir := range grepSkipDirs {
		fixed = append(fixed, "--glob", "!"+dir)
	}

	cases := []struct {
		name  string
		args  grepArgs
		mode  string
		path  string
		extra []string
	}{
		{
			name:  "files_with_matches asks for a NUL-framed path list",
			args:  grepArgs{Pattern: "needle"},
			mode:  "files_with_matches",
			extra: []string{"-l", "--null", "needle"},
		},
		{
			name:  "count names each file so a single-file search still shows a path",
			args:  grepArgs{Pattern: "needle"},
			mode:  "count",
			extra: []string{"-c", "-H", "--null", "needle"},
		},
		{
			name:  "content names every file and turns line numbers on by default",
			args:  grepArgs{Pattern: "needle"},
			mode:  "content",
			extra: []string{"-H", "-n", "needle"},
		},
		{
			name:  "an explicit -n false leaves the lines alone",
			args:  grepArgs{Pattern: "needle", ShowLineNumbers: lineNumbers(false)},
			mode:  "content",
			extra: []string{"-H", "needle"},
		},
		{
			name:  "content carries context, case and the file filters",
			args:  grepArgs{Pattern: "needle", Context: 2, CaseInsensitive: true, Glob: "*.go", FileType: "go"},
			mode:  "content",
			extra: []string{"-H", "-n", "-C", "2", "-i", "--glob", "*.go", "--type", "go", "needle"},
		},
		{
			name:  "content translates -B and -A when no -C was given",
			args:  grepArgs{Pattern: "needle", ContextBefore: 1, ContextAfter: 2},
			mode:  "content",
			extra: []string{"-H", "-n", "-B", "1", "-A", "2", "needle"},
		},
		{
			name:  "multiline matches across lines with . matching a newline",
			args:  grepArgs{Pattern: "a.b", Multiline: true},
			mode:  "content",
			extra: []string{"-H", "-n", "-U", "--multiline-dotall", "a.b"},
		},
		{
			name:  "a pattern starting with a dash is passed as a pattern",
			args:  grepArgs{Pattern: "-needle"},
			mode:  "files_with_matches",
			extra: []string{"-l", "--null", "-e", "-needle"},
		},
		{
			name:  "a search path is spelled out and separated from the flags",
			args:  grepArgs{Pattern: "needle"},
			mode:  "files_with_matches",
			path:  "sub dir/x.md",
			extra: []string{"-l", "--null", "needle", "--", "sub dir/x.md"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ripgrepArgs(tc.args, tc.mode, tc.path)
			want := append(append([]string{}, fixed...), tc.extra...)
			if len(got) != len(want) {
				t.Fatalf("argv = %q, want %q", got, want)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("argv = %q, want %q", got, want)
				}
			}
		})
	}
}

// TestRipgrepPathOrder pins where the binary comes from: the flag first, then
// AGENT_HARNESS_RG, then the PATH. It also pins the asymmetry — a path named
// explicitly must exist — because a flag naming a binary that is not there
// means the caller expected ripgrep, and searching the tree instead would
// answer from the wrong place and say it had.
func TestRipgrepPathOrder(t *testing.T) {
	named := filepath.Join(t.TempDir(), "rg")
	if err := os.WriteFile(named, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("the flag wins over the environment", func(t *testing.T) {
		t.Setenv("AGENT_HARNESS_RG", filepath.Join(t.TempDir(), "other-rg"))
		got, err := RipgrepPath(named)
		if err != nil {
			t.Fatalf("RipgrepPath: %v", err)
		}
		if got != named {
			t.Fatalf("RipgrepPath = %q, want the flag's %q", got, named)
		}
	})

	t.Run("the environment is used when no flag was given", func(t *testing.T) {
		t.Setenv("AGENT_HARNESS_RG", named)
		got, err := RipgrepPath("")
		if err != nil {
			t.Fatalf("RipgrepPath: %v", err)
		}
		if got != named {
			t.Fatalf("RipgrepPath = %q, want AGENT_HARNESS_RG's %q", got, named)
		}
	})

	t.Run("a named binary that is not there is an error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "rg")
		if _, err := RipgrepPath(missing); err == nil || !strings.Contains(err.Error(), missing) {
			t.Fatalf("RipgrepPath(%q) = %v, want an error naming it", missing, err)
		}
	})

	t.Run("nothing named and nothing on the PATH falls back", func(t *testing.T) {
		t.Setenv("AGENT_HARNESS_RG", "")
		os.Unsetenv("AGENT_HARNESS_RG")
		t.Setenv("PATH", "")
		got, err := RipgrepPath("")
		if err != nil {
			t.Fatalf("RipgrepPath: %v", err)
		}
		if got != "" {
			t.Fatalf("RipgrepPath = %q, want no binary so Grep walks", got)
		}
	})
}

// TestGrepRipgrepLabelsPathsTheWayTheCallerSpelledThem pins the directory
// label: a search of "src" reports "src/main/prompts/cad.md", not the path
// relative to the search root, which names no file the model can open
// without re-prefixing it. ripgrep echoes the path as the caller wrote it and
// appends the rest, so "./src" keeps its "./" and an absolute path stays
// absolute.
func TestGrepRipgrepLabelsPathsTheWayTheCallerSpelledThem(t *testing.T) {
	rg := ripgrepForTest(t)
	root := t.TempDir()
	mustMkdirAll(t, root, "src/main/prompts")
	writeFile(t, root, "src/main/prompts/cad.md", "needle\n")
	writeFile(t, root, "elsewhere.md", "needle\n")
	e := executorAt(t, root, rg)

	cases := []struct {
		name string
		path string
		want string
	}{
		{"a directory keeps the caller's spelling", "src", "src/main/prompts/cad.md"},
		{"a trailing slash does not double", "src/", "src/main/prompts/cad.md"},
		{"a dot keeps its leading ./", ".", "./src/main/prompts/cad.md"},
		{"a file reports the path the caller gave", "src/main/prompts/cad.md", "src/main/prompts/cad.md"},
		{"an absolute directory reports absolute paths", filepath.Join(root, "src"), filepath.ToSlash(filepath.Join(root, "src/main/prompts/cad.md"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle", Path: tc.path}))
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			for _, line := range strings.Split(res.Content, "\n") {
				if strings.Contains(line, "cad.md") && line != tc.want {
					t.Fatalf("search of %q reported %q, want %q", tc.path, line, tc.want)
				}
			}
			if !strings.Contains(res.Content, tc.want) {
				t.Fatalf("search of %q reported %q, want it to contain %q", tc.path, res.Content, tc.want)
			}
		})
	}
}

// TestGrepRipgrepReportsAMissingPathAsAnError pins the exit code mapping: a
// path that is not there is ripgrep's exit 2, and its stderr names the path.
// Read as "no matches" the caller is told a search happened and found
// nothing, which is a different answer from the search never running.
func TestGrepRipgrepReportsAMissingPathAsAnError(t *testing.T) {
	rg := ripgrepForTest(t)
	e := executorAt(t, t.TempDir(), rg)

	res := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle", Path: "no/such/dir"}))
	if !res.IsError {
		t.Fatalf("a missing path must be an error, got %q", res.Content)
	}
	if !strings.Contains(res.Content, "no/such/dir") {
		t.Fatalf("the error must name the path it could not search, got %q", res.Content)
	}
}

// TestGrepRipgrepKeepsAColonInAFileName pins the choice the two list modes
// make: they ask ripgrep for --null, so a path containing ":" is framed by a
// NUL rather than a separator it could be split on. files_with_matches is the
// mode whose output a model hands straight back to Read, and it comes back as
// one whole path per line however the name is spelled.
func TestGrepRipgrepKeepsAColonInAFileName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows file names cannot hold a colon; on NTFS a:b.md names stream b.md of file a")
	}
	rg := ripgrepForTest(t)
	root := t.TempDir()
	writeFile(t, root, "a:b.md", "needle\nneedle\n")
	e := executorAt(t, root, rg)

	files := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle"}))
	if files.Content != "a:b.md" {
		t.Fatalf("files_with_matches = %q, want the whole path a:b.md", files.Content)
	}

	counted := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle", OutputMode: "count"}))
	if counted.Content != "a:b.md:2" {
		t.Fatalf("count = %q, want a:b.md:2", counted.Content)
	}
}

// TestGrepRipgrepUsesRipgrepsTypeList pins the contract change the ripgrep
// path brings with it: `type` is ripgrep's own table of names, not the
// harness's old hand-written list of twenty extensions. haskell is a name
// that list never carried, so a search for it finding an .hs file is what
// says the list came from ripgrep.
func TestGrepRipgrepUsesRipgrepsTypeList(t *testing.T) {
	rg := ripgrepForTest(t)
	root := t.TempDir()
	writeFile(t, root, "main.hs", "needle\n")
	writeFile(t, root, "main.go", "needle\n")
	e := executorAt(t, root, rg)

	typed := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle", FileType: "haskell"}))
	if typed.IsError || typed.Content != "main.hs" {
		t.Fatalf("type haskell = %q (error %v), want main.hs", typed.Content, typed.IsError)
	}

	bad := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle", FileType: "not-a-real-type"}))
	if !bad.IsError || !strings.Contains(bad.Content, "not-a-real-type") {
		t.Fatalf("an unrecognised type must be refused by name, got %q", bad.Content)
	}
}

// TestGrepFallbackRefusesTypeWithoutRipgrep pins what the walk cannot do.
// ripgrep's type names are ripgrep's to resolve, and the walk has no table of
// its own to answer with — an approximation would answer a call differently
// from the ripgrep path and say nothing about it.
func TestGrepFallbackRefusesTypeWithoutRipgrep(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "needle\n")

	res := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle", FileType: "go"}))
	if !res.IsError {
		t.Fatalf("the walk must refuse a type filter it cannot apply, got %q", res.Content)
	}
	for _, want := range []string{"go", "-rg", "AGENT_HARNESS_RG"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("the refusal must name %q, got %q", want, res.Content)
		}
	}
}

// TestGrepFallbackMatchesRipgrep is the invariant the two paths are built
// around: the same call answers the same way whether ripgrep ran it or the Go
// walk did. The walk is what a machine with no ripgrep has, so a label, a
// separator or an omission that only one path produces makes the tool's
// answer depend on which machine it is on.
//
// It fails on any difference in content, error state or truncation for any
// case below.
func TestGrepFallbackMatchesRipgrep(t *testing.T) {
	rg := ripgrepForTest(t)
	root := newGrepFixture(t)
	withRG := executorAt(t, root, rg)
	withWalk := executorAt(t, root, "")

	cases := []struct {
		name string
		args grepArgs
	}{
		{"files_with_matches in the workspace", grepArgs{Pattern: "needle"}},
		{"files_with_matches from a subdirectory", grepArgs{Pattern: "needle", Path: "sub"}},
		{"files_with_matches from a root spelled as .", grepArgs{Pattern: "needle", Path: "."}},
		{"files_with_matches on one file", grepArgs{Pattern: "needle", Path: "sub/b.go"}},
		{"files_with_matches on a name holding a colon", grepArgs{Pattern: "needle", Path: "a:b.md"}},
		{"files_with_matches with no matches", grepArgs{Pattern: "no-such-token-anywhere"}},
		{"count in the workspace", grepArgs{Pattern: "needle", OutputMode: "count"}},
		{"count on one file", grepArgs{Pattern: "needle", Path: "a:b.md", OutputMode: "count"}},
		{"content with line numbers on by default", grepArgs{Pattern: "needle", OutputMode: "content"}},
		{"content with line numbers turned off", grepArgs{Pattern: "needle", OutputMode: "content", ShowLineNumbers: lineNumbers(false)}},
		{"content on one file names the file too", grepArgs{Pattern: "needle", Path: "sub/b.go", OutputMode: "content"}},
		{"content with context separators", grepArgs{Pattern: "needle", OutputMode: "content", Context: 1}},
		{"content with -B and -A", grepArgs{Pattern: "needle", OutputMode: "content", ContextBefore: 1, ContextAfter: 2}},
		{"content with no matches", grepArgs{Pattern: "no-such-token-anywhere", OutputMode: "content"}},
		{"case insensitive", grepArgs{Pattern: "NEEDLE", CaseInsensitive: true}},
		{"glob by extension", grepArgs{Pattern: "needle", Glob: "*.py"}},
		{"glob naming a directory", grepArgs{Pattern: "needle", Glob: "sub/*.md"}},
		{"multiline across lines", grepArgs{Pattern: "alpha.*gamma", Multiline: true, OutputMode: "content"}},
		{"head_limit keeps the first line", grepArgs{Pattern: "needle", OutputMode: "content", HeadLimit: 1}},
		{"a line past the column cap is omitted", grepArgs{Pattern: "x{5}", OutputMode: "content"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := execGrep(t.Context(), withRG, mustJSON(t, tc.args))
			want := execGrep(t.Context(), withWalk, mustJSON(t, tc.args))
			if got.Content != want.Content || got.IsError != want.IsError || got.Truncated != want.Truncated {
				t.Fatalf("the paths disagree for %+v:\nripgrep:  %q (error %v, truncated %v)\nfallback: %q (error %v, truncated %v)",
					tc.args, got.Content, got.IsError, got.Truncated, want.Content, want.IsError, want.Truncated)
			}
		})
	}
}

// TestGrepSkipsVersionControlDirectoriesOnBothPaths pins the one exclusion
// list both paths carry. ripgrep does not skip dot-directories on its own
// once --hidden is passed, so each name has to be named; the walk names the
// same six. A search that returned .git's contents would be reading a
// repository's internals into the model's context on every orienting Grep.
func TestGrepSkipsVersionControlDirectoriesOnBothPaths(t *testing.T) {
	root := newGrepFixture(t)
	mustMkdirAll(t, root, ".git")
	writeFile(t, root, ".git/config.md", "needle git\n")

	paths := map[string]string{"ripgrep": "", "fallback": ""}
	if rg, err := RipgrepPath(""); err == nil && rg != "" {
		paths["ripgrep"] = rg
	}
	for name, rg := range paths {
		if name == "ripgrep" && rg == "" {
			t.Log("no ripgrep found; only the fallback path is exercised")
			continue
		}
		t.Run(name, func(t *testing.T) {
			res := execGrep(t.Context(), executorAt(t, root, rg), mustJSON(t, grepArgs{Pattern: "needle"}))
			if res.IsError {
				t.Fatalf("unexpected error: %s", res.Content)
			}
			for _, line := range strings.Split(res.Content, "\n") {
				if strings.Contains(line, ".git/") || strings.Contains(line, ".svn/") {
					t.Fatalf("%s reported a version-control directory: %q", name, line)
				}
			}
			if !strings.Contains(res.Content, "node_modules/x.js") {
				t.Fatalf("%s did not search node_modules, which ripgrep searches: %q", name, res.Content)
			}
		})
	}
}
