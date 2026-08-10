package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateAndGetSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	sess := Session{
		ID:             "sess-1",
		Model:          "deepseek-v4-pro",
		Effort:         "high",
		Thinking:       true,
		Workspace:      "/tmp/ws",
		PermissionMode: "default",
		DenyPatterns:   []string{"git push"},
		SystemPrompt:   "you are an agent",
		ToolSchema:     json.RawMessage(`[{"type":"function"}]`),
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	got, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Model != sess.Model || got.SystemPrompt != sess.SystemPrompt || got.Status != StatusRunning {
		t.Fatalf("unexpected session: %+v", got)
	}
	if len(got.DenyPatterns) != 1 || got.DenyPatterns[0] != "git push" {
		t.Fatalf("unexpected deny patterns: %+v", got.DenyPatterns)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("expected non-zero created_at")
	}

	finished := time.Now().UTC()
	if err := s.UpdateSessionStatus(ctx, "sess-1", StatusOK, &finished); err != nil {
		t.Fatalf("update status: %v", err)
	}
	got, err = s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOK || got.FinishedAt == nil {
		t.Fatalf("expected finished session, got %+v", got)
	}
}

func TestGetSessionNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.GetSession(context.Background(), "missing"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestSessionProvenanceRoundTrip pins the three provenance fields end to end:
// a populated session comes back with all of them, and a zero-value session
// comes back with the default implementation job type and no parent agent.
func TestSessionProvenanceRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	populated := Session{
		ID:              "sess-prov",
		Model:           "deepseek-v4-pro",
		Effort:          "high",
		Workspace:       "/tmp/ws",
		PermissionMode:  "default",
		SystemPrompt:    "sys",
		ToolSchema:      json.RawMessage(`[]`),
		JobType:         agentmeta.JobTypeOrchestration,
		ParentAgentType: "orchestrator",
		ParentAgentID:   "orchestrator-1",
	}
	if err := s.CreateSession(ctx, populated); err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err := s.GetSession(ctx, populated.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.JobType != agentmeta.JobTypeOrchestration || got.ParentAgentType != "orchestrator" || got.ParentAgentID != "orchestrator-1" {
		t.Fatalf("provenance fields not preserved: %+v", got)
	}

	empty := Session{
		ID:             "sess-plain",
		Model:          "deepseek-v4-flash",
		Effort:         "high",
		Workspace:      "/tmp/plain",
		PermissionMode: "default",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
	}
	if err := s.CreateSession(ctx, empty); err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err = s.GetSession(ctx, empty.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.JobType != agentmeta.JobTypeImplementation {
		t.Fatalf("expected job type %q, got %q", agentmeta.JobTypeImplementation, got.JobType)
	}
	if got.ParentAgentType != "" || got.ParentAgentID != "" {
		t.Fatalf("expected empty parent agent fields, got %+v", got)
	}
}

// TestOpenMigratesLegacySessionsTable builds a database with the pre-change
// sessions table, inserts a row, and proves Open adds the three columns,
// backfilling the row, and that a second Open is a no-op.
func TestOpenMigratesLegacySessionsTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	// The sessions table as it existed before the provenance columns.
	legacy := `
CREATE TABLE sessions (
	id              TEXT PRIMARY KEY,
	parent_id       TEXT,
	model           TEXT NOT NULL,
	effort          TEXT NOT NULL,
	thinking        INTEGER NOT NULL,
	workspace       TEXT NOT NULL,
	permission_mode TEXT NOT NULL,
	deny_patterns   TEXT NOT NULL DEFAULT '[]',
	system_prompt   TEXT NOT NULL,
	tool_schema     TEXT NOT NULL,
	result_schema   TEXT,
	status          TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	finished_at     TEXT
);
INSERT INTO sessions (id, model, effort, thinking, workspace, permission_mode,
	system_prompt, tool_schema, status, created_at)
VALUES ('legacy-1', 'deepseek-v4-pro', 'high', 1, '/tmp/ws', 'default',
	'sys', '[]', 'ok', '2026-01-02T03:04:05Z');`
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(legacy); err != nil {
		t.Fatalf("build legacy db: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	for attempt := 1; attempt <= 2; attempt++ {
		s, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", attempt, err)
		}
		got, err := s.GetSession(ctx, "legacy-1")
		if err != nil {
			t.Fatalf("get session after open %d: %v", attempt, err)
		}
		if got.JobType != agentmeta.JobTypeImplementation {
			t.Fatalf("open %d: expected job type %q, got %q", attempt, agentmeta.JobTypeImplementation, got.JobType)
		}
		if got.ParentAgentType != "" || got.ParentAgentID != "" {
			t.Fatalf("open %d: expected empty parent agent fields, got %+v", attempt, got)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", attempt, err)
		}
	}
}

func TestAppendEventsAssignsSequentialSeq(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")

	first, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindSessionStarted, Payload: SessionStartedPayload{OpeningMessage: "hi"}},
		{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Seq != 1 || first[1].Seq != 2 {
		t.Fatalf("expected seq 1,2, got %d,%d", first[0].Seq, first[1].Seq)
	}

	second, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindTurnFinished, Payload: TurnFinishedPayload{FinishReason: "stop"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if second[0].Seq != 3 {
		t.Fatalf("expected seq 3, got %d", second[0].Seq)
	}

	events, err := s.GetEvents(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("event %d has seq %d", i, e.Seq)
		}
	}
}

// TestAppendEventsRoundTripsTurnFinishedElapsedMs proves the elapsed_ms
// payload field survives the marshal/read path that serves the browser via
// /events and the SSE stream.
func TestAppendEventsRoundTripsTurnFinishedElapsedMs(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")

	appended, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindTurnFinished, Payload: TurnFinishedPayload{FinishReason: "stop", ElapsedMs: 124200}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var p TurnFinishedPayload
	if err := json.Unmarshal(appended[0].Payload, &p); err != nil {
		t.Fatalf("decode appended payload: %v", err)
	}
	if p.ElapsedMs != 124200 {
		t.Fatalf("expected appended turn_finished to carry elapsed_ms, got %d", p.ElapsedMs)
	}

	events, err := s.GetEvents(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(events[0].Payload, &p); err != nil {
		t.Fatalf("decode stored payload: %v", err)
	}
	if p.ElapsedMs != 124200 {
		t.Fatalf("expected stored turn_finished to carry elapsed_ms, got %d", p.ElapsedMs)
	}
}

func TestAppendEventsConcurrentSessionsDoNotInterleaveSeq(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-a")
	mustCreateSession(t, s, "sess-b")

	var wg sync.WaitGroup
	for _, id := range []string{"sess-a", "sess-b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := s.AppendEvents(ctx, id, []EventInput{
					{Kind: KindContentDelta, Payload: ContentDeltaPayload{Text: "x"}},
				}); err != nil {
					t.Errorf("append to %s: %v", id, err)
				}
			}
		}(id)
	}
	wg.Wait()

	for _, id := range []string{"sess-a", "sess-b"} {
		events, err := s.GetEvents(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) != 20 {
			t.Fatalf("session %s: expected 20 events, got %d", id, len(events))
		}
		for i, e := range events {
			if e.Seq != int64(i+1) {
				t.Fatalf("session %s: event %d has seq %d, want contiguous from 1", id, i, e.Seq)
			}
		}
	}
}

func TestWorkspaceLease(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// Re-acquiring for the same session is fine (idempotent heartbeat).
	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("re-acquire same session: %v", err)
	}
	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-2"); err != ErrWorkspaceLeased {
		t.Fatalf("expected ErrWorkspaceLeased, got %v", err)
	}
	if err := s.ReleaseWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-2"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func mustCreateSession(t *testing.T, s *Store, id string) {
	t.Helper()
	err := s.CreateSession(context.Background(), Session{
		ID:             id,
		Model:          "deepseek-v4-flash",
		Effort:         "high",
		Workspace:      "/tmp/" + id,
		PermissionMode: "default",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

// TestResumeSessionClearsFinishedAt is the state a resumed session needs
// before Runner.Resume appends its continuation: running again, and not
// still carrying the timestamp from the run that just finished.
func TestResumeSessionClearsFinishedAt(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")

	finished := time.Now().UTC()
	if err := s.UpdateSessionStatus(ctx, "sess-1", StatusOK, &finished); err != nil {
		t.Fatal(err)
	}
	if err := s.ResumeSession(ctx, "sess-1"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	got, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning {
		t.Fatalf("expected status running, got %s", got.Status)
	}
	if got.FinishedAt != nil {
		t.Fatalf("expected finished_at cleared, got %v", got.FinishedAt)
	}
}

// TestDeleteSessionRemovesEventsToo proves DeleteSession is not just a
// sessions-row delete: a session's whole event log goes with it, and a
// second delete on the same id reports ErrNotFound rather than succeeding
// silently on nothing.
func TestDeleteSessionRemovesEventsToo(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")
	if _, err := s.AppendEvents(ctx, "sess-1", []EventInput{
		{Kind: KindSessionStarted, Payload: SessionStartedPayload{OpeningMessage: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	finished := time.Now().UTC()
	if err := s.UpdateSessionStatus(ctx, "sess-1", StatusOK, &finished); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteSession(ctx, "sess-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.GetSession(ctx, "sess-1"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound after delete, got %v", err)
	}
	events, err := s.GetEvents(ctx, "sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("expected no events left for a deleted session, got %d", len(events))
	}

	if err := s.DeleteSession(ctx, "sess-1"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound deleting an already-deleted session, got %v", err)
	}
}

// TestDeleteSessionRefusesRunning protects a live session goroutine's own
// writes: deleting the row out from under it while it is still running
// would make its next AppendEvents call race a table that no longer exists.
func TestDeleteSessionRefusesRunning(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1") // CreateSession defaults to StatusRunning

	if err := s.DeleteSession(ctx, "sess-1"); err == nil {
		t.Fatal("expected DeleteSession to refuse a running session")
	}
	if _, err := s.GetSession(ctx, "sess-1"); err != nil {
		t.Fatalf("session should still exist after a refused delete: %v", err)
	}
}
