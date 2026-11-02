package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runApplyPatch sends one patch as the model's call would: a single input
// string.
func runApplyPatch(t *testing.T, e *Executor, patch string) Result {
	t.Helper()
	raw, err := json.Marshal(applyPatchArgs{Input: patch})
	if err != nil {
		t.Fatal(err)
	}
	return execApplyPatch(t.Context(), e, raw)
}

// readAll marks every named file read, the gate apply_patch shares with write
// and edit.
func readAll(t *testing.T, e *Executor, names ...string) {
	t.Helper()
	for _, name := range names {
		if res := execRead(t.Context(), e, mustJSON(t, readArgs{FilePath: name})); res.IsError {
			t.Fatalf("read %s: %s", name, res.Content)
		}
	}
}

// TestApplyPatchAppliesTheGrammar is the grammar's own table: every section
// the format has, a hunk that misses, a path that escapes the workspace and a
// header the parser does not know. Each case gets a fresh workspace, so a
// failure names the one patch that broke.
func TestApplyPatchAppliesTheGrammar(t *testing.T) {
	cases := []struct {
		name    string
		setup   map[string]string
		read    []string
		patch   string
		want    map[string]string
		gone    []string
		absent  string
		wantErr string
	}{
		{
			name:  "add",
			patch: "*** Begin Patch\n*** Add File: notes/todo.txt\n+first\n+second\n*** End Patch\n",
			want:  map[string]string{"notes/todo.txt": "first\nsecond\n"},
		},
		{
			name:    "add refuses a file that already exists",
			setup:   map[string]string{"notes/todo.txt": "old\n"},
			patch:   "*** Begin Patch\n*** Add File: notes/todo.txt\n+new\n*** End Patch\n",
			want:    map[string]string{"notes/todo.txt": "old\n"},
			wantErr: "already exists",
		},
		{
			name:  "update with a bare @@ and context lines",
			setup: map[string]string{"a.go": "package a\n\nfunc f() {}\n"},
			read:  []string{"a.go"},
			patch: "*** Begin Patch\n*** Update File: a.go\n@@\n package a\n \n-func f() {}\n+func g() {}\n*** End Patch\n",
			want:  map[string]string{"a.go": "package a\n\nfunc g() {}\n"},
		},
		{
			name:  "update anchored by the @@ context text",
			setup: map[string]string{"a.go": "package a\n\nfunc h() {\n\tx := 1\n}\n\nfunc keep() {}\n"},
			read:  []string{"a.go"},
			patch: "*** Begin Patch\n*** Update File: a.go\n@@ func h() {\n \tx := 1\n-}\n+\tx := 2\n+}\n*** End Patch\n",
			want:  map[string]string{"a.go": "package a\n\nfunc h() {\n\tx := 1\n\tx := 2\n}\n\nfunc keep() {}\n"},
		},
		{
			name:  "update applies two hunks to one file",
			setup: map[string]string{"a.go": "one\ntwo\nthree\nfour\nfive\n"},
			read:  []string{"a.go"},
			patch: "*** Begin Patch\n*** Update File: a.go\n@@\n one\n-two\n+TWO\n@@\n four\n-five\n+FIVE\n*** End Patch\n",
			want:  map[string]string{"a.go": "one\nTWO\nthree\nfour\nFIVE\n"},
		},
		{
			name:  "update keeps a missing trailing newline missing",
			setup: map[string]string{"a.go": "alpha"},
			read:  []string{"a.go"},
			patch: "*** Begin Patch\n*** Update File: a.go\n@@\n-alpha\n+ALPHA\n*** End Patch\n",
			want:  map[string]string{"a.go": "ALPHA"},
		},
		{
			name:  "a patch written with CRLF line endings",
			setup: map[string]string{"a.go": "alpha\n"},
			read:  []string{"a.go"},
			patch: "*** Begin Patch\r\n*** Update File: a.go\r\n@@\r\n-alpha\r\n+ALPHA\r\n*** End Patch\r\n",
			want:  map[string]string{"a.go": "ALPHA\n"},
		},
		{
			name:  "a bare empty line in a hunk is an empty context line",
			setup: map[string]string{"a.go": "alpha\n\nbravo\n"},
			read:  []string{"a.go"},
			patch: "*** Begin Patch\n*** Update File: a.go\n@@\n alpha\n\n-bravo\n+BRAVO\n*** End Patch\n",
			want:  map[string]string{"a.go": "alpha\n\nBRAVO\n"},
		},
		{
			name:  "update anchored to the end of the file",
			setup: map[string]string{"a.go": "alpha\nbravo\n"},
			read:  []string{"a.go"},
			patch: "*** Begin Patch\n*** Update File: a.go\n@@\n-bravo\n+BRAVO\n*** End of File\n*** End Patch\n",
			want:  map[string]string{"a.go": "alpha\nBRAVO\n"},
		},
		{
			name:    "End of File refuses a hunk that is not at the end",
			setup:   map[string]string{"a.go": "alpha\nbravo\ncharlie\n"},
			read:    []string{"a.go"},
			patch:   "*** Begin Patch\n*** Update File: a.go\n@@\n-alpha\n+ALPHA\n*** End of File\n*** End Patch\n",
			want:    map[string]string{"a.go": "alpha\nbravo\ncharlie\n"},
			wantErr: "does not apply",
		},
		{
			name:  "delete",
			setup: map[string]string{"gone.txt": "bye\n"},
			read:  []string{"gone.txt"},
			gone:  []string{"gone.txt"},
			patch: "*** Begin Patch\n*** Delete File: gone.txt\n*** End Patch\n",
		},
		{
			name:    "delete refuses a file that is not there",
			patch:   "*** Begin Patch\n*** Delete File: gone.txt\n*** End Patch\n",
			wantErr: "file not found",
		},
		{
			name:  "move",
			setup: map[string]string{"old/name.go": "package a\n\nfunc f() {}\n"},
			read:  []string{"old/name.go"},
			patch: "*** Begin Patch\n*** Update File: old/name.go\n*** Move to: new/name.go\n*** End Patch\n",
			want:  map[string]string{"new/name.go": "package a\n\nfunc f() {}\n"},
			gone:  []string{"old/name.go"},
		},
		{
			name:  "move with a hunk",
			setup: map[string]string{"old/name.go": "package a\n\nfunc f() {}\n"},
			read:  []string{"old/name.go"},
			patch: "*** Begin Patch\n*** Update File: old/name.go\n*** Move to: new/name.go\n@@\n-func f() {}\n+func g() {}\n*** End Patch\n",
			want:  map[string]string{"new/name.go": "package a\n\nfunc g() {}\n"},
			gone:  []string{"old/name.go"},
		},
		{
			name:    "move refuses a destination that exists",
			setup:   map[string]string{"old/name.go": "package a\n", "new/name.go": "package b\n"},
			read:    []string{"old/name.go", "new/name.go"},
			patch:   "*** Begin Patch\n*** Update File: old/name.go\n*** Move to: new/name.go\n*** End Patch\n",
			want:    map[string]string{"old/name.go": "package a\n", "new/name.go": "package b\n"},
			wantErr: "destination already exists",
		},
		{
			name:  "multi-file patch",
			setup: map[string]string{"a.go": "alpha\n", "c.txt": "charlie\n"},
			read:  []string{"a.go", "c.txt"},
			patch: "*** Begin Patch\n*** Update File: a.go\n@@\n-alpha\n+ALPHA\n" +
				"*** Add File: b.txt\n+bravo\n*** Delete File: c.txt\n*** End Patch\n",
			want: map[string]string{"a.go": "ALPHA\n", "b.txt": "bravo\n"},
			gone: []string{"c.txt"},
		},
		{
			name:    "a hunk that misses fails and changes nothing",
			setup:   map[string]string{"a.go": "alpha\n"},
			read:    []string{"a.go"},
			patch:   "*** Begin Patch\n*** Update File: a.go\n@@\n-bravo\n+BRAVO\n*** End Patch\n",
			want:    map[string]string{"a.go": "alpha\n"},
			wantErr: "does not apply",
		},
		{
			name:  "a missed hunk in a later file leaves the earlier one untouched",
			setup: map[string]string{"a.go": "alpha\n", "b.go": "bravo\n"},
			read:  []string{"a.go", "b.go"},
			patch: "*** Begin Patch\n*** Update File: a.go\n@@\n-alpha\n+ALPHA\n" +
				"*** Update File: b.go\n@@\n-charlie\n+CHARLIE\n*** End Patch\n",
			want:    map[string]string{"a.go": "alpha\n", "b.go": "bravo\n"},
			wantErr: "does not apply",
		},
		{
			name:    "a path that escapes the workspace",
			patch:   "*** Begin Patch\n*** Add File: ../escape.txt\n+nope\n*** End Patch\n",
			absent:  "../escape.txt",
			wantErr: "escapes workspace",
		},
		{
			name:    "a malformed header",
			setup:   map[string]string{"a.go": "alpha\n"},
			read:    []string{"a.go"},
			patch:   "*** Begin Patch\n*** Update File: a.go\n*** Bogus Header\n*** End Patch\n",
			want:    map[string]string{"a.go": "alpha\n"},
			wantErr: "unrecognised patch header",
		},
		{
			name:    "a patch without End Patch",
			patch:   "*** Begin Patch\n*** Add File: x.txt\n+x\n",
			wantErr: `the last line of a patch must be "*** End Patch"`,
		},
		{
			name:    "a patch without Begin Patch",
			patch:   "*** Add File: x.txt\n+x\n*** End Patch\n",
			wantErr: `the first line of a patch must be "*** Begin Patch"`,
		},
		{
			name:    "an update with neither a hunk nor a move",
			setup:   map[string]string{"a.go": "alpha\n"},
			read:    []string{"a.go"},
			patch:   "*** Begin Patch\n*** Update File: a.go\n*** End Patch\n",
			want:    map[string]string{"a.go": "alpha\n"},
			wantErr: "needs a hunk",
		},
		{
			name:    "a file that has not been read",
			setup:   map[string]string{"a.go": "alpha\n"},
			patch:   "*** Begin Patch\n*** Update File: a.go\n@@\n-alpha\n+ALPHA\n*** End Patch\n",
			want:    map[string]string{"a.go": "alpha\n"},
			wantErr: "has not been read in this session",
		},
		{
			name:    "a delete of a file that has not been read",
			setup:   map[string]string{"a.go": "alpha\n"},
			patch:   "*** Begin Patch\n*** Delete File: a.go\n*** End Patch\n",
			want:    map[string]string{"a.go": "alpha\n"},
			wantErr: "has not been read in this session",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, root := newTestExecutor(t)
			for name, content := range tc.setup {
				if dir := filepath.Dir(filepath.Join(root, name)); dir != root {
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				writeFile(t, root, name, content)
			}
			readAll(t, e, tc.read...)

			res := runApplyPatch(t, e, tc.patch)
			if tc.wantErr == "" {
				if res.IsError {
					t.Fatalf("unexpected error: %s", res.Content)
				}
			} else {
				if !res.IsError {
					t.Fatalf("expected an error containing %q, got: %s", tc.wantErr, res.Content)
				}
				if !strings.Contains(res.Content, tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", res.Content, tc.wantErr)
				}
			}

			for name, want := range tc.want {
				got, err := os.ReadFile(filepath.Join(root, name))
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				if string(got) != want {
					t.Fatalf("%s = %q, want %q", name, got, want)
				}
			}
			for _, name := range tc.gone {
				if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatalf("%s still exists after the patch", name)
				}
			}
			if tc.absent != "" {
				if _, err := os.Stat(filepath.Join(root, tc.absent)); !os.IsNotExist(err) {
					t.Fatalf("%s was created outside the workspace", tc.absent)
				}
			}
		})
	}
}

// TestApplyPatchReportsEveryFileItChanged pins the summary the model reads
// back: each file by the path the patch named, with the operation that
// happened to it.
func TestApplyPatchReportsEveryFileItChanged(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "alpha\n")
	writeFile(t, root, "c.txt", "charlie\n")
	readAll(t, e, "a.go", "c.txt")

	res := runApplyPatch(t, e, "*** Begin Patch\n*** Update File: a.go\n@@\n-alpha\n+ALPHA\n"+
		"*** Add File: b.txt\n+bravo\n*** Delete File: c.txt\n*** End Patch\n")
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	for _, want := range []string{"Applied a patch to 3 files", "- updated a.go", "- added b.txt", "- deleted c.txt"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("result %q does not contain %q", res.Content, want)
		}
	}
}

// TestApplyPatchIsInEveryProviderArray pins the membership the phase asks
// for: the tool sits in the base array, so it reaches Kimi's, Gemini's and
// Claude's arrays, and it survives DeepSeek's seven-name subtraction.
func TestApplyPatchIsInEveryProviderArray(t *testing.T) {
	for _, model := range []string{"deepseek-v4-pro", "deepseek-flash", "kimi-k3", "gemini-3.7-flash", "claude-sonnet-5-5"} {
		found := false
		for _, tool := range DefinitionsFor(model) {
			if tool.Function.Name == "apply_patch" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s's array does not carry apply_patch", model)
		}
	}
}

// TestApplyPatchIsMutatingAndDeniedInReadOnlyMode pins the permission shape
// the plan asks for: like write and edit, apply_patch is never in
// alwaysAllowed, so a read-only session is refused it and a full session is
// allowed it.
func TestApplyPatchIsMutatingAndDeniedInReadOnlyMode(t *testing.T) {
	if alwaysAllowed["apply_patch"] {
		t.Fatal("apply_patch is in alwaysAllowed; it rewrites files and must not be")
	}
	if d := (&Policy{Mode: ModeReadOnly}).Check("apply_patch", "apply_patch"); d.Allow {
		t.Fatalf("read-only mode allowed apply_patch: %s", d.Rule)
	}
	if d := (&Policy{Mode: ModeFull}).Check("apply_patch", "apply_patch"); !d.Allow {
		t.Fatalf("full mode denied apply_patch: %s", d.Rule)
	}
}

// TestMutationTargetsReportsEveryFileAPatchRewrites is the scheduling
// contract: a patch's targets are every file it names, the move destination
// included, so the runner can order it against an edit of any of them.
func TestMutationTargetsReportsEveryFileAPatchRewrites(t *testing.T) {
	root := t.TempDir()
	patch := "*** Begin Patch\n*** Update File: a.go\n*** Move to: moved.go\n*** Add File: b.txt\n+bravo\n" +
		"*** Delete File: c.txt\n*** End Patch\n"
	args, err := json.Marshal(applyPatchArgs{Input: patch})
	if err != nil {
		t.Fatal(err)
	}
	got := MutationTargets(root, "apply_patch", string(args))
	want := []string{"a.go", "moved.go", "b.txt", "c.txt"}
	if len(got) != len(want) {
		t.Fatalf("targets = %v, want %v", got, want)
	}
	for i, name := range want {
		if filepath.Base(got[i]) != name {
			t.Fatalf("target %d = %q, want it to end in %q", i, got[i], name)
		}
	}
	// A patch that does not parse has no targets: the call fails in
	// execution and names the real reason.
	broken, err := json.Marshal(applyPatchArgs{Input: "not a patch"})
	if err != nil {
		t.Fatal(err)
	}
	if got := MutationTargets(root, "apply_patch", string(broken)); got != nil {
		t.Fatalf("a malformed patch reported targets %v", got)
	}
}
