package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobFindsNestedFiles(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a")
	mustMkdirAll(t, root, "sub")
	writeFile(t, root, "sub/b.go", "package sub")
	writeFile(t, root, "c.txt", "not go")

	res := execGlob(t.Context(), e, mustJSON(t, globArgs{Pattern: "**/*.go"}))
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go") || !strings.Contains(res.Content, "sub/b.go") {
		t.Fatalf("expected both go files, got: %s", res.Content)
	}
	if strings.Contains(res.Content, "c.txt") {
		t.Fatalf("did not expect c.txt to match **/*.go, got: %s", res.Content)
	}
}

// TestGlobRejectsAFileRoot proves Glob names a file it was handed as its
// search root rather than answering "no files matched" for a path that is
// sitting there. Glob resolves its root the way Grep does, and its walk
// skips the root entry itself, so a file root can never contribute a match.
func TestGlobRejectsAFileRoot(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a")

	res := execGlob(t.Context(), e, mustJSON(t, globArgs{Pattern: "**/*.go", Path: "a.go"}))
	if !res.IsError {
		t.Fatalf("expected an error for a file root, got: %s", res.Content)
	}
	if !strings.Contains(res.Content, "a.go") {
		t.Fatalf("expected the error to name the path, got: %s", res.Content)
	}
}

func TestGrepFindsMatchesAndRespectsOutputMode(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a\nfunc Foo() {}\n")
	writeFile(t, root, "b.go", "package b\n")

	files := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "Foo"}))
	if !strings.Contains(files.Content, "a.go") || strings.Contains(files.Content, "b.go") {
		t.Fatalf("expected only a.go in files_with_matches, got: %s", files.Content)
	}

	content := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "Foo", OutputMode: "content"}))
	if !strings.Contains(content.Content, "a.go:func Foo") {
		t.Fatalf("expected a path:text match with no line number by default, got: %s", content.Content)
	}

	numbered := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "Foo", OutputMode: "content", ShowLineNumbers: true}))
	if !strings.Contains(numbered.Content, "a.go:2:") {
		t.Fatalf("expected a path:line:text match with -n, got: %s", numbered.Content)
	}
}

// TestGrepContextLinesAndHeadLimit exercises -A/-B/-C and head_limit, the
// two additions content mode's default shape does not exercise on its own.
func TestGrepContextLinesAndHeadLimit(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a\n\nfunc Foo() {}\n\nfunc Bar() {}\n")

	ctx := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "Foo", OutputMode: "content", ContextBefore: 1, ContextAfter: 1, ShowLineNumbers: true}))
	for _, want := range []string{"a.go-2-", "a.go:3:func Foo", "a.go-4-"} {
		if !strings.Contains(ctx.Content, want) {
			t.Fatalf("expected %q in context output, got: %s", want, ctx.Content)
		}
	}

	limited := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "func", OutputMode: "content", HeadLimit: 1}))
	if strings.Count(limited.Content, "\n") != 0 || !strings.Contains(limited.Content, "Foo") {
		t.Fatalf("expected head_limit 1 to keep only the first match line, got: %s", limited.Content)
	}
}

// TestGrepCaseInsensitive exercises -i, which widens what the default
// pattern-only search finds. `type` is not here: it is ripgrep's own table of
// names, so the ripgrep path carries it (TestGrepRipgrepUsesRipgrepsTypeList)
// and the fallback refuses it (TestGrepFallbackRefusesTypeWithoutRipgrep).
func TestGrepCaseInsensitive(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "package a\nfunc FOO() {}\n")
	writeFile(t, root, "b.py", "def foo(): pass\n")

	insensitive := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "foo", CaseInsensitive: true}))
	if !strings.Contains(insensitive.Content, "a.go") {
		t.Fatalf("expected -i to match FOO against foo, got: %s", insensitive.Content)
	}
}

// TestGrepSingleFilePathLabelsMatches proves a search rooted at one file
// searches that file, and reports it the way ripgrep does. A directory root
// reports each match under the path the caller spelled and the file's path
// beneath it; a single file reports the path the caller gave, which is what
// it can open. Content mode is the exception: ripgrep prints no path when the
// search named one file, so a caller that named it gets the lines alone.
func TestGrepSingleFilePathLabelsMatches(t *testing.T) {
	e, root := newTestExecutor(t)
	mustMkdirAll(t, root, "projects")
	writeFile(t, root, "projects/DESIGN-NOTES.md", "intro\nneedle here\n")
	writeFile(t, root, "projects/DESIGN-LOG.md", "needle elsewhere\n")

	files := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle", Path: "projects/DESIGN-NOTES.md"}))
	if files.Content != "projects/DESIGN-NOTES.md" {
		t.Fatalf("expected files_with_matches to name the file searched, got: %q", files.Content)
	}

	content := execGrep(t.Context(), e, mustJSON(t, grepArgs{
		Pattern: "needle", Path: "projects/DESIGN-NOTES.md", OutputMode: "content", ShowLineNumbers: true}))
	if content.Content != "2:needle here" {
		t.Fatalf("expected content on one file to carry no path, as ripgrep prints none, got: %q", content.Content)
	}

	counted := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle", Path: "projects/DESIGN-NOTES.md", OutputMode: "count"}))
	if counted.Content != "projects/DESIGN-NOTES.md:1" {
		t.Fatalf("expected count to name the file searched, got: %q", counted.Content)
	}

	// An absolute path is reported as the caller spelled it, the way
	// ripgrep's own CLI does.
	abs := filepath.Join(root, "projects", "DESIGN-NOTES.md")
	absolute := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle", Path: abs}))
	if absolute.Content != filepath.ToSlash(abs) {
		t.Fatalf("expected an absolute search to report the path given, got: %q", absolute.Content)
	}

	// A directory root reports the caller's spelling and the file's path
	// beneath it, so the path a match is reported under is one the caller can
	// hand to Read without re-prefixing it.
	dir := execGrep(t.Context(), e, mustJSON(t, grepArgs{Pattern: "needle", Path: "projects"}))
	if dir.Content != "projects/DESIGN-LOG.md\nprojects/DESIGN-NOTES.md" {
		t.Fatalf("expected a directory search to report paths under it, got: %q", dir.Content)
	}
}

func TestListShowsEntriesAndRespectsIgnore(t *testing.T) {
	e, root := newTestExecutor(t)
	writeFile(t, root, "a.go", "x")
	writeFile(t, root, "a_test.go", "x")
	mustMkdirAll(t, root, "sub")

	res := execList(t.Context(), e, mustJSON(t, listArgs{Path: "."}))
	if !strings.Contains(res.Content, "a.go") || !strings.Contains(res.Content, "sub/") {
		t.Fatalf("expected a.go and sub/, got: %s", res.Content)
	}

	filtered := execList(t.Context(), e, mustJSON(t, listArgs{Path: ".", Ignore: []string{"*_test.go"}}))
	if strings.Contains(filtered.Content, "a_test.go") {
		t.Fatalf("expected a_test.go to be ignored, got: %s", filtered.Content)
	}
}

func TestTaskCreateValidatesBeforeMutating(t *testing.T) {
	e, _ := newTestExecutor(t)

	// Validation happens before any mutation: a bad item anywhere rejects the
	// whole call and nothing is stored.
	bad := execTaskCreate(t.Context(), e, mustJSON(t, taskCreateArgs{Tasks: []taskCreateItem{{Subject: "x", Description: "y", Status: "bogus", ActiveForm: "Xing"}}}))
	if !bad.IsError {
		t.Fatal("expected an error for an invalid status")
	}
	if len(e.Todos()) != 0 {
		t.Fatalf("a rejected call must not mutate the plan, got %d tasks", len(e.Todos()))
	}
	badSubject := execTaskCreate(t.Context(), e, mustJSON(t, taskCreateArgs{Tasks: []taskCreateItem{
		{Subject: "ok", Description: "d", ActiveForm: "Oking"},
		{Subject: "", Description: "d2", ActiveForm: "Eing"},
	}}))
	if !badSubject.IsError {
		t.Fatal("expected an error for an empty subject")
	}
	badDescription := execTaskCreate(t.Context(), e, mustJSON(t, taskCreateArgs{Tasks: []taskCreateItem{
		{Subject: "ok", Description: "d", ActiveForm: "Oking"},
		{Subject: "fine", Description: "", ActiveForm: "Eing"},
	}}))
	if !badDescription.IsError {
		t.Fatal("expected an error for an empty description")
	}
	if len(e.Todos()) != 0 {
		t.Fatalf("a rejected call must not mutate the plan, got %d tasks", len(e.Todos()))
	}

	good := execTaskCreate(t.Context(), e, mustJSON(t, taskCreateArgs{Tasks: []taskCreateItem{
		{Subject: "Run tests", Description: "Run the whole suite", ActiveForm: "Running tests"},
		{Subject: "Push", Description: "Push the branch", Status: "completed", ActiveForm: "Pushing"},
	}}))
	if good.IsError {
		t.Fatalf("unexpected error: %s", good.Content)
	}
	todos := e.Todos()
	if len(todos) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(todos))
	}
	if todos[0].TaskID != "1" || todos[1].TaskID != "2" {
		t.Fatalf("expected ids minted in array order, got %q and %q", todos[0].TaskID, todos[1].TaskID)
	}
	if todos[0].Status != "pending" {
		t.Fatalf("expected an omitted status to default to pending, got %q", todos[0].Status)
	}
	if todos[0].Subject != "Run tests" || todos[0].Description != "Run the whole suite" {
		t.Fatalf("expected subject and description stored, got %+v", todos[0])
	}
	if !strings.Contains(good.Content, "[ ] #1 Run tests") || !strings.Contains(good.Content, "[x] #2 Push") {
		t.Fatalf("expected a rendered checklist with ids, got: %s", good.Content)
	}
}

func TestTaskGetFindsById(t *testing.T) {
	e, _ := newTestExecutor(t)
	execTaskCreate(t.Context(), e, mustJSON(t, taskCreateArgs{Tasks: []taskCreateItem{{Subject: "Fix bug", Description: "Find and fix the leak", ActiveForm: "Fixing bug"}}}))

	got := execTaskGet(t.Context(), e, mustJSON(t, taskGetArgs{TaskID: "1"}))
	if got.IsError {
		t.Fatalf("unexpected error: %s", got.Content)
	}
	if !strings.Contains(got.Content, "[ ] #1 Fix bug") {
		t.Fatalf("expected the rendered task, got: %s", got.Content)
	}
	if !strings.Contains(got.Content, "Description: Find and fix the leak") {
		t.Fatalf("expected the task's description in the result, got: %s", got.Content)
	}

	missing := execTaskGet(t.Context(), e, mustJSON(t, taskGetArgs{TaskID: "99"}))
	if !missing.IsError || !strings.Contains(missing.Content, "99") {
		t.Fatalf("expected an error naming the missing id, got: %+v", missing)
	}
}

func TestTaskListFiltersByStatus(t *testing.T) {
	e, _ := newTestExecutor(t)
	execTaskCreate(t.Context(), e, mustJSON(t, taskCreateArgs{Tasks: []taskCreateItem{
		{Subject: "Read spec", Description: "Read the spec", ActiveForm: "Reading spec"},
		{Subject: "Wire it", Description: "Wire it up", Status: "in_progress", ActiveForm: "Wiring"},
		{Subject: "Push", Description: "Push the branch", ActiveForm: "Pushing"},
	}}))

	all := execTaskList(t.Context(), e, mustJSON(t, taskListArgs{}))
	if all.IsError {
		t.Fatalf("unexpected error: %s", all.Content)
	}
	if !strings.Contains(all.Content, "#1 Read spec") || !strings.Contains(all.Content, "#2 Wire it") || !strings.Contains(all.Content, "#3 Push") {
		t.Fatalf("expected the whole checklist, got: %s", all.Content)
	}

	inProgress := execTaskList(t.Context(), e, mustJSON(t, taskListArgs{Status: "in_progress"}))
	if inProgress.IsError || !strings.Contains(inProgress.Content, "#2 Wire it") || strings.Contains(inProgress.Content, "#1 Read spec") {
		t.Fatalf("expected only the in_progress task, got: %s", inProgress.Content)
	}

	none := execTaskList(t.Context(), e, mustJSON(t, taskListArgs{Status: "completed"}))
	if none.IsError || none.Content != "No tasks." {
		t.Fatalf("expected No tasks., got: %q", none.Content)
	}
}

func TestTaskUpdatePatchesAndDeletes(t *testing.T) {
	e, _ := newTestExecutor(t)
	execTaskCreate(t.Context(), e, mustJSON(t, taskCreateArgs{Tasks: []taskCreateItem{
		{Subject: "Fix bug", Description: "Fix the leak", ActiveForm: "Fixing bug"},
		{Subject: "Push", Description: "Push the branch", ActiveForm: "Pushing"},
	}}))

	updated := execTaskUpdate(t.Context(), e, mustJSON(t, taskUpdateArgs{TaskID: "1", Status: "in_progress", Subject: "Fix the bug", Description: "Find and fix the leak"}))
	if updated.IsError {
		t.Fatalf("unexpected error: %s", updated.Content)
	}
	if !strings.Contains(updated.Content, "[~] #1 Fix the bug") {
		t.Fatalf("expected the patched line, got: %s", updated.Content)
	}
	todos := e.Todos()
	if todos[0].Status != "in_progress" || todos[0].Subject != "Fix the bug" || todos[0].Description != "Find and fix the leak" || todos[0].ActiveForm != "Fixing bug" {
		t.Fatalf("expected only the present fields patched, got %+v", todos[0])
	}

	// status: "deleted" removes the task exactly as the old delete flag did.
	deleted := execTaskUpdate(t.Context(), e, mustJSON(t, taskUpdateArgs{TaskID: "1", Status: "deleted"}))
	if deleted.IsError {
		t.Fatalf("unexpected error: %s", deleted.Content)
	}
	if deleted.Content != "Task #1 deleted." {
		t.Fatalf("expected the delete message, got: %s", deleted.Content)
	}
	todos = e.Todos()
	if len(todos) != 1 || todos[0].TaskID != "2" {
		t.Fatalf("expected the remaining task to keep its id and order, got %+v", todos)
	}
}

func TestTaskUpdateRejectsBadArgs(t *testing.T) {
	e, _ := newTestExecutor(t)
	execTaskCreate(t.Context(), e, mustJSON(t, taskCreateArgs{Tasks: []taskCreateItem{{Subject: "Fix bug", Description: "Fix the leak", ActiveForm: "Fixing bug"}}}))

	if res := execTaskUpdate(t.Context(), e, mustJSON(t, taskUpdateArgs{TaskID: "1", Status: "deleted", Subject: "renamed"})); !res.IsError {
		t.Fatal("expected status \"deleted\" combined with a subject to be rejected")
	}
	if res := execTaskUpdate(t.Context(), e, mustJSON(t, taskUpdateArgs{TaskID: "1", Status: "deleted", Description: "changed"})); !res.IsError {
		t.Fatal("expected status \"deleted\" combined with a description to be rejected")
	}
	if res := execTaskUpdate(t.Context(), e, mustJSON(t, taskUpdateArgs{TaskID: "1", Status: "deleted", ActiveForm: "Renaming"})); !res.IsError {
		t.Fatal("expected status \"deleted\" combined with activeForm to be rejected")
	}
	if res := execTaskUpdate(t.Context(), e, mustJSON(t, taskUpdateArgs{TaskID: "1"})); !res.IsError {
		t.Fatal("expected an update with nothing to set to be rejected")
	}
	if res := execTaskUpdate(t.Context(), e, mustJSON(t, taskUpdateArgs{TaskID: "99", Status: "completed"})); !res.IsError || !strings.Contains(res.Content, "99") {
		t.Fatal("expected an unknown id to be rejected and named")
	}
	if len(e.Todos()) != 1 {
		t.Fatalf("rejected calls must not mutate the plan, got %d tasks", len(e.Todos()))
	}
}

func TestCompleteValidatesAgainstResultSchema(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.ResultSchema = json.RawMessage(`{
		"type": "object",
		"properties": {"count": {"type": "integer"}},
		"required": ["count"],
		"additionalProperties": false
	}`)

	res, _, ok := e.execComplete(mustJSON(t, completeArgs{Summary: "done", Result: json.RawMessage(`{"count": "not a number"}`)}))
	if ok {
		t.Fatal("expected schema validation to fail")
	}
	if !res.IsError || !strings.Contains(res.Content, "count") {
		t.Fatalf("expected a schema error naming count, got: %s", res.Content)
	}

	res2, payload, ok2 := e.execComplete(mustJSON(t, completeArgs{Summary: "done", Result: json.RawMessage(`{"count": 3}`), Status: "done"}))
	if !ok2 {
		t.Fatalf("expected valid payload to succeed: %s", res2.Content)
	}
	if payload.Summary != "done" || payload.Status != "done" {
		t.Fatalf("unexpected payload: %+v", payload)
	}
}

// The failure this covers is one a review found: the schema's
// fields sent as top-level arguments beside status, with no result at all.
// The validator's own message is accurate but describes what is absent, so
// the hint has to name what is present.
func TestCompleteNamesFlattenedResultFields(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.ResultSchema = json.RawMessage(`{
		"type": "object",
		"properties": {
			"summary": {"type": "string"},
			"branch": {"type": "string"},
			"files_changed": {"type": "array", "items": {"type": "string"}}
		},
		"required": ["summary", "branch"]
	}`)

	res, _, ok := e.execComplete(json.RawMessage(
		`{"summary": "did the thing", "status": "done", "branch": "feat/x", "files_changed": ["a.go"]}`))
	if ok {
		t.Fatal("expected the flattened call to be rejected")
	}
	if !strings.Contains(res.Content, "expected object, got null") {
		t.Fatalf("the schema error must survive alongside the hint, got: %s", res.Content)
	}
	for _, want := range []string{`"branch"`, `"files_changed"`, "nest those fields inside result"} {
		if !strings.Contains(res.Content, want) {
			t.Fatalf("hint must contain %s, got: %s", want, res.Content)
		}
	}
	// summary is one of Complete's own arguments, so its presence at the top
	// level is correct and must not be reported as a stray.
	if strings.Contains(res.Content, `"summary" and`) || strings.Contains(res.Content, `"summary",`) {
		t.Fatalf("summary is a Complete argument, not a stray: %s", res.Content)
	}
}

// A result that is present but wrong is a different mistake, and the hint
// would be actively misleading about it.
func TestCompleteHintOnlyFiresOnMissingResult(t *testing.T) {
	e, _ := newTestExecutor(t)
	e.ResultSchema = json.RawMessage(`{
		"type": "object",
		"properties": {"branch": {"type": "string"}},
		"required": ["branch"]
	}`)

	res, _, ok := e.execComplete(json.RawMessage(
		`{"summary": "s", "result": {"branch": 7}, "branch": "feat/x"}`))
	if ok {
		t.Fatal("expected schema validation to fail")
	}
	if strings.Contains(res.Content, "nest those fields inside result") {
		t.Fatalf("hint must not fire when result was sent: %s", res.Content)
	}
}

func mustMkdirAll(t *testing.T, root, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
		t.Fatal(err)
	}
}
