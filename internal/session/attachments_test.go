package session

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// startedPayloadOfRun runs one session and returns the session_started
// payload it stored, the way startedPayloadOf does but with the caller's own
// RunOptions (attachment names among them).
func startedPayloadOfRun(t *testing.T, srvURL, ws string, opts RunOptions) store.SessionStartedPayload {
	t.Helper()
	r := newTestRunner(t, srvURL)
	res, err := r.Run(t.Context(), opts)
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

// TestRunCarriesAttachmentPathsInSessionStarted pins the contract the
// transcript's rendering of task attachments is built on: the run-creating
// session_started payload carries, separately from the message text, the
// paths the request's attachments were materialised under
// (scratch/attachments/<name>), so the browser can address each one on
// GET /api/sessions/{id}/screenshot without parsing the opening message.
func TestRunCarriesAttachmentPathsInSessionStarted(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	started := startedPayloadOfRun(t, srv.URL, t.TempDir(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "say something",
		AttachmentNames: []string{"mockup.png", "light.webp"},
	})
	if started.OpeningMessage == "" {
		t.Fatal("started payload is empty")
	}
	want := []string{"scratch/attachments/mockup.png", "scratch/attachments/light.webp"}
	if len(started.Attachments) != len(want) {
		t.Fatalf("attachments = %q, want %q", started.Attachments, want)
	}
	for i := range want {
		if started.Attachments[i] != want[i] {
			t.Errorf("attachment %d = %q, want %q", i, started.Attachments[i], want[i])
		}
		// The same paths the opening message names, so the browser could
		// subtract them from the text if it ever needed to.
		if !strings.Contains(started.OpeningMessage, "- "+want[i]) {
			t.Errorf("opening message should list %q:\n%s", want[i], started.OpeningMessage)
		}
	}
}

// TestRunWithoutAttachmentsLeavesPayloadFieldAbsent pins that a run with no
// attachments omits the field entirely, the same way skill_catalogue is
// absent for a skill-less workspace.
func TestRunWithoutAttachmentsLeavesPayloadFieldAbsent(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()

	started := startedPayloadOfRun(t, srv.URL, t.TempDir(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "say something",
	})
	if len(started.Attachments) != 0 {
		t.Fatalf("attachments should be empty, got %q", started.Attachments)
	}
	if strings.Contains(started.OpeningMessage, "scratch/attachments") {
		t.Fatalf("opening message should not mention attachments:\n%s", started.OpeningMessage)
	}
}
