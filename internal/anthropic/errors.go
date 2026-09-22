package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIError is a non-2xx response, or a mid-stream error event, from the
// Messages API (docs/ANTHROPIC-INTEGRATION.md, "Errors and retry").
type APIError struct {
	StatusCode int
	Type       string
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("anthropic: %d %s: %s", e.StatusCode, e.Type, e.Message)
	}
	return fmt.Sprintf("anthropic: unexpected status %d", e.StatusCode)
}

// IsAPIError reports whether err is an *APIError, unwrapping through
// fmt.Errorf's %w chain the way errors.As does.
func IsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}

// RefusalError is what a 200 response carrying stop_reason "refusal" ends
// the turn with (docs/ANTHROPIC-INTEGRATION.md, "Errors and retry"):
// Claude declined to continue, and StopDetails is whatever the response's
// own stop_details carried, kept verbatim rather than parsed since its
// shape is not otherwise needed by this client.
type RefusalError struct {
	StopDetails json.RawMessage
}

func (e *RefusalError) Error() string {
	if len(e.StopDetails) > 0 {
		return fmt.Sprintf("anthropic: refusal: %s", string(e.StopDetails))
	}
	return "anthropic: refusal"
}

// parseAPIError reads and closes resp.Body, building an *APIError from the
// {"type":"error","error":{"type":...,"message":...}} envelope, falling
// back to the raw body if that does not parse.
func parseAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	var wrapped apiErrorBody
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Error.Message != "" {
		return &APIError{StatusCode: resp.StatusCode, Type: wrapped.Error.Type, Message: wrapped.Error.Message, RequestID: wrapped.RequestID}
	}
	return &APIError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(body))}
}

// isRetryableStatus reports whether a response status from POST
// /v1/messages should be retried with backoff: 429 (rate limit), the 5xx
// server faults, and 529 (overloaded) — the set
// docs/ANTHROPIC-INTEGRATION.md's "Errors and retry" names, read off
// <https://platform.claude.com/docs/en/api/errors>.
func isRetryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		529:
		return true
	default:
		return false
	}
}
