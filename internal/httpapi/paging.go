package httpapi

import (
	"net/http"
)

const (
	// defaultPageLimit and maxPageLimit are the built-in paging bounds for
	// the offset pager (Page), beside the events endpoint's limits above.
	// They are deliberately NOT wired into the settings registry — the
	// events limits are, and that is a different problem: the events pager
	// bounds a per-session read whose cost the operator tunes, while a page
	// of the session list is a fixed 20 rows by design. No settings keys
	// here.
	defaultPageLimit = 20
	maxPageLimit     = 200
)

// Page is the envelope every paginated list endpoint returns.
type Page[T any] struct {
	Items   []T  `json:"items"`
	Total   int  `json:"total"` // rows matching the filter, ignoring limit/offset
	Limit   int  `json:"limit"` // the limit actually applied, after clamping
	Offset  int  `json:"offset"`
	HasMore bool `json:"has_more"`       // offset+len(items) < total
	Next    *int `json:"next,omitempty"` // the offset to ask for next; set only when HasMore
}

// parsePaging reads ?limit= and ?offset= with the same forgiving shape the
// events endpoint uses: clamp, never 400. A limit that is absent, non-numeric,
// <= 0, or > max falls back to def; a negative offset clamps to 0.
func parsePaging(r *http.Request, def, max int) (limit, offset int) {
	limit = parseInt(r.URL.Query().Get("limit"), def)
	if limit <= 0 || limit > max {
		limit = def
	}
	offset = parseInt(r.URL.Query().Get("offset"), 0)
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// newPage assembles the envelope for one fetched page. HasMore is exactly
// "offset+len(items) < total", and Next — set only when HasMore — is the
// offset the next page starts at. Items is guaranteed to marshal as []
// rather than null, so an empty page has one shape on the wire.
func newPage[T any](items []T, total, limit, offset int) Page[T] {
	if items == nil {
		items = []T{}
	}
	page := Page[T]{Items: items, Total: total, Limit: limit, Offset: offset}
	page.HasMore = offset+len(items) < total
	if page.HasMore {
		next := offset + len(items)
		page.Next = &next
	}
	return page
}
