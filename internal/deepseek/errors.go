package deepseek

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// APIError is a non-2xx response from the API.
type APIError struct {
	StatusCode int
	Type       string
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("deepseek: %d %s: %s", e.StatusCode, e.Type, e.Message)
	}
	return fmt.Sprintf("deepseek: unexpected status %d", e.StatusCode)
}

// IsInsufficientBalance reports whether err is a 402 from the API, meaning
// the account balance is exhausted rather than the harness being broken.
// 402 is never retried; the caller decides how to surface it.
func IsInsufficientBalance(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusPaymentRequired
	}
	return false
}

// parseAPIError reads and closes resp.Body, building an *APIError from
// whatever it contains. The error body shape is not in the vendored docs;
// this follows the OpenAI-compatible {"error": {...}} envelope DeepSeek's
// samples imply and falls back to the raw body if that doesn't parse.
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
