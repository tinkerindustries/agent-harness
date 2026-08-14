package deepseek

import "testing"

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 503}
	notRetryable := []int{400, 401, 402, 422, 200, 201, 301, 404, 502, 504}

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
