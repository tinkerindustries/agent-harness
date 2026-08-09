package deepseek

import (
	"math/rand"
	"net/http"
	"time"
)

// isRetryableStatus reports whether a response status code should be
// retried with backoff. 429, 500, and 503 are transient. 400, 401, 402, and
// 422 are a malformed request or an empty account, never transient, and a
// retry only burns a turn (docs/DESIGN.md §4.5).
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable:
		return true
	default:
		return false
	}
}

// backoffDelay returns a full-jitter exponential delay for the given retry
// attempt (0-indexed), capped at max.
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	ceiling := base << uint(attempt)
	if ceiling <= 0 || ceiling > max {
		ceiling = max
	}
	return time.Duration(rand.Int63n(int64(ceiling) + 1))
}
