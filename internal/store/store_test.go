package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	// The optimistic-concurrency counter (docs/DATA-API.md) starts at 1 and
	// increments on every successful mutation.
	if got.Version != 2 {
		t.Fatalf("expected version 2 after one mutation, got %d", got.Version)
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
		ParentIsUser:    true,
	}
	if err := s.CreateSession(ctx, populated); err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err := s.GetSession(ctx, populated.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.JobType != agentmeta.JobTypeOrchestration || got.ParentAgentType != "orchestrator" || got.ParentAgentID != "orchestrator-1" || !got.ParentIsUser {
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
	if got.ParentAgentType != "" || got.ParentAgentID != "" || got.ParentIsUser {
		t.Fatalf("expected empty parent agent fields, got %+v", got)
	}
}

// TestSessionTitleFieldsRoundTrip pins the four session fields end to end:
// a session created with a title, description, and phase set comes back with
// all of them, and a session created without them comes back with the
// empty/zero values — the unphased default the browser renders without a
// title line or a phase chip.
func TestSessionTitleFieldsRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	populated := Session{
		ID:             "sess-titled",
		Model:          "deepseek-v4-pro",
		Effort:         "high",
		Workspace:      "/tmp/ws",
		PermissionMode: "default",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
		Task:           "raw launching prompt",
		Title:          "Add session title fields",
		Description:    "Carry a title, description, and phase position from every producer onto the session row and the main page.",
		Phase:          2,
		TotalPhases:    5,
	}
	if err := s.CreateSession(ctx, populated); err != nil {
		t.Fatalf("create session: %v", err)
	}
	got, err := s.GetSession(ctx, populated.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Title != populated.Title || got.Description != populated.Description {
		t.Fatalf("title fields not preserved: %+v", got)
	}
	if got.Phase != 2 || got.TotalPhases != 5 {
		t.Fatalf("phase fields not preserved: %+v", got)
	}
	if got.Task != populated.Task {
		t.Fatalf("task must stay the raw launching prompt, got %q", got.Task)
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
	if got.Title != "" || got.Description != "" {
		t.Fatalf("expected empty title and description, got %q/%q", got.Title, got.Description)
	}
	if got.Phase != 0 || got.TotalPhases != 0 {
		t.Fatalf("expected zero phase fields, got %d/%d", got.Phase, got.TotalPhases)
	}
}

// TestOpenMigratesLegacySessionsTable builds a database with the pre-change
// sessions table, inserts a row, and proves Open adds the missing columns —
// provenance, task, the terminal-metadata columns, and the four new session
// fields — backfilling the row, and that a second Open is a no-op.
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
		if got.ParentAgentType != "" || got.ParentAgentID != "" || got.ParentIsUser {
			t.Fatalf("open %d: expected empty parent agent fields, got %+v", attempt, got)
		}
		// The migration backfill rule: a row
		// written by an older binary reads back with the empty complete_status
		// the new column defaults to — the browser renders that as the plain
		// terminal status rather than guessing which outcome it was.
		if got.CompleteStatus != "" {
			t.Fatalf("open %d: expected complete_status to default to empty on a pre-migration row, got %q", attempt, got.CompleteStatus)
		}
		// Same rule for task: a row written before the task column existed
		// reads back empty, and the in-flight card renders no description
		// rather than inventing one.
		if got.Task != "" {
			t.Fatalf("open %d: expected task to default to empty on a pre-migration row, got %q", attempt, got.Task)
		}
		// The same migration rule for the four new session fields: a row
		// written by an older binary reads back with the empty/zero values
		// the new columns default to — the browser renders the task line
		// without a bold title and with no phase chip rather than guessing.
		if got.Title != "" || got.Description != "" {
			t.Fatalf("open %d: expected title and description to default to empty on a pre-migration row, got %q/%q", attempt, got.Title, got.Description)
		}
		if got.Phase != 0 || got.TotalPhases != 0 {
			t.Fatalf("open %d: expected phase and total_phases to default to zero on a pre-migration row, got %d/%d", attempt, got.Phase, got.TotalPhases)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", attempt, err)
		}
	}
}

// TestOpenMigratesLegacyRequestsAndLeasesTables covers the migration
// (docs/DATA-API.md): work_requests and workspace_leases rows written by an
// older binary have no version column, and Open adds it with default 1,
// backfilling existing rows, and a second Open is a no-op.
func TestOpenMigratesLegacyRequestsAndLeasesTables(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy.db")

	legacy := `
CREATE TABLE work_requests (
	request_id     TEXT PRIMARY KEY,
	session_id     TEXT,
	status         TEXT NOT NULL,
	result         TEXT,
	received_at    TEXT NOT NULL,
	finished_at    TEXT,
	delivery_count INTEGER NOT NULL DEFAULT 0
);
INSERT INTO work_requests (request_id, session_id, status, result, received_at, finished_at, delivery_count)
VALUES ('legacy-req', 'legacy-sess', 'running', NULL, '2026-01-02T03:04:05Z', NULL, 3);
CREATE TABLE workspace_leases (
	workspace    TEXT PRIMARY KEY,
	session_id   TEXT NOT NULL,
	acquired_at  TEXT NOT NULL,
	heartbeat_at TEXT NOT NULL
);
INSERT INTO workspace_leases (workspace, session_id, acquired_at, heartbeat_at)
VALUES ('/tmp/legacy', 'legacy-sess', '2026-01-02T03:04:05Z', '2026-01-02T04:04:05Z');`
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
		wr, err := s.GetWorkRequest(ctx, "legacy-req")
		if err != nil {
			t.Fatalf("get work request after open %d: %v", attempt, err)
		}
		if wr.Version != 1 {
			t.Fatalf("open %d: expected a pre-migration work request at version 1, got %d", attempt, wr.Version)
		}
		if wr.DeliveryCount != 3 || wr.SessionID != "legacy-sess" {
			t.Fatalf("open %d: migration must not disturb the row's own fields, got %+v", attempt, wr)
		}
		leases, err := s.ListWorkspaceLeases(ctx)
		if err != nil {
			t.Fatalf("list leases after open %d: %v", attempt, err)
		}
		if len(leases) != 1 || leases[0].Workspace != "/tmp/legacy" || leases[0].Version != 1 {
			t.Fatalf("open %d: expected the legacy lease at version 1, got %+v", attempt, leases)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", attempt, err)
		}
	}
}

// TestFinishSessionRecordsCompleteStatus proves the finish path writes the
// model's Complete status argument onto the session row alongside the
// terminal status, so the session list can tell DONE from GAVE UP without
// re-walking the event log.
func TestFinishSessionRecordsCompleteStatus(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")

	finished := time.Now().UTC()
	if err := s.FinishSession(ctx, "sess-1", StatusOK, "gave_up", "could not find the bug", &finished); err != nil {
		t.Fatalf("finish session: %v", err)
	}
	got, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Status != StatusOK {
		t.Fatalf("expected status %q, got %q", StatusOK, got.Status)
	}
	if got.CompleteStatus != "gave_up" {
		t.Fatalf("expected complete_status %q, got %q", "gave_up", got.CompleteStatus)
	}
	if got.Summary != "could not find the bug" {
		t.Fatalf("expected summary %q, got %q", "could not find the bug", got.Summary)
	}
	if got.FinishedAt == nil {
		t.Fatal("expected finished_at set")
	}

	// A session that ends without calling Complete — the model answered in
	// prose and called no tool — keeps the empty string, not a made-up one.
	if err := s.FinishSession(ctx, "sess-1", StatusOK, "", "", &finished); err != nil {
		t.Fatalf("finish session: %v", err)
	}
	got, err = s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.CompleteStatus != "" {
		t.Fatalf("expected complete_status to be empty when Complete was never called, got %q", got.CompleteStatus)
	}
}

// TestPlanColumnRoundTrips proves the session row's plan and
// recent-tool-call roll survive the trip into the row and back out, and
// that the roll is trimmed to the most recent few calls — the shape the
// session list needs to render the in-flight card without re-walking the
// event log.
func TestPlanColumnRoundTrips(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")

	plan := `[{"content":"Read the spec","status":"completed","activeForm":""},{"content":"Wire it up","status":"in_progress","activeForm":"Wiring"}]`
	now := time.Now().UTC()
	calls := []RecentToolCall{
		{Name: "Read", Arguments: `{"file_path":"docs/DESIGN.md"}`, CreatedAt: now},
		{Name: "TodoWrite", Arguments: `{"todos":[]}`, CreatedAt: now},
		{Name: "Bash", Arguments: `{"command":"go build ./..."}`, CreatedAt: now.Add(time.Second)},
	}
	if err := s.UpdateSessionLiveState(ctx, "sess-1", plan, calls); err != nil {
		t.Fatalf("update live state: %v", err)
	}

	got, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Plan != plan {
		t.Fatalf("expected plan %q, got %q", plan, got.Plan)
	}
	// The store persists what it is given and trims to the window; filtering
	// TodoWrite out of the roll is the runner's job, never the store's.
	if len(got.RecentToolCalls) != 3 {
		t.Fatalf("expected 3 recent tool calls, got %d: %+v", len(got.RecentToolCalls), got.RecentToolCalls)
	}
	if got.RecentToolCalls[0].Name != "Read" || got.RecentToolCalls[1].Name != "TodoWrite" || got.RecentToolCalls[2].Name != "Bash" {
		t.Fatalf("unexpected roll: %+v", got.RecentToolCalls)
	}

	// A sub-turn with no TodoWrite must not clobber the stored plan; the
	// runner passes "" and the store keeps what is there.
	if err := s.UpdateSessionLiveState(ctx, "sess-1", "", nil); err != nil {
		t.Fatalf("update live state without plan: %v", err)
	}
	got, err = s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Plan != plan {
		t.Fatalf("expected plan to survive a no-plan update, got %q", got.Plan)
	}

	// The roll trims to maxRecentToolCalls, keeping the newest entries.
	for i := 0; i < 6; i++ {
		if err := s.UpdateSessionLiveState(ctx, "sess-1", "", []RecentToolCall{
			{Name: "Grep", Arguments: `{"pattern":"x"}`, CreatedAt: now.Add(time.Duration(i+1) * time.Hour)},
		}); err != nil {
			t.Fatalf("update live state %d: %v", i, err)
		}
	}
	got, err = s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if len(got.RecentToolCalls) != maxRecentToolCalls {
		t.Fatalf("expected the roll trimmed to %d, got %d: %+v", maxRecentToolCalls, len(got.RecentToolCalls), got.RecentToolCalls)
	}
	for _, c := range got.RecentToolCalls {
		if c.Name != "Grep" {
			t.Fatalf("expected the roll to hold only the newest calls, got %+v", got.RecentToolCalls)
		}
	}
}

// TestTaskColumnRoundTrips pins the task column: the job's description set at
// creation comes back through both read paths — GetSession and ListSessions —
// so the session list can say what a session is about without reading the
// event log.
func TestTaskColumnRoundTrips(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	sess := Session{
		ID:             "sess-1",
		Model:          "deepseek-v4-pro",
		Effort:         "high",
		Workspace:      "/tmp/ws",
		PermissionMode: "default",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
		Task:           "carry the job's description onto the session row",
	}
	if err := s.CreateSession(ctx, sess); err != nil {
		t.Fatalf("create session: %v", err)
	}

	got, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if got.Task != sess.Task {
		t.Fatalf("expected task %q via GetSession, got %q", sess.Task, got.Task)
	}

	all, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(all) != 1 || all[0].Task != sess.Task {
		t.Fatalf("expected task %q via ListSessions, got %+v", sess.Task, all)
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

// finishedPtr returns a pointer to now, for the tests that must pass a
// non-nil finished_at to a status write.
func finishedPtr() *time.Time {
	now := time.Now().UTC()
	return &now
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
	sess, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	if err := s.DeleteSession(ctx, "sess-1", sess.Version); err != nil {
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

	if err := s.DeleteSession(ctx, "sess-1", sess.Version); err != ErrNotFound {
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
	sess, err := s.GetSession(ctx, "sess-1")
	if err != nil {
		t.Fatalf("get session: %v", err)
	}

	var running *SessionRunningError
	if err := s.DeleteSession(ctx, "sess-1", sess.Version); !errors.As(err, &running) {
		t.Fatalf("expected SessionRunningError, got %v", err)
	}
	if _, err := s.GetSession(ctx, "sess-1"); err != nil {
		t.Fatalf("session should still exist after a refused delete: %v", err)
	}
}

// TestDeleteSessionVersionConflict pins the optimistic-concurrency
// precondition (docs/DATA-API.md): a delete carrying a version that does not
// match the row refuses with VersionConflictError, and the row survives.
func TestDeleteSessionVersionConflict(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1") // version 1, running
	if err := s.UpdateSessionStatus(ctx, "sess-1", StatusOK, finishedPtr()); err != nil {
		t.Fatal(err)
	}
	// The row is now version 2; deleting with the stale version 1 refuses.
	var conflict *VersionConflictError
	if err := s.DeleteSession(ctx, "sess-1", 1); !errors.As(err, &conflict) {
		t.Fatalf("expected VersionConflictError, got %v", err)
	}
	if conflict.Want != 1 || conflict.Current != 2 {
		t.Fatalf("conflict = want %d current %d, want 1 and 2", conflict.Want, conflict.Current)
	}
	if _, err := s.GetSession(ctx, "sess-1"); err != nil {
		t.Fatalf("session should survive a refused delete: %v", err)
	}
}

// TestCloseSession pins CloseSession end to end: an idle running session can
// be closed (status terminal, finished_at set, version bumped, event log
// untouched), a session with a recent event refuses with ActiveSessionError
// carrying the last event's time, a stale version refuses with
// VersionConflictError, and an already-terminal session re-closes as a no-op
// that preserves finished_at.
func TestCloseSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	// A running session whose most recent event is 20 minutes old: abandoned.
	mustCreateSession(t, s, "abandoned")
	appendAt := time.Now().UTC()
	if _, err := s.AppendEvents(ctx, "abandoned", []EventInput{
		{Kind: KindSessionStarted, Payload: SessionStartedPayload{OpeningMessage: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	// now 20 minutes after the append: the event is older than any threshold
	// the harness uses (sessionIdleThreshold is 10m).
	now := appendAt.Add(20 * time.Minute)
	closed, err := s.CloseSession(ctx, "abandoned", StatusCancelled, 1, now, 10*time.Minute)
	if err != nil {
		t.Fatalf("close abandoned session: %v", err)
	}
	if closed.Status != StatusCancelled || closed.FinishedAt == nil {
		t.Fatalf("expected cancelled with finished_at, got %+v", closed)
	}
	if !closed.FinishedAt.Equal(now) {
		t.Fatalf("finished_at = %v, want the close time %v", closed.FinishedAt, now)
	}
	if closed.Version != 2 {
		t.Fatalf("expected version 2 after a close, got %d", closed.Version)
	}
	// The event log is untouched by a close.
	events, err := s.GetEvents(ctx, "abandoned")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Kind != KindSessionStarted {
		t.Fatalf("close must not append or remove events, got %+v", events)
	}

	// A running session with a fresh event is presumed live: 409 material.
	mustCreateSession(t, s, "live")
	if _, err := s.AppendEvents(ctx, "live", []EventInput{
		{Kind: KindSessionStarted, Payload: SessionStartedPayload{OpeningMessage: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = s.CloseSession(ctx, "live", StatusCancelled, 1, time.Now().UTC(), 10*time.Minute)
	var active *ActiveSessionError
	if !errors.As(err, &active) {
		t.Fatalf("expected ActiveSessionError for a live session, got %v", err)
	}
	if active.LastEventAt.IsZero() {
		t.Fatal("ActiveSessionError must carry the last event's time")
	}
	// The session is unchanged.
	live, err := s.GetSession(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if live.Status != StatusRunning || live.Version != 1 {
		t.Fatalf("a refused close must not touch the row, got %+v", live)
	}

	// A stale version refuses before any idleness judgement.
	mustCreateSession(t, s, "stale")
	if err := s.UpdateSessionStatus(ctx, "stale", StatusOK, finishedPtr()); err != nil {
		t.Fatal(err)
	}
	_, err = s.CloseSession(ctx, "stale", StatusCancelled, 1, time.Now().UTC(), 10*time.Minute)
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected VersionConflictError for a stale close, got %v", err)
	}

	// Re-closing an already-terminal session is a no-op on both status and
	// finished_at, and bumps the version. See
	// TestCloseSessionDoesNotRelabelATerminalSession for why the status is
	// held rather than overwritten.
	reclosed, err := s.CloseSession(ctx, "abandoned", StatusFailed, 2, now.Add(time.Hour), 10*time.Minute)
	if err != nil {
		t.Fatalf("re-close: %v", err)
	}
	if reclosed.Status != StatusCancelled || !reclosed.FinishedAt.Equal(now) {
		t.Fatalf("re-close must keep both status and finished_at, got %+v", reclosed)
	}
	if reclosed.Version != 3 {
		t.Fatalf("expected version 3 after a re-close, got %d", reclosed.Version)
	}
}

// TestCloseSessionNotFound reports ErrNotFound for an unknown id.
func TestCloseSessionNotFound(t *testing.T) {
	s := openTestStore(t)
	if _, err := s.CloseSession(context.Background(), "missing", StatusCancelled, 1, time.Now().UTC(), time.Minute); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestAppendEventsRefusesCancelledSession pins the append fence
// (docs/RUN-CONTROL.md "Half two"): a stopped run's wedged goroutine that
// wakes later must not be able to dirty the session it was stopped in. Only
// "cancelled" refuses — compaction and resume append to sessions in every
// other terminal status, so a fence that widened to terminal statuses
// generally would break both.
func TestAppendEventsRefusesCancelledSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mustCreateSession(t, s, "stopped")
	if err := s.CancelRunningSession(ctx, "stopped", time.Now().UTC()); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := s.AppendEvents(ctx, "stopped", []EventInput{
		{Kind: KindContentDelta, Payload: ContentDeltaPayload{Text: "late"}},
	}); !errors.Is(err, ErrSessionCancelled) {
		t.Fatalf("append to a cancelled session = %v, want ErrSessionCancelled", err)
	}
	events, err := s.GetEvents(ctx, "stopped")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("a refused append must leave the log untouched, got %d events", len(events))
	}

	// Every other terminal status still accepts appends: compaction retires a
	// parent as compacted and resume appends a continuation before flipping
	// the row back to running.
	for _, status := range []string{StatusOK, StatusFailed, StatusTimeout, StatusMaxTurns, StatusCompacted} {
		id := "sess-" + status
		mustCreateSession(t, s, id)
		if err := s.UpdateSessionStatus(ctx, id, status, finishedPtr()); err != nil {
			t.Fatalf("set %s to %s: %v", id, status, err)
		}
		if _, err := s.AppendEvents(ctx, id, []EventInput{
			{Kind: KindTurnStarted, Payload: TurnStartedPayload{SubTurn: 1}},
		}); err != nil {
			t.Fatalf("append to a %s session must still work, got %v", status, err)
		}
	}
}

// TestFinishSessionCannotLeaveCancelled pins the terminal-status fence: a
// cancelled row is final, so neither finish path may move it anywhere else —
// that is what stops a wedged goroutine that wakes after a stop from
// relabelling the session (docs/RUN-CONTROL.md "Half two").
func TestFinishSessionCannotLeaveCancelled(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "stopped")
	now := time.Now().UTC()
	if err := s.CancelRunningSession(ctx, "stopped", now); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	if err := s.FinishSession(ctx, "stopped", StatusOK, "done", "summary", finishedPtr()); !errors.Is(err, ErrSessionCancelled) {
		t.Fatalf("FinishSession out of cancelled = %v, want ErrSessionCancelled", err)
	}
	if err := s.UpdateSessionStatus(ctx, "stopped", StatusFailed, finishedPtr()); !errors.Is(err, ErrSessionCancelled) {
		t.Fatalf("UpdateSessionStatus out of cancelled = %v, want ErrSessionCancelled", err)
	}
	// Even a finish that would leave it cancelled is refused: the row is
	// already terminal, and the refused write is the wedged goroutine's
	// signal to log and unwind rather than publish a second result.
	if err := s.FinishSession(ctx, "stopped", StatusCancelled, "", "", finishedPtr()); !errors.Is(err, ErrSessionCancelled) {
		t.Fatalf("FinishSession as cancelled on a cancelled row = %v, want ErrSessionCancelled", err)
	}

	got, err := s.GetSession(ctx, "stopped")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCancelled || got.FinishedAt == nil || !got.FinishedAt.Equal(now) {
		t.Fatalf("a refused status write must leave the row untouched, got %+v", got)
	}
}

// TestCancelRunningSessionIgnoresIdleness is the mirror image of CloseSession's
// idle test: CancelRunningSession takes no idle precondition, because it is
// the run's own owner stopping a live row — the row being live is the point —
// where CloseSession's idle check exists to stop an operator closing one.
func TestCancelRunningSessionIgnoresIdleness(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "live")

	// A fresh event would make CloseSession refuse with ActiveSessionError
	// for any minIdle the harness uses; CancelRunningSession must not care.
	if _, err := s.AppendEvents(ctx, "live", []EventInput{
		{Kind: KindSessionStarted, Payload: SessionStartedPayload{OpeningMessage: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := s.CancelRunningSession(ctx, "live", now); err != nil {
		t.Fatalf("cancel a live session: %v", err)
	}
	got, err := s.GetSession(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCancelled || got.FinishedAt == nil || !got.FinishedAt.Equal(now) {
		t.Fatalf("expected cancelled with finished_at = now, got %+v", got)
	}
	if got.Version != 2 {
		t.Fatalf("expected version 2 (create, cancel), got %d", got.Version)
	}

	// A second cancel is a no-op: the row is already cancelled, and neither
	// finished_at nor the version moves. A stop retried by an operator, or by
	// a caller that did not see the first answer, must not shift the moment
	// the stop actually landed.
	again := now.Add(time.Minute)
	if err := s.CancelRunningSession(ctx, "live", again); err != nil {
		t.Fatalf("re-cancel: %v", err)
	}
	got, err = s.GetSession(ctx, "live")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusCancelled || !got.FinishedAt.Equal(now) {
		t.Fatalf("a re-cancel must keep status and finished_at, got %+v", got)
	}
	if got.Version != 2 {
		t.Fatalf("expected the version to stay at 2 after a no-op re-cancel, got %d", got.Version)
	}
}

// TestCancelRunningSessionRefusesAFinishedRun is the race a stop's grace
// period creates: the run reaches a terminal status of its own in the moment
// between the operator asking and the timer firing. Cancelling then would
// relabel a completed run as one an operator killed, destroying the
// distinction store.StatusCancelled exists to carry, so it refuses and names
// what the session actually finished as.
func TestCancelRunningSessionRefusesAFinishedRun(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "finished-first")

	finished := time.Now().UTC()
	if err := s.FinishSession(ctx, "finished-first", StatusOK, "done", "all done", &finished); err != nil {
		t.Fatal(err)
	}

	err := s.CancelRunningSession(ctx, "finished-first", finished.Add(time.Second))
	var fin *SessionFinishedError
	if !errors.As(err, &fin) {
		t.Fatalf("expected SessionFinishedError, got %v", err)
	}
	if fin.Status != StatusOK {
		t.Fatalf("expected the error to name the status it finished as, got %q", fin.Status)
	}

	got, err := s.GetSession(ctx, "finished-first")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOK || got.CompleteStatus != "done" {
		t.Fatalf("the completed run must keep its own outcome, got %+v", got)
	}
}

// TestCancelRunningSessionNotFound reports ErrNotFound for an unknown id.
func TestCancelRunningSessionNotFound(t *testing.T) {
	s := openTestStore(t)
	if err := s.CancelRunningSession(context.Background(), "missing", time.Now().UTC()); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestCloseSessionDoesNotRelabelATerminalSession pins the audit property:
// PATCH exists to close an abandoned run, not to rewrite what a finished one
// did. A session that already reached a terminal status keeps that status,
// so a re-close is a version bump and nothing else — idempotent for a
// retried write, and unable to turn a completed run into a failed one.
func TestCloseSessionDoesNotRelabelATerminalSession(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	mustCreateSession(t, s, "finished")
	appendAt := time.Now().UTC()
	if _, err := s.AppendEvents(ctx, "finished", []EventInput{
		{Kind: KindSessionStarted, Payload: SessionStartedPayload{OpeningMessage: "hi"}},
	}); err != nil {
		t.Fatal(err)
	}
	now := appendAt.Add(20 * time.Minute)

	// Close it once, the way an abandoned run would be closed.
	closed, err := s.CloseSession(ctx, "finished", StatusOK, 1, now, 10*time.Minute)
	if err != nil {
		t.Fatalf("first close: %v", err)
	}
	if closed.Status != StatusOK {
		t.Fatalf("first close: status = %q, want %q", closed.Status, StatusOK)
	}
	firstFinished := closed.FinishedAt

	// Now try to relabel the finished run as failed.
	again, err := s.CloseSession(ctx, "finished", StatusFailed, closed.Version, now.Add(time.Minute), 10*time.Minute)
	if err != nil {
		t.Fatalf("re-close: %v", err)
	}
	if again.Status != StatusOK {
		t.Fatalf("a terminal session was relabelled %q; a finished run's status is a fact about what happened", again.Status)
	}
	if again.FinishedAt == nil || !again.FinishedAt.Equal(*firstFinished) {
		t.Fatalf("finished_at moved on a re-close: %v, want %v", again.FinishedAt, firstFinished)
	}
	if again.Version != closed.Version+1 {
		t.Fatalf("version = %d, want %d — a re-close still bumps the version so a stale retry fails", again.Version, closed.Version+1)
	}
}

// TestOpenMigratesLegacyMCPServersTable covers an mcp_servers table written
// by a binary from before instructions were plumbed through: the column is
// added, the row survives it with its probe snapshot intact, and the
// backfilled value is "" — which reads as "this server said nothing", so
// the operator's next Refresh is all it takes to start carrying the real
// text. Opened twice, because a migration that is not a no-op the second
// time is a migration that breaks every restart after the first.
func TestOpenMigratesLegacyMCPServersTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "legacy-mcp.db")

	legacy := `
CREATE TABLE mcp_servers (
	name           TEXT PRIMARY KEY,
	transport      TEXT NOT NULL,
	command        TEXT NOT NULL DEFAULT '',
	args           TEXT NOT NULL DEFAULT '[]',
	env            TEXT NOT NULL DEFAULT '{}',
	url            TEXT NOT NULL DEFAULT '',
	headers        TEXT NOT NULL DEFAULT '{}',
	enabled        INTEGER NOT NULL DEFAULT 1,
	allow_readonly INTEGER NOT NULL DEFAULT 0,
	tools_json     TEXT NOT NULL DEFAULT '[]',
	probed_at      TEXT NOT NULL DEFAULT '',
	probe_error    TEXT NOT NULL DEFAULT '',
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	version        INTEGER NOT NULL DEFAULT 1
);
INSERT INTO mcp_servers (name, transport, command, tools_json, probed_at, created_at, updated_at)
VALUES ('blender', 'stdio', 'blender-mcp',
	'[{"name":"get_objects_summary","qualified_name":"mcp__blender__get_objects_summary","description":"list scene objects","input_schema":null}]',
	'2026-01-02T03:04:05Z', '2026-01-02T03:04:05Z', '2026-01-02T03:04:05Z');`
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
		srv, err := s.GetMCPServer(ctx, "blender")
		if err != nil {
			t.Fatalf("get mcp server after open %d: %v", attempt, err)
		}
		if srv.Instructions != "" {
			t.Fatalf("open %d: expected a pre-migration row to backfill to empty, got %q", attempt, srv.Instructions)
		}
		if len(srv.Tools) != 1 || srv.Tools[0].Name != "get_objects_summary" {
			t.Fatalf("open %d: migration must not disturb the probe snapshot, got %+v", attempt, srv.Tools)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", attempt, err)
		}
	}
}
