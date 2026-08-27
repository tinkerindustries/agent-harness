// Package httpapi is the harness's HTTP surface (docs/DESIGN.md §4.2,
// docs/DATA-API.md): it reads and writes the data the harness manages and
// cannot reach the run loop. GET and HEAD are served on every path, including
// ones that do not exist; the writing methods are allowed where a write route
// exists — PUT and DELETE on a settings key, PATCH and DELETE on one session,
// POST/PATCH/DELETE on the MCP server registry, and POST on the run-control
// endpoints (docs/RUN-CONTROL.md). It serves
// the session list and metadata from the store, a paged read of one session's
// event log, two SSE streams — a per-session transcript and a quiet
// session-level list feed — fed by the in-process hub package rather than
// the queue, the settings table, the work-request and workspace-lease rows,
// the MCP server registry (mcp.go, docs/MCP.md), and a read-only GitHub repo
// list (GET /api/github/repos) backing the
// start-run form's repo picker (github.go).
// The write surface is the data the harness manages: closing an abandoned
// session, deleting a finished one, setting a key, registering or editing an
// MCP server. Run control is a declared
// seam, not an import: stopping goes through the RunController interface
// below, satisfied by *worker.Pool without this package knowing the package
// exists; starting goes through the RunPublisher interface, satisfied by
// cmd/harness over the store-backed queue — so this package holds no queue
// handle, only the narrow ability to enqueue one request.
// Steering is the one control that needs no seam at all — it is a store
// write by the handler and a store read by the loop, with the database as
// the boundary (docs/RUN-CONTROL.md "Steering: augment, don't gate").
//
// The package splits by resource, one file per group: this file keeps the
// package doc, the declared seams, the Server type, and the router
// (routes()), so the whole surface stays readable in one list; methods.go
// is the method gate; sessions.go and runcontrol.go split the session
// resource itself from the three actions that act on a run; auth.go is the
// control token; requests.go and leases.go are work requests and workspace
// leases; settings.go is the settings surface; events.go and sse.go are the
// event log and its two streams; respond.go is the small helpers shared
// across more than one of the above. evals.go, github.go, models.go,
// pricing.go, screenshots.go, paging.go, and static.go were already split
// out before this list existed; mcp.go is the MCP server registry.
package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/evals"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/pricing"
	"github.com/mrgeoffrich/agent-harness/internal/queue"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// sseKeepaliveInterval is how often an idle stream sends a ": keep-alive"
// comment, which also doubles as the poll interval for noticing a session
// that reached a terminal status without an event of its own to announce it
// (compaction retires the old session id silently; see handleSessionStream).
const sseKeepaliveInterval = 15 * time.Second

// sessionIdleThreshold is how long a running session must have been quiet
// before PATCH /api/sessions/{id} may close it (docs/DATA-API.md
// "Preconditions"). A live run appends events continuously, so a session
// whose most recent event is newer than this is presumed live and the close
// refuses with a 409 naming the last event's time. Ten minutes is the value
// used operating the harness by hand: long enough that a run paused on a
// slow tool call is never mistaken for dead, short enough that an abandoned
// row is closable without waiting out the work day. A named constant, never
// a literal, because the close endpoints and the tests both lean on it.
const sessionIdleThreshold = 10 * time.Minute

// terminalSessionStatuses are the statuses PATCH /api/sessions/{id} accepts:
// every status except running. Closing *into* running would be a resume,
// which is run control (docs/DATA-API.md) — the endpoint is for closing a
// session a dead worker left running, not for reviving one.
var terminalSessionStatuses = map[string]bool{
	store.StatusOK:        true,
	store.StatusFailed:    true,
	store.StatusTimeout:   true,
	store.StatusMaxTurns:  true,
	store.StatusCancelled: true,
	store.StatusCompacted: true,
}

// terminalSessionStatusList is the same set in a stable order, for the 400
// message that names the accepted values.
var terminalSessionStatusList = []string{
	store.StatusOK, store.StatusFailed, store.StatusTimeout,
	store.StatusMaxTurns, store.StatusCancelled, store.StatusCompacted,
}

// terminalRequestStatuses are the statuses PATCH /api/requests/{request_id}
// accepts: the terminal statuses a work request can hold (docs/DESIGN.md
// §4.10) — everything except running, which is the only status a live worker
// can hold and the one this endpoint exists to retire. "cancelled" is
// included even though nothing in the worker produces it: closing a dead
// request is the operator's analogue of closing a dead session, and the
// operator's status vocabulary is the session one.
var terminalRequestStatuses = map[string]bool{
	"ok":        true,
	"failed":    true,
	"denied":    true,
	"timeout":   true,
	"cancelled": true,
}

// terminalRequestStatusList is the same set in a stable order, for the 400
// message that names the accepted values.
var terminalRequestStatusList = []string{
	"ok", "failed", "denied", "timeout", "cancelled",
}

const (
	// defaultEventsLimit and maxEventsLimit are the built-in paging bounds
	// when a Server is built without the fields (the test path). Production
	// resolves http.events_limit_default and http.events_limit_max from the
	// settings registry at startup and sets the Server fields; the values
	// are pinned equal by internal/settings/registry_test.go.
	defaultEventsLimit = 500
	maxEventsLimit     = 5000
)

// QueueStats is the subset of the queue the queue health endpoint reads. A
// narrow interface here, rather than a direct *queue.Queue field, is what
// lets a test supply a fake with no store behind it.
type QueueStats interface {
	Stats(ctx context.Context) (queue.Stats, error)
}

// QueuePool is the subset of *worker.Pool's halted state the queue health
// endpoint reads. A narrow interface here, rather than an import of
// internal/worker, keeps this package's dependency pointed at the one
// method it needs rather than a whole package.
type QueuePool interface {
	Halted() (bool, string)
}

// RunController is the subset of the worker pool that run control needs,
// declared here rather than importing internal/worker — the same shape as
// QueuePool above: a narrow interface named for what it does, implemented by
// *worker.Pool without this package knowing the package exists, so the import
// boundary ARCHITECTURE.md calls structural stays structural (docs/RUN-CONTROL.md
// "Stopping is addressed at one goroutine, so the seam is a registry").
type RunController interface {
	// Stop begins ending sessionID and returns immediately: it cancels the
	// run's context, and a stop already in progress is not an error (a second
	// stop is another 202, not a 409). It reports ErrRunNotFound when no run
	// in this process owns that session. The handler must not parse the
	// error's text to tell those apart — it asks Running first and treats any
	// error from Stop as a 500 carrying the message.
	Stop(sessionID, reason string) error
	// Running reports whether this process is running sessionID.
	Running(sessionID string) bool
}

// RunPublisher is the subset of the queue a start endpoint needs: publish
// one validated work request. Declared here and implemented in cmd/harness,
// so this package never holds a queue handle — only the ability to enqueue
// one request (docs/RUN-CONTROL.md "Starting is a publish, so the seam is a
// publisher"). It is deliberately an import of internal/queue's Request and
// Validate rather than a copy of either: a request body validated by a copy
// of the rules is a request body that eventually disagrees with the queue's.
type RunPublisher interface {
	PublishRequest(ctx context.Context, req queue.Request) error
}

// MCPProber is the subset of the MCP client manager the server needs:
// re-read one server's tool list and store it. Declared here and
// implemented by *mcpclient.Manager in cmd/harness, so this package still
// holds no MCP client and no subprocess — only the ability to ask for one
// probe (docs/MCP.md).
type MCPProber interface {
	Refresh(ctx context.Context, name string) (store.MCPServer, error)

	// Complete asks one server what values an argument could take
	// (docs/MCP.md, "Completions"). It is here rather than on the
	// MCPProvider seam internal/tools declares because nothing in a run
	// wants it: a model does not autocomplete, an operator filling in a
	// prompt's argument does. kind is "prompt" or "resource", naming which
	// of the two things ref identifies.
	Complete(ctx context.Context, server, kind, ref, argName, argValue string) ([]string, error)
}

// EvalController is the subset of the eval orchestrator the eval endpoints
// need. Declared here and implemented in cmd/harness over *evals.Orchestrator,
// the shape RunController and RunPublisher already use: this package asks a
// seam to begin and end an orchestration and still holds no queue handle —
// the orchestrator has one, through the same one-method publisher seam.
//
// StartEval returns as soon as the run is recorded and its first requests are
// published; it never waits for the eval to finish.
type EvalController interface {
	StartEval(ctx context.Context, spec evals.Spec) (string, error)
	CancelEval(evalRunID string) error
	RunningEval(evalRunID string) bool
}

// Server holds the things every handler reads: the store, for everything
// historical; the hub, for everything live; and, optionally, the queue and
// pool, for /api/queue's depth, scheduled, in-flight, and redelivery counts
// (docs/DESIGN.md §5.8). The write surface is the data the harness manages
// (docs/DATA-API.md): the settings endpoints and the session close/delete
// endpoints read and write through Store, and nothing else in Server is
// mutated by a request — the run surface stays read-only except for the
// run-control endpoints: stop, which acts on a run through the RunController
// seam; steer, which is a plain store write the session loop reads at its
// next sub-turn boundary and needs no seam at all; and start, which
// enqueues a work request through the RunPublisher seam
// (docs/RUN-CONTROL.md). Queue and Pool are nil in any caller that has no
// queue at all (a CLI-only harness never wires one up); the handler
// degrades to reporting the queue as unavailable rather than panicking. Run
// is nil the same way in a caller with no pool, and the stop handler then
// answers 409 for every existing session — this process is running nothing.
// Publisher is nil in any caller that has no queue, and the start handler
// then answers 503 — no publisher wired means no run can be started, and
// that must fail closed, the same shape the missing token has. ControlToken
// is the process's copy of http.control_token; empty means run control is
// not configured and the run-control endpoints fail closed with 503. MCP is
// nil in any caller with no client manager wired (a CLI-only harness, or
// most tests): creating and editing an MCP server still work, they simply
// record no probe on the row, and POST .../refresh answers 503 the same way
// the missing publisher does (docs/MCP.md).
type Server struct {
	Store          *store.Store
	Hub            *hub.Hub
	Static         http.Handler
	Queue          QueueStats
	Pool           QueuePool
	Run            RunController
	Publisher      RunPublisher
	Evals          EvalController
	MCP            MCPProber
	ControlToken   string
	PriceTableDate string
	// Prices is the loaded price table, for GET /api/pricing. Only its
	// capture date and rate schedule are served — the rates themselves stay
	// on the server, because no screen prices anything: every cost figure in
	// the UI was computed when its usage event was committed and stored on
	// it (docs/DESIGN.md §4.9). Nil in a test that does not care, which the
	// endpoint answers with an empty schedule rather than a 500.
	Prices   *pricing.Table
	Settings *settings.Resolver

	// GitHubBaseURL overrides the GitHub REST API root GET /api/github/repos
	// fetches from (github.go). Empty means the real api.github.com; tests
	// set it to an httptest.Server standing in for GitHub. github holds the
	// last successful repo fetch and its time — the only mutable state on an
	// otherwise stateless Server — so reopening the start-run dialog
	// re-reads it for githubCacheTTL instead of re-hitting GitHub.
	GitHubBaseURL string
	github        githubCache

	// DefaultEventsLimit and MaxEventsLimit bound ?limit= on the events
	// endpoint. They are resolved from http.events_limit_default and
	// http.events_limit_max once at startup: a change needs a restart, which
	// the settings screen marks. Zero (the test path) falls back to the
	// package constants.
	DefaultEventsLimit int
	MaxEventsLimit     int

	// OnSettingChanged, when set, is called after a successful write to key
	// — handlePutSetting's Set and handleDeleteSetting's Unset both call it,
	// never on a rejected value. cmd/harness/serve.go supplies the closure
	// that re-runs internal/githubauth.Sync when key is
	// settings.KeyGitHubToken, so a token typed into the settings screen
	// takes effect without a restart; this package never imports
	// internal/githubauth itself, only this hook, which is what keeps
	// composition in cmd/harness and nowhere else (ARCHITECTURE.md). Nil is
	// safe — every test that builds a Server directly leaves it unset — and
	// a callback error is the caller's to log, not this package's: the HTTP
	// response has already been decided by the write that preceded it.
	OnSettingChanged func(ctx context.Context, key string)
}

// Handler returns the harness's whole HTTP surface. methodGate runs before
// routing, so a request outside the method allowlist is rejected on every
// path, including ones nothing here recognises: GET and HEAD everywhere, and
// the writing methods where a write route exists — PUT and DELETE on a
// settings key path, PATCH and DELETE on one session's path, POST on the MCP
// server collection and its refresh subresource and PATCH/DELETE on one MCP
// server, and POST on the
// run-control subresources (docs/DESIGN.md §4.2, docs/DATA-API.md,
// docs/RUN-CONTROL.md, docs/MCP.md).
func (s *Server) Handler() http.Handler {
	return methodGate(s.routes())
}

// routes builds the router: every pattern the surface serves, method and
// path. Handler wraps it in the method gate; the bare router is also
// exposed for the test that asserts the events resource carries no
// mutating route (docs/DATA-API.md "Events are not writable") — a route
// registered on it later fails that test rather than passing behind the
// gate unnoticed.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("GET /api/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("PATCH /api/sessions/{id}", s.handlePatchSession)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleDeleteSession)
	mux.HandleFunc("POST /api/sessions/{id}/stop", s.handleStopSession)
	mux.HandleFunc("POST /api/sessions/{id}/steer", s.handleSteerSession)
	mux.HandleFunc("POST /api/sessions/{id}/resume", s.handleResumeSession)
	mux.HandleFunc("POST /api/runs", s.handleStartRun)
	mux.HandleFunc("GET /api/sessions/{id}/events", s.handleGetEvents)
	mux.HandleFunc("GET /api/sessions/{id}/stream", s.handleSessionStream)
	mux.HandleFunc("GET /api/sessions/{id}/snapshot", s.handleGetSessionSnapshot)
	mux.HandleFunc("GET /api/sessions/{id}/events/{seq}/image", s.handleGetEventImage)
	mux.HandleFunc("GET /api/sessions/{id}/screenshot", s.handleGetScreenshot)
	mux.HandleFunc("GET /api/control-token", s.handleGetControlToken)
	mux.HandleFunc("GET /api/requests", s.handleListWorkRequests)
	mux.HandleFunc("GET /api/requests/{request_id}", s.handleGetWorkRequest)
	mux.HandleFunc("PATCH /api/requests/{request_id}", s.handlePatchWorkRequest)
	mux.HandleFunc("DELETE /api/requests/{request_id}", s.handleDeleteWorkRequest)
	mux.HandleFunc("GET /api/requests/{request_id}/status", s.handleRequestStatus)
	mux.HandleFunc("GET /api/leases", s.handleListLeases)
	// The lease key is a workspace path, which contains slashes, so the
	// segment is the rest of the path after /api/leases/ — a client
	// percent-encodes each slash (DELETE /api/leases/%2Ftmp%2Fws) and the
	// wildcard matches the remainder exactly (docs/DATA-API.md).
	mux.HandleFunc("DELETE /api/leases/{workspace...}", s.handleDeleteLease)
	mux.HandleFunc("GET /api/stream", s.handleListStream)
	mux.HandleFunc("GET /api/queue", s.handleQueueHealth)
	mux.HandleFunc("GET /api/pricing", s.handlePricing)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings/{key}", s.handlePutSetting)
	mux.HandleFunc("DELETE /api/settings/{key}", s.handleDeleteSetting)
	mux.HandleFunc("GET /api/evals", s.handleListEvals)
	mux.HandleFunc("POST /api/evals", s.handleStartEval)
	mux.HandleFunc("POST /api/evals/{id}/cancel", s.handleCancelEval)
	mux.HandleFunc("PATCH /api/evals/{id}", s.handlePatchEval)
	mux.HandleFunc("DELETE /api/evals/{id}", s.handleDeleteEval)
	mux.HandleFunc("GET /api/evals/stream", s.handleEvalListStream)
	mux.HandleFunc("GET /api/evals/{id}/stream", s.handleEvalStream)
	mux.HandleFunc("GET /api/evals/suites", s.handleListEvalSuites)
	mux.HandleFunc("GET /api/evals/variants", s.handleListEvalVariants)
	mux.HandleFunc("GET /api/evals/{id}", s.handleGetEval)
	mux.HandleFunc("GET /api/sessions/{id}/eval", s.handleGetSessionEval)
	mux.HandleFunc("GET /api/github/repos", s.handleListGithubRepos)
	mux.HandleFunc("GET /api/models", s.handleListModels)
	mux.HandleFunc("GET /api/mcp/servers", s.handleListMCPServers)
	mux.HandleFunc("POST /api/mcp/servers", s.handleCreateMCPServer)
	mux.HandleFunc("PATCH /api/mcp/servers/{name}", s.handlePatchMCPServer)
	mux.HandleFunc("DELETE /api/mcp/servers/{name}", s.handleDeleteMCPServer)
	mux.HandleFunc("POST /api/mcp/servers/{name}/refresh", s.handleRefreshMCPServer)
	mux.HandleFunc("POST /api/mcp/servers/{name}/complete", s.handleCompleteMCPServer)
	mux.Handle("/", s.Static)
	return mux
}
