package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// TestCreateInsertsCreatingRow pins Runner.Create, the worker's pre-creation
// of the session row: the row exists as "creating" with the workspace path
// the worker is about to clone into and the metadata the request already
// carries, while system_prompt and tool_schema stay empty — neither is
// resolvable until the run starts.
func TestCreateInsertsCreatingRow(t *testing.T) {
	srv := plainAnswerServer(t, "unused")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	err := r.Create(t.Context(), RunOptions{
		SessionID:       "sess-create-test",
		Model:           "test-model",
		Effort:          wire.EffortMax,
		Thinking:        true,
		Workspace:       "/ws/sess-create-test",
		PermissionMode:  tools.ModeReadOnly,
		Deny:            []string{"git push"},
		Prompt:          "build the thing",
		JobType:         agentmeta.JobTypeOrchestration,
		ParentAgentType: "claude-code",
		ParentAgentID:   "parent-1",
		ParentIsUser:    true,
		Phase:           2,
		TotalPhases:     4,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	sess, err := r.Store.GetSession(t.Context(), "sess-create-test")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusCreating {
		t.Fatalf("expected status creating, got %q", sess.Status)
	}
	if sess.Workspace != "/ws/sess-create-test" {
		t.Fatalf("expected the about-to-clone workspace, got %q", sess.Workspace)
	}
	if sess.SystemPrompt != "" || len(sess.ToolSchema) != 0 {
		t.Fatalf("expected empty system_prompt and tool_schema before the run starts, got %q / %q", sess.SystemPrompt, sess.ToolSchema)
	}
	if sess.Model != "test-model" || sess.Effort != wire.EffortMax || !sess.Thinking {
		t.Fatalf("expected the request's model/effort/thinking, got %+v", sess)
	}
	if sess.Task != "build the thing" || sess.JobType != agentmeta.JobTypeOrchestration {
		t.Fatalf("expected the request's task and job type, got %+v", sess)
	}
	if sess.ParentAgentType != "claude-code" || sess.ParentAgentID != "parent-1" || !sess.ParentIsUser {
		t.Fatalf("expected the request's parent fields, got %+v", sess)
	}
	if sess.Phase != 2 || sess.TotalPhases != 4 {
		t.Fatalf("expected the request's phase fields, got %+v", sess)
	}
	if len(sess.DenyPatterns) != 1 || sess.DenyPatterns[0] != "git push" {
		t.Fatalf("expected the request's deny patterns, got %+v", sess.DenyPatterns)
	}
	if sess.PermissionMode != string(tools.ModeReadOnly) {
		t.Fatalf("expected the request's permission mode, got %q", sess.PermissionMode)
	}
}

// TestRunPromotesPreCreatedRow is the worker path end to end: the row exists
// as "creating" (Runner.Create ran before workspace preparation), and
// Runner.Run must promote it to "running" and write the three columns that
// are only resolvable now — not insert a second row, which would fail on the
// primary key. The run itself proceeds normally to a terminal result.
func TestRunPromotesPreCreatedRow(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	if err := r.Create(t.Context(), RunOptions{
		SessionID: "sess-promote-test", Model: "test-model", Effort: wire.EffortHigh,
		Thinking: true, Workspace: filepath.Join(t.TempDir(), "sess-promote-test"),
		PermissionMode: tools.ModeFull, Prompt: "do it",
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		SessionID: "sess-promote-test", Model: "test-model", Effort: wire.EffortHigh,
		Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "do it",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
	}

	sess, err := r.Store.GetSession(t.Context(), "sess-promote-test")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusOK {
		t.Fatalf("expected the run to finish ok, got %q", sess.Status)
	}
	// The promotion happened: version 1 (create) → 2 (promote) → 3 (finish),
	// where an inserted row would read 1 → 2.
	if sess.Version != 3 {
		t.Fatalf("expected version 3 (create, promote, finish), got %d", sess.Version)
	}
	// The three columns only resolvable once the run starts were written by
	// the promotion.
	if sess.Workspace == "" || sess.Workspace != mustEvalSymlinks(t, ws) {
		t.Fatalf("expected the prepared workspace on the promoted row, got %q", sess.Workspace)
	}
	if !strings.Contains(sess.SystemPrompt, "headless coding agent") {
		t.Fatalf("expected the rendered system prompt on the promoted row, got %q", sess.SystemPrompt)
	}
	var schema []json.RawMessage
	if err := json.Unmarshal(sess.ToolSchema, &schema); err != nil || len(schema) == 0 {
		t.Fatalf("expected the tool schema on the promoted row, got %q", sess.ToolSchema)
	}
}

// TestRunInsertsWhenNoRowExists pins the other half of promote-or-insert:
// a caller with no pre-created row (harness run, harness resume, the Task
// subagent path, compaction) gets the row inserted exactly as before — as
// running, not creating.
func TestRunInsertsWhenNoRowExists(t *testing.T) {
	srv := plainAnswerServer(t, "all done")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	ws := t.TempDir()
	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: ws, PermissionMode: tools.ModeFull, Prompt: "do it",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusOK {
		t.Fatalf("expected the run to finish ok, got %q", sess.Status)
	}
	// The row was inserted, not promoted: version 1 at insert, 2 at finish —
	// promotion would have added a bump in between.
	if sess.Version != 2 {
		t.Fatalf("expected version 2 (insert, finish), got %d", sess.Version)
	}
	if sess.SystemPrompt == "" || len(sess.ToolSchema) == 0 {
		t.Fatalf("expected system_prompt and tool_schema on an inserted row, got %q / %q", sess.SystemPrompt, sess.ToolSchema)
	}
}

// TestResumeRefusesCreating pins the resume fence for the preparation
// window: a "creating" session has no loop to resume, so Resume refuses it
// exactly as it refuses a running one.
func TestResumeRefusesCreating(t *testing.T) {
	srv := plainAnswerServer(t, "unused")
	defer srv.Close()
	r := newTestRunner(t, srv.URL)

	if err := r.Create(t.Context(), RunOptions{
		SessionID: "sess-resume-creating", Model: "test-model", Effort: wire.EffortHigh,
		Thinking: true, Workspace: "/ws/sess-resume-creating", PermissionMode: tools.ModeFull,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := r.Resume(t.Context(), ResumeOptions{SessionID: "sess-resume-creating"})
	if err == nil || !strings.Contains(err.Error(), "still being prepared") {
		t.Fatalf("expected Resume to refuse a creating session, got %v", err)
	}
	// The row is untouched.
	sess, err := r.Store.GetSession(context.Background(), "sess-resume-creating")
	if err != nil {
		t.Fatal(err)
	}
	if sess.Status != store.StatusCreating {
		t.Fatalf("expected the row to stay creating after a refused resume, got %q", sess.Status)
	}
}
