package gemini

import "testing"

// TestIsRetryableStatus pins the set retry.go's own comment argues for: the
// three codes DeepSeek retries (429, 500, 503), on the strength of their
// being the standard transient trio and the vendored docs' silence on this
// endpoint's error behaviour rather than any Gemini-specific citation. 504
// is deliberately absent — see retry.go for why that is not the same
// omission.
func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 503}
	notRetryable := []int{400, 401, 402, 403, 404, 422, 428, 502, 504, 200, 201, 301}

	for _, code := range retryable {
		if !isRetryableStatus(code) {
			t.Errorf("isRetryableStatus(%d) = false, want true", code)
		}
	}
	for _, code := range notRetryable {
		if isRetryableStatus(code) {
			t.Errorf("isRetryableStatus(%d) = true, want false", code)
		}
	}
}
