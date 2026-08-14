package deepseek

import "net/http"

// isRetryableStatus reports whether a response status code should be
// retried with backoff. 429, 500, and 503 are transient. 400, 401, 402, and
// 422 are a malformed request or an empty account, never transient, and a
// retry only burns a turn (docs/DESIGN.md §4.5). The backoff schedule
// itself is internal/providerhttp.BackoffDelay, shared with internal/kimi;
// this predicate is the one place the two providers' retry behaviour
// differs (Kimi also retries 504).
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable:
		return true
	default:
		return false
	}
}
