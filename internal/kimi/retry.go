package kimi

import "net/http"

// isRetryableStatus reports whether a response status code should be
// retried with backoff, mirroring internal/deepseek's classification but for
// the statuses Kimi documents as transient (third_party/kimi-docs/api/errors.md):
//
//   - 429 is rate limiting or quota pressure, where the docs say to back off
//     and retry with exponential backoff — including 429s whose type is
//     exceeded_current_quota_error, which are retried here and then surface
//     as the balance problem they are (api/balance.md);
//   - 500 (server_error / unexpected_output) and 503 (server_unavailable)
//     are "try again later" server faults;
//   - 504 is the gateway timing out after 900 seconds of no response — the
//     docs' fix is to use streaming, which the harness already does, and a
//     request that hit it is worth one retry rather than a hard failure.
//
// 400, 401, 403, and 404 are a malformed request, bad key, missing
// permission, or unknown model — never transient, and a retry only burns a
// turn. 499 (client_closed_request) names a client-side disconnect, which
// retrying cannot fix, so it is not retried either. The backoff schedule
// itself is internal/providerhttp.BackoffDelay, shared with
// internal/deepseek — this predicate is the one place the two providers'
// retry behaviour differs.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
