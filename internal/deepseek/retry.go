package deepseek

import "net/http"

// isRetryableStatus reports whether a response status code should be
// retried with backoff. 429, 500, and 503 are transient; 502 and 504 join
// them as the reverse-proxy gateway in front of the API returning its own
// fault rather than the model backend's — a production run hit a live 502
// from that gateway (openresty) after a two-minute hang and ended outright
// for want of this (docs/OBSERVED.md, "A gateway 502 ended a run at
// sub-turn 91"). 400, 401, 402, and 422 are a malformed request or an empty
// account, never transient, and a retry only burns a turn (docs/DESIGN.md
// §4.5). The backoff schedule itself is internal/providerhttp.BackoffDelay,
// shared with internal/kimi, whose predicate now agrees on all five codes.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	default:
		return false
	}
}
