package mcp

import (
	"sync"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// runRecord is one request this MCP process has launched and what it last
// learned about it. It is bookkeeping for deepseek_runs, not a second source
// of truth — the durable record of a request's outcome is the RESULTS
// stream and the harness's own store, both of which outlive this process.
type runRecord struct {
	RequestID      string    `json:"request_id"`
	Title          string    `json:"title,omitempty"`
	Description    string    `json:"description"`
	Phase          int       `json:"phase,omitempty"`
	TotalPhases    int       `json:"total_phases,omitempty"`
	Repos          []string  `json:"repos,omitempty"` // url#branch, as launched
	Profile        string    `json:"profile,omitempty"`
	LaunchedAt     time.Time `json:"launched_at"`
	Status         string    `json:"status"`
	SessionID      string    `json:"session_id,omitempty"`
	CompleteStatus string    `json:"complete_status,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// registryCap bounds how many launches one process remembers, so a
// long-lived MCP server does not grow this list without limit. The durable
// history lives in the harness's own store (harness://sessions); this is
// only this process's short-term memory of what it started.
const registryCap = 500

// Registry is this MCP process's own record of what it has launched, kept
// in memory only — the safety section's "holds a queue connection and
// nothing else" rules out a database here, so a process restart starts a
// clean list. It is not the durable history of a request; deepseek_result
// and harness://sessions read that from the harness's work-request rows and
// its store respectively, which is why calling deepseek_result is
// what refreshes an entry here rather than this registry polling anything
// on its own.
type Registry struct {
	mu    sync.Mutex
	order []string // request ids, oldest first
	byID  map[string]*runRecord
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{byID: make(map[string]*runRecord)}
}

// recordLaunch adds or replaces rec's entry. A second launch under the same
// request_id (a caller retrying with the same idempotency key, matching the
// harness's own dedup story) replaces rather than duplicates the entry.
func (r *Registry) recordLaunch(rec runRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byID[rec.RequestID]; !exists {
		r.order = append(r.order, rec.RequestID)
		if len(r.order) > registryCap {
			drop := r.order[0]
			r.order = r.order[1:]
			delete(r.byID, drop)
		}
	}
	r.byID[rec.RequestID] = &rec
}

// updateStatus records the last known status (and, once known, the session
// id and Complete's own status) for requestID. It is a no-op for a
// request_id this process never launched — deepseek_result may be called
// for a request another process started, and that is not this registry's
// concern to track.
func (r *Registry) updateStatus(requestID, status, sessionID, completeStatus string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.byID[requestID]
	if !ok {
		return
	}
	rec.Status = status
	if sessionID != "" {
		rec.SessionID = sessionID
	}
	if completeStatus != "" {
		rec.CompleteStatus = completeStatus
	}
	rec.UpdatedAt = time.Now().UTC()
}

// updateFromResult is updateStatus specialised for a collected final
// result, so callers do not have to unpack a queue.Result themselves.
func (r *Registry) updateFromResult(requestID string, res queue.Result) {
	r.updateStatus(requestID, res.Status, res.SessionID, res.CompleteStatus)
}

// list returns every remembered launch, newest first.
func (r *Registry) list() []runRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]runRecord, len(r.order))
	for i, id := range r.order {
		out[len(out)-1-i] = *r.byID[id]
	}
	return out
}
