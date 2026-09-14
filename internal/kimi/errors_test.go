package kimi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestIsRetryableStatus pins the status code classification: 429 (rate
// limit / quota pressure), 500, 503, and 504 are transient per
// third_party/kimi-docs/api/errors.md; 502 joins them undocumented, on the
// same gateway-fault reasoning as 503 and 504 and the live DeepSeek 502
// that motivated it (internal/kimi/retry.go, docs/OBSERVED.md). 400, 401,
// 403, 404 (bad request, bad key, permission, unknown model) and 499
// (client-closed request, a client-side disconnect) are not retried.
func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504}
	notRetryable := []int{400, 401, 402, 403, 404, 422, 499, 200, 201, 301}

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

// TestParseAPIErrorKimiEnvelope parses the documented error body
// (third_party/kimi-docs/api/errors.md): {"error": {"type": ..., "message":
// ...}}, with the optional code field, into an *APIError.
func TestParseAPIErrorKimiEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, `{"error":{"type":"invalid_request_error","message":"prompt tokens + max_tokens exceeds the model specification","code":"bad_param"}}`)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	_, err := c.ListModels(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error = %v, want *APIError", err)
	}
	if apiErr.StatusCode != 400 || apiErr.Type != "invalid_request_error" || apiErr.Code != "bad_param" {
		t.Errorf("APIError = %+v, want status 400 type invalid_request_error code bad_param", apiErr)
	}
	if !strings.Contains(apiErr.Error(), "prompt tokens + max_tokens exceeds the model specification") {
		t.Errorf("APIError.Error() = %q, want it to carry the message", apiErr.Error())
	}
}

// TestRetriesTransientStatusThenSucceeds pins that the client retries the
// documented transient statuses with backoff: a 503 and a 500 followed by a
// 200 must yield one successful call, not an error, and each attempt must
// resend the same body and key.
func TestRetriesTransientStatusThenSucceeds(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintln(w, `{"error":{"type":"server_unavailable","message":"The service is temporarily unavailable"}}`)
			return
		}
		fmt.Fprintln(w, `{"object":"list","data":[]}`)
	}))
	defer srv.Close()

	// The real backoff would make this test wait; shrink the schedule.
	c := NewClient(srv.URL, "test-key", WithHTTPClient(&http.Client{}))
	c.transport.RetryBase = time.Millisecond
	c.transport.RetryMax = 5 * time.Millisecond

	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels after transient statuses: %v", err)
	}
	if attempts != 3 {
		t.Errorf("server saw %d attempts, want 3 (one per transient status plus the success)", attempts)
	}
}

// TestDoesNotRetryPermanentStatus pins that 400 and 401 are returned as-is,
// not retried: a malformed request or a bad key is never transient, and a
// retry only burns a turn (third_party/kimi-docs/api/errors.md).
func TestDoesNotRetryPermanentStatus(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		attempts := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			attempts++
			w.WriteHeader(status)
			fmt.Fprintln(w, `{"error":{"type":"invalid_request_error","message":"nope"}}`)
		}))
		c := NewClient(srv.URL, "test-key", WithHTTPClient(&http.Client{}))
		c.transport.RetryBase = time.Millisecond
		c.transport.RetryMax = 5 * time.Millisecond
		_, err := c.ListModels(context.Background())
		if err == nil {
			t.Fatalf("status %d: expected an error, got nil", status)
		}
		if attempts != 1 {
			t.Errorf("status %d: server saw %d attempts, want 1", status, attempts)
		}
		srv.Close()
	}
}

// TestIsInsufficientBalance pins Kimi's empty-account signal: no 402 exists;
// an exhausted account or disabled one answers 429 with error type
// exceeded_current_quota_error, which IsInsufficientBalance must recognise —
// while a plain rate-limit 429 must not count as insufficient balance
// (third_party/kimi-docs/api/errors.md, api/balance.md).
func TestIsInsufficientBalance(t *testing.T) {
	quota := &APIError{StatusCode: 429, Type: "exceeded_current_quota_error", Message: "Account balance is insufficient or the account has been disabled"}
	if !IsInsufficientBalance(quota) {
		t.Error("IsInsufficientBalance(429 exceeded_current_quota_error) = false, want true")
	}
	rateLimit := &APIError{StatusCode: 429, Type: "rate_limit_reached_error", Message: "Organization-level RPM limit reached"}
	if IsInsufficientBalance(rateLimit) {
		t.Error("IsInsufficientBalance(429 rate_limit_reached_error) = true, want false")
	}
	server := &APIError{StatusCode: 500, Type: "server_error", Message: "Internal server error"}
	if IsInsufficientBalance(server) {
		t.Error("IsInsufficientBalance(500 server_error) = true, want false")
	}
	if IsInsufficientBalance(io.EOF) {
		t.Error("IsInsufficientBalance(io.EOF) = true, want false")
	}
}
