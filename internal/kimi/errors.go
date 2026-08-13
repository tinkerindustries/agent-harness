package kimi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIError is a non-2xx response from the API. Kimi's error body is the
// documented {"error": {"type": ..., "message": ..., "code": ...}} envelope
// (third_party/kimi-docs/api/errors.md, openapi.json ErrorResponse).
type APIError struct {
	StatusCode int
	Type       string
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("kimi: %d %s: %s", e.StatusCode, e.Type, e.Message)
	}
	return fmt.Sprintf("kimi: unexpected status %d", e.StatusCode)
}

// IsInsufficientBalance reports whether err is an exhausted Kimi account.
// Kimi has no 402: when the available balance is zero or the account is
// disabled, the API answers 429 with error type exceeded_current_quota_error
// (third_party/kimi-docs/api/errors.md, api/balance.md). A 429 with any
// other type is a rate limit, not a balance problem, and stays retryable.
func IsInsufficientBalance(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusTooManyRequests && apiErr.Type == "exceeded_current_quota_error"
	}
	return false
}

// parseAPIError reads and closes resp.Body, building an *APIError from
// whatever it contains. The envelope is the documented
// {"error": {message, type, code}} shape (third_party/kimi-docs/api/errors.md);
// a body that does not parse falls back to the raw text so the operator
// still sees what the server said.
func parseAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	apiErr := &APIError{StatusCode: resp.StatusCode}
	var wrapped struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Error.Message != "" {
		apiErr.Message = wrapped.Error.Message
		apiErr.Type = wrapped.Error.Type
		apiErr.Code = wrapped.Error.Code
	} else {
		apiErr.Message = strings.TrimSpace(string(body))
	}
	return apiErr
}
