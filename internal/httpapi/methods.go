package httpapi

import (
	"net/http"
	"slices"
	"strings"
)

// The method gate: what methodGate itself does, and the path-shape
// predicates and route table (writeAllowed, allowedMethods) that decide
// which writing method each resource accepts. GET and HEAD need no entry
// here — they pass on every path — so everything below is about the
// narrower set of paths a write route exists for (docs/DATA-API.md,
// docs/RUN-CONTROL.md).

// methodGate enforces the method allowlist ahead of any routing decision. GET
// and HEAD pass on every path; the writing methods pass only where a write
// route exists — PUT and DELETE on a settings key path, PATCH and DELETE on a
// session or work-request path, DELETE on a lease path, POST on the MCP
// server collection and one server's refresh subresource and PATCH/DELETE on
// one server's own path, and POST on the
// run-control subresources (docs/DATA-API.md, docs/RUN-CONTROL.md,
// docs/MCP.md). A
// pattern registered with a method already 405s a wrong-method request that
// matches its path (net/http's ServeMux does this since Go 1.22), but that
// only covers paths this package recognises; the static handler's "/" pattern
// matches everything, method or not, and a POST to a path nobody registered
// would otherwise fall through to a 404 rather than the 405 docs/DESIGN.md
// §4.2 requires everywhere else.
//
// The gate decides against the escaped path, matching how the mux matches:
// a workspace lease key is a path containing slashes, and a client
// percent-encodes them (DELETE /api/leases/%2Ftmp%2Fws), so the escaped form
// is the one whose shape identifies the route.
func methodGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.Method
		if method == http.MethodGet || method == http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		path := r.URL.EscapedPath()
		if writeAllowed(method, path) {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Allow", allowedMethods(path))
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	})
}

// writeRoute pairs a path-shape predicate with the writing methods the gate
// allows there. writeRoutes is the single ordered list writeAllowed and
// allowedMethods both look up, so a phase that adds a resource extends one
// list rather than two switches that had to be kept in lockstep by hand —
// the two used to disagree on nothing only because every edit remembered to
// touch both; a route added to one and not the other would 405 with a wrong
// Allow header, silently.
//
// Order is significant and preserved from the two switches this replaced:
// isEvalPath must keep losing to isEvalsCollectionPath and
// isEvalCancelPath, isStopPath/isSteerPath must keep being distinct
// from isSessionPath, and isMCPServerRefreshPath must keep being consulted
// before isMCPServerPath — the same shape isStopPath takes against
// isSessionPath — for the reasons each predicate's own doc comment gives
// below.
type writeRoute struct {
	match   func(path string) bool
	methods []string
}

var writeRoutes = []writeRoute{
	{isSettingsKeyPath, []string{http.MethodPut, http.MethodDelete}},
	{isSessionPath, []string{http.MethodPatch, http.MethodDelete}},
	{isRequestPath, []string{http.MethodPatch, http.MethodDelete}},
	{isLeasePath, []string{http.MethodDelete}},
	{isStopPath, []string{http.MethodPost}},
	{isSteerPath, []string{http.MethodPost}},
	{isRunsPath, []string{http.MethodPost}},
	{isEvalsCollectionPath, []string{http.MethodPost}},
	{isEvalCancelPath, []string{http.MethodPost}},
	{isEvalPath, []string{http.MethodPatch, http.MethodDelete}},
	{isMCPServersCollectionPath, []string{http.MethodPost}},
	{isMCPServerRefreshPath, []string{http.MethodPost}},
	{isMCPServerPath, []string{http.MethodPatch, http.MethodDelete}},
}

// writeAllowed reports whether method is a writing method the surface allows
// on path, by the first writeRoutes entry whose predicate matches.
func writeAllowed(method, path string) bool {
	for _, route := range writeRoutes {
		if route.match(path) {
			return slices.Contains(route.methods, method)
		}
	}
	return false
}

// isSettingsKeyPath reports whether path is one key's settings resource —
// /api/settings/<key>, exactly one key segment. PUT and DELETE may pass the
// gate here and nowhere else; the mux's own PUT/DELETE patterns then handle
// it, and the handler rejects a key outside settings.ValidKeys.
func isSettingsKeyPath(path string) bool {
	const prefix = "/api/settings/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}

// isSessionPath reports whether path is exactly one session's resource —
// /api/sessions/<id> with no further segments. The collection
// (/api/sessions) and the event and stream subresources carry their own
// rules: events are never writable (docs/DATA-API.md).
func isSessionPath(path string) bool {
	const prefix = "/api/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}

// isStopPath reports whether path is one session's stop subresource —
// /api/sessions/<id>/stop, exactly one id segment and the literal "stop".
// POST may pass the gate here and nowhere else. It is deliberately separate
// from isSessionPath, which requires no further segments and must keep PATCH
// and DELETE scoped to the row: a stop is an action on a run, not an edit of
// a row (docs/RUN-CONTROL.md "The HTTP surface").
func isStopPath(path string) bool {
	const prefix = "/api/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	id, tail, ok := strings.Cut(rest, "/")
	return ok && id != "" && tail == "stop"
}

// isSteerPath reports whether path is one session's steer subresource —
// /api/sessions/<id>/steer, exactly one id segment and the literal "steer".
// POST may pass the gate here and nowhere else, the same shape rule as
// isStopPath: it is an action on a run (docs/RUN-CONTROL.md "The HTTP
// surface"), not an edit of the row, and isSessionPath must not be widened to
// cover it.
func isSteerPath(path string) bool {
	const prefix = "/api/sessions/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	id, tail, ok := strings.Cut(rest, "/")
	return ok && id != "" && tail == "steer"
}

// isRunsPath reports whether path is the runs collection — /api/runs, with
// no further segments. POST may pass the gate here and nowhere else: starting
// a run is an action, not a row edit, and the collection has no other write
// route (docs/RUN-CONTROL.md "The HTTP surface"). A POST to any other path
// stays a 405 with a correct Allow header.
func isRunsPath(path string) bool {
	return path == "/api/runs"
}

func isEvalsCollectionPath(path string) bool {
	return path == "/api/evals"
}

// isEvalPath reports whether path is exactly one eval run's resource. The
// suites and variants collections sit under /api/evals/ too and are reads, so
// they must not be mistaken for a run id.
func isEvalPath(path string) bool {
	const prefix = "/api/evals/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	if rest == "" || strings.Contains(rest, "/") {
		return false
	}
	return rest != "suites" && rest != "variants" && rest != "stream"
}

func isEvalCancelPath(path string) bool {
	const prefix = "/api/evals/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	id, tail, ok := strings.Cut(strings.TrimPrefix(path, prefix), "/")
	return ok && id != "" && tail == "cancel"
}

// isRequestPath reports whether path is exactly one work request's resource —
// /api/requests/<request_id> with no further segments. The poll snapshot
// subresource /api/requests/<request_id>/status is a read and carries its
// own rule (it is never writable), so the gate keeps it out of the write
// routes.
func isRequestPath(path string) bool {
	const prefix = "/api/requests/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}

// isLeasePath reports whether path is one workspace lease's resource — the
// remainder of /api/leases/ is the lease key, which is a workspace path and
// may itself contain slashes, percent-encoded by the client
// (/api/leases/%2Ftmp%2Fws). The collection (/api/leases) is read-only.
func isLeasePath(path string) bool {
	const prefix = "/api/leases/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != ""
}

// isMCPServersCollectionPath reports whether path is the MCP server
// collection — /api/mcp/servers, with no further segments. POST may pass the
// gate here and nowhere else: registering a server is a create on the
// collection, the same shape /api/runs and /api/evals take (docs/MCP.md).
func isMCPServersCollectionPath(path string) bool {
	return path == "/api/mcp/servers"
}

// isMCPServerRefreshPath reports whether path is one MCP server's refresh
// subresource — /api/mcp/servers/<name>/refresh, exactly one name segment
// and the literal "refresh". POST may pass the gate here and nowhere else.
// It is deliberately checked before isMCPServerPath, the same ordering
// isStopPath takes against isSessionPath: a refresh is an action (probe now),
// not an edit of the row, and isMCPServerPath must not be widened to cover
// it.
func isMCPServerRefreshPath(path string) bool {
	const prefix = "/api/mcp/servers/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	name, tail, ok := strings.Cut(strings.TrimPrefix(path, prefix), "/")
	return ok && name != "" && tail == "refresh"
}

// isMCPServerPath reports whether path is exactly one MCP server's own
// resource — /api/mcp/servers/<name> with no further segments. PATCH and
// DELETE may pass the gate here and nowhere else; the refresh subresource
// carries its own rule above and must be checked first.
func isMCPServerPath(path string) bool {
	const prefix = "/api/mcp/servers/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(path, prefix)
	return rest != "" && !strings.Contains(rest, "/")
}

// allowedMethods names the methods the surface actually allows for path, for
// the Allow header on a rejected request, by the same writeRoutes lookup
// writeAllowed uses. Only paths with a write route allow the writing
// methods; every other path is GET and HEAD.
func allowedMethods(path string) string {
	methods := []string{http.MethodGet, http.MethodHead}
	for _, route := range writeRoutes {
		if route.match(path) {
			return strings.Join(append(methods, route.methods...), ", ")
		}
	}
	return strings.Join(methods, ", ")
}
