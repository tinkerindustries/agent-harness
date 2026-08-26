package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// startedPayloadOf runs one session against srvURL in ws and returns the
// session_started payload the run stored.
func startedPayloadOf(t *testing.T, srvURL, ws string) store.SessionStartedPayload {
	t.Helper()
	r := newTestRunner(t, srvURL)
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var started store.SessionStartedPayload
	if err := json.Unmarshal(events[0].Payload, &started); err != nil {
		t.Fatal(err)
	}
	return started
}

// A skill shipped in a cloned repository reaches the model through the
// opening message, ahead of the task text.
func TestRunListsWorkspaceSkills(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	ws := t.TempDir()
	dir := filepath.Join(ws, "myrepo", ".claude", "skills", "test-runner")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: test-runner\ndescription: Run this repository's suite the way CI does.\n---\n\nRun scripts/test.sh.\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	started := startedPayloadOf(t, srv.URL, ws)
	opening := started.OpeningMessage
	wantPath := "myrepo/.claude/skills/test-runner/SKILL.md"
	if !strings.Contains(opening, wantPath) {
		t.Fatalf("opening message does not name the skill file:\n%s", opening)
	}
	if !strings.Contains(opening, "Run this repository's suite the way CI does.") {
		t.Fatalf("opening message does not carry the skill description:\n%s", opening)
	}
	if strings.Index(opening, wantPath) > strings.Index(opening, "Task:") {
		t.Fatalf("the skill catalogue should precede the task text:\n%s", opening)
	}

	// The browser lifts the catalogue into its own panel by removing this
	// exact substring from the opening message, so it has to be one.
	if started.SkillCatalogue == "" {
		t.Fatal("session_started should carry the catalogue separately")
	}
	if !strings.Contains(opening, started.SkillCatalogue) {
		t.Fatalf("skill_catalogue is not a substring of the opening message:\n%q", started.SkillCatalogue)
	}
}

// A workspace with no skills leaves the opening message exactly as it was
// before skills existed, so the shared prefix is undisturbed (docs/CACHE.md).
func TestRunWithoutSkillsLeavesOpeningMessageUnchanged(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	ws := t.TempDir()
	resolved, err := tools.ResolvePath(ws, ".")
	if err != nil {
		t.Fatal(err)
	}
	started := startedPayloadOf(t, srv.URL, ws)
	if want := RenderOpeningMessage(resolved, "say something", nil, "", "", "", nil); started.OpeningMessage != want {
		t.Fatalf("opening message = %q, want %q", started.OpeningMessage, want)
	}
	if started.SkillCatalogue != "" {
		t.Fatalf("skill_catalogue should be empty, got %q", started.SkillCatalogue)
	}
}
