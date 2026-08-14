package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// createSessionAt creates a session with a fixed created_at, so ordering
// tests do not depend on wall-clock insertion order.
func createSessionAt(t *testing.T, s *Store, id string, createdAt time.Time) {
	t.Helper()
	err := s.CreateSession(context.Background(), Session{
		ID:             id,
		Model:          "deepseek-v4-flash",
		Effort:         "high",
		Workspace:      "/tmp/" + id,
		PermissionMode: "default",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
		CreatedAt:      createdAt,
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

// createSessionWS creates a session whose workspace is independent of its id,
// so a query test can hit exactly one of the three search targets.
func createSessionWS(t *testing.T, s *Store, id, workspace string) {
	t.Helper()
	err := s.CreateSession(context.Background(), Session{
		ID:             id,
		Model:          "deepseek-v4-flash",
		Effort:         "high",
		Workspace:      workspace,
		PermissionMode: "default",
		SystemPrompt:   "sys",
		ToolSchema:     json.RawMessage(`[]`),
	})
	if err != nil {
		t.Fatalf("create session %s: %v", id, err)
	}
}

func finishSession(t *testing.T, s *Store, id string) {
	t.Helper()
	finished := time.Now().UTC()
	if err := s.UpdateSessionStatus(context.Background(), id, StatusOK, &finished); err != nil {
		t.Fatalf("finish session %s: %v", id, err)
	}
}

func TestListSessionsPageOrderingAndWindow(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Newest is "newest", oldest is "oldest"; insertion order is the reverse,
	// so any accidental insertion-order sort would show up immediately.
	createSessionAt(t, s, "oldest", now.Add(-2*time.Hour))
	createSessionAt(t, s, "middle", now.Add(-1*time.Hour))
	createSessionAt(t, s, "newest", now)

	page, total, err := s.ListSessionsPage(ctx, SessionPageOptions{Limit: 2})
	if err != nil {
		t.Fatalf("list page: %v", err)
	}
	if total != 3 {
		t.Fatalf("expected total 3, got %d", total)
	}
	if len(page) != 2 || page[0].ID != "newest" || page[1].ID != "middle" {
		t.Fatalf("expected page [newest middle], got %+v", ids(page))
	}

	page, total, err = s.ListSessionsPage(ctx, SessionPageOptions{Limit: 2, Offset: 2})
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if total != 3 {
		t.Fatalf("expected total 3 on the second page, got %d", total)
	}
	if len(page) != 1 || page[0].ID != "oldest" {
		t.Fatalf("expected page [oldest], got %+v", ids(page))
	}

	// The page order is the same order ListSessions uses, so a row's
	// position never depends on which call fetched it.
	all, err := s.ListSessions(ctx)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 || all[0].ID != "newest" || all[1].ID != "middle" || all[2].ID != "oldest" {
		t.Fatalf("ListSessions order diverged from ListSessionsPage: %+v", ids(all))
	}
}

func TestListSessionsPageStatusSplit(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "run-1")
	mustCreateSession(t, s, "run-2")
	mustCreateSession(t, s, "done-1")
	mustCreateSession(t, s, "done-2")
	finishSession(t, s, "done-1")
	finishSession(t, s, "done-2")

	page, total, err := s.ListSessionsPage(ctx, SessionPageOptions{Status: "running", Limit: 20})
	if err != nil {
		t.Fatalf("list running: %v", err)
	}
	if total != 2 || len(page) != 2 {
		t.Fatalf("expected 2 running rows, got total %d len %d", total, len(page))
	}
	for _, sess := range page {
		if sess.Status != StatusRunning {
			t.Fatalf("expected only running rows, got %q", sess.Status)
		}
	}

	page, total, err = s.ListSessionsPage(ctx, SessionPageOptions{Status: "finished", Limit: 20})
	if err != nil {
		t.Fatalf("list finished: %v", err)
	}
	if total != 2 || len(page) != 2 {
		t.Fatalf("expected 2 finished rows, got total %d len %d", total, len(page))
	}
	for _, sess := range page {
		if sess.Status == StatusRunning {
			t.Fatalf("expected no running rows under finished, got %q", sess.Status)
		}
	}

	// No status clause: everything.
	_, total, err = s.ListSessionsPage(ctx, SessionPageOptions{Limit: 20})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if total != 4 {
		t.Fatalf("expected 4 rows with no status filter, got %d", total)
	}
}

func TestListSessionsPageQueryMatchesAllThreeTargets(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	// Workspace "/ws/one" matches the query "one" only through the workspace
	// column; "sess-2" only through the id; "special" only through the
	// work-request join.
	createSessionWS(t, s, "sess-1", "/ws/one")
	createSessionWS(t, s, "sess-2", "/ws/two")
	createSessionWS(t, s, "sess-3", "/ws/three")
	if _, err := s.ClaimWorkRequest(ctx, "req-special-9", 1, time.Now()); err != nil {
		t.Fatalf("claim work request: %v", err)
	}
	if err := s.SetWorkRequestSession(ctx, "req-special-9", "sess-3"); err != nil {
		t.Fatalf("set work request session: %v", err)
	}

	// Workspace-only match.
	page, total, err := s.ListSessionsPage(ctx, SessionPageOptions{Query: "one", Limit: 20})
	if err != nil {
		t.Fatalf("query workspace: %v", err)
	}
	if total != 1 || len(page) != 1 || page[0].ID != "sess-1" {
		t.Fatalf("expected only sess-1 for query one, got %+v", ids(page))
	}

	// Id-only match.
	page, total, err = s.ListSessionsPage(ctx, SessionPageOptions{Query: "sess-2", Limit: 20})
	if err != nil {
		t.Fatalf("query id: %v", err)
	}
	if total != 1 || len(page) != 1 || page[0].ID != "sess-2" {
		t.Fatalf("expected only sess-2 for query sess-2, got %+v", ids(page))
	}

	// Work-request-only match, and case-insensitively (SQLite LIKE on ASCII,
	// the browser's toLowerCase().includes() analogue).
	page, total, err = s.ListSessionsPage(ctx, SessionPageOptions{Query: "SPECIAL", Limit: 20})
	if err != nil {
		t.Fatalf("query work request: %v", err)
	}
	if total != 1 || len(page) != 1 || page[0].ID != "sess-3" {
		t.Fatalf("expected only sess-3 for query SPECIAL, got %+v", ids(page))
	}

	// A query matching nothing is an empty page with a zero total.
	page, total, err = s.ListSessionsPage(ctx, SessionPageOptions{Query: "no-such-row", Limit: 20})
	if err != nil {
		t.Fatalf("query no match: %v", err)
	}
	if total != 0 || len(page) != 0 {
		t.Fatalf("expected nothing for a no-match query, got total %d len %d", total, len(page))
	}
}

func TestListSessionsPageTotalCountsFilterNotPage(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		mustCreateSession(t, s, "run-"+string(rune('a'+i)))
	}
	for i := 0; i < 3; i++ {
		mustCreateSession(t, s, "done-"+string(rune('a'+i)))
		finishSession(t, s, "done-"+string(rune('a'+i)))
	}

	// A page of 2 finished rows must report total 3 — the filter's count,
	// not the page's length.
	page, total, err := s.ListSessionsPage(ctx, SessionPageOptions{Status: "finished", Limit: 2})
	if err != nil {
		t.Fatalf("list finished page: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("expected 2 rows on the page, got %d", len(page))
	}
	if total != 3 {
		t.Fatalf("expected total 3 (the filter's count), got %d", total)
	}
}

func TestListSessionsPageOffsetPastEnd(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	mustCreateSession(t, s, "sess-1")
	mustCreateSession(t, s, "sess-2")

	page, total, err := s.ListSessionsPage(ctx, SessionPageOptions{Limit: 20, Offset: 10})
	if err != nil {
		t.Fatalf("list page past the end: %v", err)
	}
	if total != 2 {
		t.Fatalf("expected total 2 past the end, got %d", total)
	}
	if len(page) != 0 {
		t.Fatalf("expected an empty page past the end, got %+v", ids(page))
	}
}

func ids(sessions []Session) []string {
	out := make([]string, len(sessions))
	for i, s := range sessions {
		out[i] = s.ID
	}
	return out
}
