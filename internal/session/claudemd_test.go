package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// A CLAUDE.md carried by a cloned repository reaches the model through the
// opening message, between the workspace line and the task text.
func TestRunListsWorkspaceClaudeMD(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(ws, "myrepo"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "# myrepo\n\nThese are the instructions of this repository; follow them.\n"
	if err := os.WriteFile(filepath.Join(ws, "myrepo", "CLAUDE.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	started := startedPayloadOf(t, srv.URL, ws)
	opening := started.OpeningMessage
	wantPath := "myrepo/CLAUDE.md"
	if !strings.Contains(opening, wantPath) {
		t.Fatalf("opening message does not name the CLAUDE.md file:\n%s", opening)
	}
	if !strings.Contains(opening, content) {
		t.Fatalf("opening message does not carry the CLAUDE.md contents:\n%s", opening)
	}
	if strings.Index(opening, wantPath) <= strings.Index(opening, "Workspace:") {
		t.Fatalf("the CLAUDE.md block should follow the workspace line:\n%s", opening)
	}
	if strings.Index(opening, wantPath) > strings.Index(opening, "Task:") {
		t.Fatalf("the CLAUDE.md block should precede the task text:\n%s", opening)
	}

	// Like the skill catalogue, the block rides on session_started as an
	// exact substring of the opening message so a consumer can lift it out
	// without parsing prose.
	if started.ClaudeMDBlock == "" {
		t.Fatal("session_started should carry the CLAUDE.md block separately")
	}
	if !strings.Contains(opening, started.ClaudeMDBlock) {
		t.Fatalf("claude_md_block is not a substring of the opening message:\n%q", started.ClaudeMDBlock)
	}
}

// A workspace with no CLAUDE.md leaves the payload field and the rendered
// block empty, and the opening message exactly as it was before the feature
// existed, so the shared prefix is undisturbed (docs/CACHE.md).
func TestRunWithoutClaudeMDLeavesOpeningMessageUnchanged(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	ws := t.TempDir()
	resolved, err := tools.ResolvePath(ws, ".")
	if err != nil {
		t.Fatal(err)
	}
	started := startedPayloadOf(t, srv.URL, ws)
	if want := RenderOpeningMessage(resolved, "say something", nil, "", ""); started.OpeningMessage != want {
		t.Fatalf("opening message = %q, want %q", started.OpeningMessage, want)
	}
	if started.ClaudeMDBlock != "" {
		t.Fatalf("claude_md_block should be empty, got %q", started.ClaudeMDBlock)
	}
}
