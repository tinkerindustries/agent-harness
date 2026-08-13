package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestListWorkspaceLeases lists the lease table in workspace order, each row
// carrying the version it was created with.
func TestListWorkspaceLeases(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws-b", "sess-2"); err != nil {
		t.Fatalf("acquire ws-b: %v", err)
	}
	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws-a", "sess-1"); err != nil {
		t.Fatalf("acquire ws-a: %v", err)
	}

	leases, err := s.ListWorkspaceLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 2 {
		t.Fatalf("expected 2 leases, got %d: %+v", len(leases), leases)
	}
	if leases[0].Workspace != "/tmp/ws-a" || leases[1].Workspace != "/tmp/ws-b" {
		t.Fatalf("expected leases in workspace order, got %+v", leases)
	}
	for _, l := range leases {
		if l.Version != 1 {
			t.Fatalf("a freshly acquired lease must start at version 1, got %+v", l)
		}
		if l.AcquiredAt.IsZero() || l.HeartbeatAt.IsZero() {
			t.Fatalf("expected acquired_at and heartbeat_at set, got %+v", l)
		}
	}
}

// TestDeleteWorkspaceLeaseReleases pins the success path: a lease whose
// heartbeat is older than the idle threshold is stranded — its session is
// gone or dead — and the release removes the row.
func TestDeleteWorkspaceLeaseReleases(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// The heartbeat is judged against the caller's `now`: two hours after the
	// lease was taken is far past any idle threshold.
	future := time.Now().UTC().Add(2 * time.Hour)
	if err := s.DeleteWorkspaceLease(ctx, "/tmp/ws", 1, future, 10*time.Minute); err != nil {
		t.Fatalf("release: %v", err)
	}
	leases, err := s.ListWorkspaceLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 0 {
		t.Fatalf("expected the lease gone, got %+v", leases)
	}
}

// TestDeleteWorkspaceLeaseRefusesLive pins the precondition that matters
// (docs/DATA-API.md): a lease whose heartbeat is newer than the idle
// threshold is held by a live session right now, and the release is a 409
// whose message names when the lease was last heartbeated. The row is
// untouched.
func TestDeleteWorkspaceLeaseRefusesLive(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	err := s.DeleteWorkspaceLease(ctx, "/tmp/ws", 1, time.Now().UTC(), 10*time.Minute)
	var active *ActiveLeaseError
	if !errors.As(err, &active) {
		t.Fatalf("expected ActiveLeaseError, got %v", err)
	}
	if active.Workspace != "/tmp/ws" || active.SessionID != "sess-1" {
		t.Fatalf("error must name the workspace and its session, got %+v", active)
	}
	if active.LastHeartbeatAt.IsZero() {
		t.Fatalf("error must name the last heartbeat's time, got %+v", active)
	}

	leases, err := s.ListWorkspaceLeases(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Version != 1 {
		t.Fatalf("a refused release must not touch the row, got %+v", leases)
	}
}

// TestDeleteWorkspaceLeaseGuards pins the remaining write preconditions: a
// stale If-Match version is a VersionConflictError naming the lease, and an
// unknown workspace is ErrNotFound.
func TestDeleteWorkspaceLeaseGuards(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	if err := s.AcquireWorkspaceLease(ctx, "/tmp/ws", "sess-1"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	future := time.Now().UTC().Add(2 * time.Hour)

	var conflict *VersionConflictError
	if err := s.DeleteWorkspaceLease(ctx, "/tmp/ws", 99, future, 10*time.Minute); !errors.As(err, &conflict) {
		t.Fatalf("expected VersionConflictError, got %v", err)
	}
	if conflict.Resource != "workspace_lease /tmp/ws" {
		t.Fatalf("conflict must name the lease, got %q", conflict.Resource)
	}

	if err := s.DeleteWorkspaceLease(ctx, "/tmp/unknown", 1, future, 10*time.Minute); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
