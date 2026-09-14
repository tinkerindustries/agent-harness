package gemini

import "net/http"

// isRetryableStatus reports whether a response status code from the
// agentic surface (POST /v1beta/interactions, stream or unary) should be
// retried with backoff.
//
// third_party/gemini-docs/ has nothing to say here: openapi.json documents
// only the 200 response for every operation it describes, and no vendored
// page names a retryable status for this endpoint the way
// third_party/kimi-docs/api/errors.md does for Kimi's. This predicate is
// therefore not a citation the way internal/deepseek's and internal/kimi's
// own are — it takes the same three codes DeepSeek retries (429 rate
// limiting, 500 and 503 server faults) on the strength of their being the
// standard "back off and retry" trio across Google's other Generative
// Language and Cloud APIs, and because there is no Gemini-specific evidence
// to contradict that reading, only silence. 502 and 504 are left out: both
// are now retried for DeepSeek and Kimi — a live 502 from the gateway
// fronting DeepSeek's API is what motivated adding them there
// (docs/OBSERVED.md, "A gateway 502 ended a run at sub-turn 91") — but
// nothing here documents, or has been observed to show, a comparable
// gateway in front of the Interactions API, so including either would be a
// guess dressed as a citation. If a live 502 or 504 is ever observed
// against this surface, docs/OBSERVED.md is where that evidence belongs and
// this predicate should change on the strength of it, not before.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable:
		return true
	default:
		return false
	}
}
