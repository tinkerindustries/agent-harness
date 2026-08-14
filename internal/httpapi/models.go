package httpapi

import (
	"net/http"

	"github.com/mrgeoffrich/deepseek-harness/internal/provider"
)

// The model list behind GET /api/models (docs/DATA-API.md "models"). A
// read-only, config-adjacent endpoint in the same shape as the GitHub repo
// list (github.go): it serves the one model→provider table
// (internal/provider), which is what validates a work request
// (internal/queue) and routes a run to its client (cmd/harness) — so the
// browser's model dropdowns are fed by the same list the rest of the harness
// runs on, not by a hardcoded copy that drifts (the system prompt's tool
// list and the tool array drifted apart once, which is why
// TestPromptNamesExactlyTheToolArray exists). It needs no store, no seams,
// and no write guards, because it is a GET with no side effects, exactly like
// handleGetSettings.

// modelsResponse is GET /api/models's 200 body. The names come from
// provider.KnownModels(), which is already sorted, so the order the form
// renders is the order every other consumer of the table sees.
type modelsResponse struct {
	Models []string `json:"models"`
}

// handleListModels serves GET /api/models. There is nothing to fail here —
// the table is compiled in — so the only answer is 200 with the list.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, modelsResponse{Models: provider.KnownModels()})
}
