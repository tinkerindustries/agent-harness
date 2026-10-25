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
	// PrefixMismatch is the anthropic-thinking-prefix-mismatch response
	// header, when the API sent one: its unpublished diagnosis of a
	// thinking block that failed preserved thinking's history-editing
	// check — the shape of the edit and the first changed path. Kept for
	// the log line alone; nothing branches on it, because Anthropic may
	// change or drop it without notice.
	PrefixMismatch string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("anthropic: %d %s: %s", e.StatusCode, e.Type, e.Message)
	}
	return fmt.Sprintf("anthropic: unexpected status %d", e.StatusCode)
}

// IsThinkingPrefixMismatch reports whether e is the 400 preserved thinking's
// history-editing check answers when a replayed thinking block's
// conversation prefix differs from the one it was minted on:
// "messages.N.content.M: Invalid `signature` in `thinking` block. The block
// is bound to a different conversation. …" A tampered or undecryptable
// signature fails with the same leading clause and without that sentence,
// and is not this — drop_block does not apply to it
// (docs/ANTHROPIC-INTEGRATION.md, "When a replayed block no longer
// matches").
func (e *APIError) IsThinkingPrefixMismatch() bool {
	return e.StatusCode == http.StatusBadRequest && e.Type == "invalid_request_error" &&
		strings.Contains(e.Message, "bound to a different conversation")
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

// prefixMismatchHeader is the response header that sometimes accompanies a
// thinking block failing the history-editing check (APIError.PrefixMismatch).
const prefixMismatchHeader = "anthropic-thinking-prefix-mismatch"

// parseAPIError reads and closes resp.Body, building an *APIError from the
// {"type":"error","error":{"type":...,"message":...}} envelope, falling
// back to the raw body if that does not parse.
func parseAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	mismatch := resp.Header.Get(prefixMismatchHeader)

	var wrapped apiErrorBody
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Error.Message != "" {
		return &APIError{StatusCode: resp.StatusCode, Type: wrapped.Error.Type, Message: wrapped.Error.Message,
			RequestID: wrapped.RequestID, PrefixMismatch: mismatch}
	}
	return &APIError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(body)), PrefixMismatch: mismatch}
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
