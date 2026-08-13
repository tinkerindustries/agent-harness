package tools

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// runTool drives one tool call the way the executor does — through
// Execute's policy check and toolFuncs dispatch rather than the exec
// function directly — so a tool that stops resolving paths is caught here,
// not just in ResolvePath's own tests.
func runTool(t *testing.T, e *Executor, name string, args any) Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	outcome := e.Execute(t.Context(), wire.ToolCall{
		ID:   "call_test",
		Type: "function",
		Function: wire.ToolCallFunc{
			Name:      name,
			Arguments: string(raw),
		},
	})
	if outcome.Denied {
		t.Fatalf("unexpected denial: %s", outcome.Rule)
	}
	return outcome.Result
}

// markReadPath records path and its symlink-resolved spelling as read this
// session, so an Edit under test reaches its write regardless of which
// spelling the implementation checks.
func markReadPath(e *Executor, path string) {
	e.markRead(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil && resolved != path {
		e.markRead(resolved)
	}
}

// assertFileState fails unless target still holds want; a nil want means the
// file must not exist at all.
func assertFileState(t *testing.T, target string, want []byte) {
	t.Helper()
	if want == nil {
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Fatalf("expected %s not to exist", target)
		}
		return
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s changed: got %q, want %q", target, got, want)
	}
}

// TestFileToolsContainToWorkspace closes the gap workspace_test.go leaves
// open: ResolvePath's own tests prove it rejects escapes, but nothing proved
// every file tool actually calls it. Each tool is driven through the
// executor against a relative escape, an absolute path outside the
// workspace, and a symlink out of it, and asserted to error without any
// filesystem effect.
func TestFileToolsContainToWorkspace(t *testing.T) {
	cases := []struct {
		name string
		// check runs one escape kind against the tool and asserts the error
		// and the absence of any filesystem effect.
		check func(t *testing.T, e *Executor, root, outside, kind string)
		// succeed runs the tool on an ordinary in-workspace path, so the
		// test cannot pass by rejecting everything.
		succeed func(t *testing.T, e *Executor, root string)
	}{
		{
			name: "Read",
			check: func(t *testing.T, e *Executor, root, outside, kind string) {
				path := "../outside.txt"
				switch kind {
				case "absolute":
					path = "/etc/passwd"
				case "symlink":
					path = "escape/secret.txt"
				}
				switch kind {
				case "relative":
					writeFile(t, filepath.Dir(root), "outside.txt", "x")
				case "symlink":
					writeFile(t, outside, "secret.txt", "x")
				}
				if res := runTool(t, e, "Read", readArgs{FilePath: path}); !res.IsError {
					t.Fatalf("expected an error for %q, got: %s", path, res.Content)
				}
			},
			succeed: func(t *testing.T, e *Executor, root string) {
				writeFile(t, root, "a.txt", "hi")
				if res := runTool(t, e, "Read", readArgs{FilePath: "a.txt"}); res.IsError {
					t.Fatalf("Read rejected an in-workspace file: %s", res.Content)
				}
			},
		},
		{
			name: "Write",
			check: func(t *testing.T, e *Executor, root, outside, kind string) {
				path := "../outside.txt"
				var want []byte // nil: the target must not exist afterwards
				switch kind {
				case "absolute":
					path = "/etc/passwd"
					var err error
					want, err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
				case "symlink":
					path = "escape/new.txt"
				}
				res := runTool(t, e, "Write", writeArgs{FilePath: path, Content: "x"})
				if !res.IsError {
					t.Fatalf("expected an error for %q, got: %s", path, res.Content)
				}
				target := filepath.Join(filepath.Dir(root), "outside.txt")
				switch kind {
				case "symlink":
					target = filepath.Join(outside, "new.txt")
				case "absolute":
					target = path
				}
				assertFileState(t, target, want)
			},
			succeed: func(t *testing.T, e *Executor, root string) {
				if res := runTool(t, e, "Write", writeArgs{FilePath: "new.txt", Content: "hi"}); res.IsError {
					t.Fatalf("Write rejected an in-workspace path: %s", res.Content)
				}
				assertFileState(t, filepath.Join(root, "new.txt"), []byte("hi"))
			},
		},
		{
			name: "Edit",
			check: func(t *testing.T, e *Executor, root, outside, kind string) {
				path := "../outside.txt"
				target := filepath.Join(filepath.Dir(root), "outside.txt")
				original := []byte("old")
				switch kind {
				case "absolute":
					path, target = "/etc/passwd", "/etc/passwd"
					var err error
					original, err = os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
				case "symlink":
					path = "escape/secret.txt"
					target = filepath.Join(outside, "secret.txt")
				}
				if kind != "absolute" {
					if err := os.WriteFile(target, original, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				// The target counts as read this session so a containment
				// regression that reaches the write is caught by the file-state
				// assertion rather than masked by Edit's prior-read gate. The
				// absolute case is deliberately not marked read: /etc/passwd
				// must stay untouched even against a broken implementation.
				markReadPath(e, target)
				if kind == "symlink" {
					markReadPath(e, filepath.Join(root, "escape", "secret.txt"))
				}
				res := runTool(t, e, "Edit", editArgs{FilePath: path, OldString: "old", NewString: "new"})
				if !res.IsError {
					t.Fatalf("expected an error for %q, got: %s", path, res.Content)
				}
				assertFileState(t, target, original)
			},
			succeed: func(t *testing.T, e *Executor, root string) {
				writeFile(t, root, "a.txt", "old")
				if res := runTool(t, e, "Read", readArgs{FilePath: "a.txt"}); res.IsError {
					t.Fatalf("Read failed before Edit: %s", res.Content)
				}
				res := runTool(t, e, "Edit", editArgs{FilePath: "a.txt", OldString: "old", NewString: "new"})
				if res.IsError {
					t.Fatalf("Edit rejected an in-workspace file: %s", res.Content)
				}
				assertFileState(t, filepath.Join(root, "a.txt"), []byte("new"))
			},
		},
		{
			name: "List",
			check: func(t *testing.T, e *Executor, root, outside, kind string) {
				path := "../outside.txt"
				switch kind {
				case "absolute":
					path = "/etc/passwd"
				case "symlink":
					path = "escape"
				}
				switch kind {
				case "relative":
					dir := filepath.Join(filepath.Dir(root), "outside.txt")
					if err := os.MkdirAll(filepath.Join(dir, "entry"), 0o755); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					writeFile(t, outside, "entry.txt", "x")
				}
				if res := runTool(t, e, "List", listArgs{Path: path}); !res.IsError {
					t.Fatalf("expected an error for %q, got: %s", path, res.Content)
				}
			},
			succeed: func(t *testing.T, e *Executor, root string) {
				writeFile(t, root, "a.txt", "x")
				if res := runTool(t, e, "List", listArgs{Path: "."}); res.IsError {
					t.Fatalf("List rejected an in-workspace path: %s", res.Content)
				}
			},
		},
		{
			name: "Glob",
			check: func(t *testing.T, e *Executor, root, outside, kind string) {
				path := "../outside.txt"
				switch kind {
				case "absolute":
					path = "/etc/passwd"
				case "symlink":
					path = "escape/secret.txt"
				}
				switch kind {
				case "relative":
					writeFile(t, filepath.Dir(root), "outside.txt", "x")
				case "symlink":
					writeFile(t, outside, "secret.txt", "x")
				}
				if res := runTool(t, e, "Glob", globArgs{Pattern: "**/*", Path: path}); !res.IsError {
					t.Fatalf("expected an error for %q, got: %s", path, res.Content)
				}
			},
			succeed: func(t *testing.T, e *Executor, root string) {
				writeFile(t, root, "a.txt", "x")
				res := runTool(t, e, "Glob", globArgs{Pattern: "**/*"})
				if res.IsError {
					t.Fatalf("Glob rejected an in-workspace path: %s", res.Content)
				}
			},
		},
		{
			name: "Grep",
			check: func(t *testing.T, e *Executor, root, outside, kind string) {
				path := "../outside.txt"
				switch kind {
				case "absolute":
					path = "/etc/passwd"
				case "symlink":
					path = "escape/secret.txt"
				}
				switch kind {
				case "relative":
					writeFile(t, filepath.Dir(root), "outside.txt", "needle")
				case "symlink":
					writeFile(t, outside, "secret.txt", "needle")
				}
				if res := runTool(t, e, "Grep", grepArgs{Pattern: "needle", Path: path}); !res.IsError {
					t.Fatalf("expected an error for %q, got: %s", path, res.Content)
				}
			},
			succeed: func(t *testing.T, e *Executor, root string) {
				writeFile(t, root, "a.txt", "needle")
				if res := runTool(t, e, "Grep", grepArgs{Pattern: "needle"}); res.IsError {
					t.Fatalf("Grep rejected an in-workspace path: %s", res.Content)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// root is a subdirectory of a private base so the relative escape
			// target (../outside.txt) is unique to this tool and cannot collide
			// with another tool's.
			base := t.TempDir()
			root := filepath.Join(base, "ws")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			e, err := NewExecutor(root, &Policy{Mode: ModeFull})
			if err != nil {
				t.Fatal(err)
			}
			outside := t.TempDir()

			for _, kind := range []string{"relative", "absolute", "symlink"} {
				t.Run(kind, func(t *testing.T) {
					if kind == "symlink" {
						if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
							t.Skipf("symlinks unavailable: %v", err)
						}
					}
					tc.check(t, e, root, outside, kind)
				})
			}

			t.Run("in-workspace", func(t *testing.T) {
				tc.succeed(t, e, root)
			})
		})
	}
}
