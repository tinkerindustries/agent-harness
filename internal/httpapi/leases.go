package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// Workspace leases (docs/DATA-API.md): the collection read and the one
// write route, deleting a stale lease. Work requests are the closely
// related resource one file over (requests.go).

// workspaceLeaseRow is one workspace_leases row over HTTP (docs/DATA-API.md):
// the workspace, the session holding it, when it was acquired, when
// it was last heartbeated, and the version every row resource carries.
type workspaceLeaseRow struct {
	Workspace   string    `json:"workspace"`
	SessionID   string    `json:"session_id"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	Version     int       `json:"version"`
}

func workspaceLeaseRowFrom(l store.WorkspaceLease) workspaceLeaseRow {
	return workspaceLeaseRow{
		Workspace:   l.Workspace,
		SessionID:   l.SessionID,
		AcquiredAt:  l.AcquiredAt,
		HeartbeatAt: l.HeartbeatAt,
		Version:     l.Version,
	}
}

func (s *Server) handleListLeases(w http.ResponseWriter, r *http.Request) {
	leases, err := s.Store.ListWorkspaceLeases(r.Context())
	if err != nil {
		writeInternalError(w, err)
		return
	}
	rows := make([]workspaceLeaseRow, 0, len(leases))
	for _, l := range leases {
		rows = append(rows, workspaceLeaseRowFrom(l))
	}
	writeJSON(w, http.StatusOK, rows)
}

// handleDeleteLease serves DELETE /api/leases/{workspace}: releases a
// workspace lease (docs/DATA-API.md). It carries the content-type and
// origin guards, requires the If-Match version (428 missing, 412 stale), and
// refuses a lease whose heartbeat is newer than sessionIdleThreshold with a
// 409 naming the last heartbeat — the lease analog of the session endpoints'
// active refusal: a live session heartbeats its lease, so a recent heartbeat
// means the workspace is in use right now. Success is 200 {"ok": true}.
func (s *Server) handleDeleteLease(w http.ResponseWriter, r *http.Request) {
	if !writeGuards(w, r) {
		return
	}
	want, ok := parseIfMatch(w, r)
	if !ok {
		return
	}
	if err := s.Store.DeleteWorkspaceLease(r.Context(), r.PathValue("workspace"), want, time.Now(), sessionIdleThreshold); err != nil {
		writeLeaseWriteError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// writeLeaseWriteError maps a store error from a lease write to the wire: not
// found is 404, a live lease is 409 naming the last heartbeat, a version
// mismatch is 412, and anything else is a 500 like every other handler.
func writeLeaseWriteError(w http.ResponseWriter, err error) {
	var active *store.ActiveLeaseError
	var conflict *store.VersionConflictError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "lease not found"})
	case errors.As(err, &active):
		writeJSON(w, http.StatusConflict, map[string]string{"error": active.Error()})
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusPreconditionFailed, map[string]string{"error": conflict.Error()})
	default:
		writeInternalError(w, err)
	}
}
