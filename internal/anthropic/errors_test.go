package anthropic

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseAPIError(t *testing.T) {
	rr := httptest.NewRecorder()
	rr.WriteHeader(http.StatusNotFound)
	rr.Body.WriteString(`{"type":"error","error":{"type":"not_found_error","message":"The requested resource could not be found."},"request_id":"req_123"}`)
	resp := rr.Result()

	err := parseAPIError(resp)
	apiErr, ok := IsAPIError(err)
	if !ok {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
	if apiErr.Type != "not_found_error" {
		t.Errorf("Type = %q", apiErr.Type)
	}
	if apiErr.RequestID != "req_123" {
		t.Errorf("RequestID = %q", apiErr.RequestID)
	}
}

func TestParseAPIErrorFallsBackToRawBody(t *testing.T) {
	rr := httptest.NewRecorder()
	rr.WriteHeader(http.StatusInternalServerError)
	rr.Body.WriteString("not json")
	resp := rr.Result()

	err := parseAPIError(resp)
	apiErr, ok := IsAPIError(err)
	if !ok {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("StatusCode = %d", apiErr.StatusCode)
	}
	if apiErr.Message != "not json" {
		t.Errorf("Message = %q", apiErr.Message)
	}
}

func TestIsRetryableStatus(t *testing.T) {
	retryable := []int{429, 500, 502, 503, 504, 529}
	for _, code := range retryable {
		if !isRetryableStatus(code) {
			t.Errorf("isRetryableStatus(%d) = false, want true", code)
		}
	}
	notRetryable := []int{200, 400, 401, 402, 403, 404, 409, 413}
	for _, code := range notRetryable {
		if isRetryableStatus(code) {
			t.Errorf("isRetryableStatus(%d) = true, want false", code)
		}
	}
}

func TestRefusalErrorMessage(t *testing.T) {
	err := &RefusalError{StopDetails: []byte(`{"type":"refusal_details"}`)}
	if err.Error() == "" {
		t.Error("Error() is empty")
	}
	empty := &RefusalError{}
	if empty.Error() == "" {
		t.Error("Error() is empty for a refusal with no stop_details")
	}
}
