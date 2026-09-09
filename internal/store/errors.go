package store

import (
	"errors"
	"fmt"
	"time"
)

// ErrClosed is returned by Store methods called after Close.
var ErrClosed = errors.New("store: closed")

// ErrWorkspaceLeased is returned when a workspace is already leased to a
// different session.
var ErrWorkspaceLeased = errors.New("store: workspace already leased")

// ErrNotFound is returned when a lookup by id finds no row.
var ErrNotFound = errors.New("store: not found")

// ErrMCPServerNotFound is returned when an mcp_servers lookup or write
// targets a name with no row.
var ErrMCPServerNotFound = errors.New("store: mcp server not found")

// ErrMCPServerExists is returned when CreateMCPServer targets a name that
// already has a row: server names are the primary key, and there is no
// per-request scoping to disambiguate a second one (docs/MCP.md, "The
// table").
var ErrMCPServerExists = errors.New("store: mcp server already exists")

// ErrSessionCancelled is returned when a write targets a session that was
// stopped: its status is "cancelled", which is terminal and final
// (docs/RUN-CONTROL.md "Half two"). AppendEvents refuses a cancelled session
// so a wedged goroutine that wakes long after a stop cannot dirty the log it
// was stopped in, and FinishSession and UpdateSessionStatus refuse to move a
// cancelled row anywhere else.
var ErrSessionCancelled = errors.New("store: session is cancelled")

// SessionFinishedError is returned when a stop reaches a session that already
// reached a terminal status of its own — the run finished in the moment
// between the operator asking and the stop landing. Status is what it
// finished as, which is what the caller reports rather than overwriting:
// "the run succeeded" and "an operator killed it" are the distinction
// store.StatusCancelled exists to carry, and a timer must not decide it
// (docs/RUN-CONTROL.md "Half two"). It surfaces as a 409 naming the status.
type SessionFinishedError struct {
	SessionID string
	Status    string
}

func (e *SessionFinishedError) Error() string {
	return fmt.Sprintf("store: session %s already finished as %q", e.SessionID, e.Status)
}

// SessionRunningError is returned when a mutating write targets a session
// whose status is still live ("running", or "creating" while a worker is
// preparing its workspace): nothing may delete (or otherwise overwrite) a
// row a live session goroutine is still writing.
// It reports a caller's precondition failing.
type SessionRunningError struct {
	SessionID string
}

func (e *SessionRunningError) Error() string {
	return fmt.Sprintf("store: refusing to delete %s: it is still live", e.SessionID)
}

// ActiveSessionError is returned when a write would close a running session
// whose most recent event is newer than the caller's idle threshold — the
// row looks live, so the write refuses rather than race the run loop
// . LastEventAt is what the message
// names: when the session was actually last heard from.
type ActiveSessionError struct {
	SessionID   string
	LastEventAt time.Time
}

func (e *ActiveSessionError) Error() string {
	return fmt.Sprintf("store: session %s is still active: most recent event at %s",
		e.SessionID, e.LastEventAt.UTC().Format(time.RFC3339))
}

// VersionConflictError is returned when a mutating write carries an If-Match
// version that does not equal the row's current version — the client read the
// row before someone else changed it (optimistic
// concurrency"). It surfaces as a 412 on the HTTP surface. Resource names the
// row ("session <id>", "work_request <id>") so the message is
// self-describing in every resource's handler.
type VersionConflictError struct {
	Resource string
	Want     int
	Current  int
}

func (e *VersionConflictError) Error() string {
	return fmt.Sprintf("store: %s changed since it was read: If-Match %d, current version %d",
		e.Resource, e.Want, e.Current)
}

// ActiveRequestError is returned when a write would close or delete a work
// request whose session is still live — the row a live pool worker is using,
// which will publish its own terminal result. The request's session id is
// the signal: the session's most recent event newer than the caller's idle
// threshold means the request is genuinely in flight, so the write refuses
// rather than race the run. It
// surfaces as a 409 on the HTTP surface, and LastEventAt is what the message
// names: when the session was actually last heard from.
type ActiveRequestError struct {
	RequestID   string
	SessionID   string
	LastEventAt time.Time
}

func (e *ActiveRequestError) Error() string {
	return fmt.Sprintf("store: work request %s is still in flight: its session %s had its most recent event at %s",
		e.RequestID, e.SessionID, e.LastEventAt.UTC().Format(time.RFC3339))
}

// ActiveLeaseError is returned when a write would release a workspace lease
// whose heartbeat is newer than the caller's idle threshold — the lease a
// live session is still holding, so the write refuses rather than releasing
// a workspace that is being used right now (
// "Preconditions"). It surfaces as a 409 on the HTTP surface, and
// LastHeartbeatAt is what the message names: when the lease was actually
// last heartbeated.
type ActiveLeaseError struct {
	Workspace       string
	SessionID       string
	LastHeartbeatAt time.Time
}

func (e *ActiveLeaseError) Error() string {
	return fmt.Sprintf("store: lease on workspace %s is still held by live session %s: last heartbeat at %s",
		e.Workspace, e.SessionID, e.LastHeartbeatAt.UTC().Format(time.RFC3339))
}
