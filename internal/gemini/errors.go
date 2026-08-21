package gemini

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// apiErrorBody is the error envelope both the SSE-framed and non-streamed
// agentic error shapes carry: {"error":{"code":..., "message":...}}
// (openapi.json "Error" — Code is a URI-shaped string like
// "invalid_request", not the integer code the plain {"error":{"code":int,
// "message":...,"status":...}} envelope client.go's own parseAPIError reads
// for the vision path uses; the two are different envelopes for different
// failure classes, not two spellings of one — docs/OBSERVED.md, "The error
// body itself is a finding").
type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// APIError is a non-2xx or mid-stream error from the agentic surface.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		if e.Code != "" {
			return fmt.Sprintf("gemini: %d %s: %s", e.StatusCode, e.Code, e.Message)
		}
		return fmt.Sprintf("gemini: %d: %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("gemini: unexpected status %d", e.StatusCode)
}

// apiErrorFrom builds an *APIError from an error event's already-decoded
// body (pumpChatEvents' mid-stream case, where the HTTP status was 200 by
// the time the event frame carrying the real failure arrived — or, per
// docs/OBSERVED.md, was already a plain 400 whose body just happened to be
// SSE-framed, which parseAgenticAPIError below handles before a stream ever
// starts being pumped). body may be nil if the frame carried an event_type
// of error with no error object at all, which the schema allows.
func apiErrorFrom(body *apiErrorBody) error {
	if body == nil {
		return &APIError{Message: "gemini: error event with no error body"}
	}
	return &APIError{Code: body.Code, Message: body.Message}
}

// parseAgenticAPIError reads and closes resp.Body, building an *APIError
// from whichever of the two shapes it turns out to be.
// docs/OBSERVED.md's central finding for this phase is that a 400 answering
// a bad or missing thought signature arrives SSE-framed —
// "event: error\ndata: {...}\n\n" — even though the request never got as
// far as opening a real stream; client.go's own parseAPIError, built for
// the vision path, expects a bare JSON body and mishandles this, reporting
// the raw SSE text as an opaque status-400 message instead of the real one.
// This tries the SSE frame first, then falls back to the plain
// {"error":{"code":int,...}} shape client.go's parseAPIError reads (the
// tool_choice-nested-wrong 400 is exactly this shape, per docs/OBSERVED.md's
// "tool_choice" section), then to the raw body.
func parseAgenticAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	body := strings.TrimSpace(string(raw))

	if data, ok := sseErrorData(body); ok {
		var wrapped struct {
			Error apiErrorBody `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &wrapped); err == nil && wrapped.Error.Message != "" {
			return &APIError{StatusCode: resp.StatusCode, Code: wrapped.Error.Code, Message: wrapped.Error.Message}
		}
	}

	var plain struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &plain); err == nil && plain.Error.Message != "" {
		code := plain.Error.Status
		if code == "" {
			code = fmt.Sprintf("%d", plain.Error.Code)
		}
		return &APIError{StatusCode: resp.StatusCode, Code: code, Message: plain.Error.Message}
	}

	return &APIError{StatusCode: resp.StatusCode, Message: body}
}

// sseErrorData reports whether body is an SSE-framed error event and, if
// so, returns its data line's content. The framing observed is exactly one
// frame — "event: error\ndata: {...}" — so this looks for that shape rather
// than running a general SSE parse over what is, outside this one case, a
// perfectly ordinary JSON error body.
func sseErrorData(body string) (data string, ok bool) {
	if !strings.HasPrefix(body, "event:") {
		return "", false
	}
	for _, line := range strings.Split(body, "\n") {
		if d, found := strings.CutPrefix(line, "data:"); found {
			return strings.TrimSpace(d), true
		}
	}
	return "", false
}

// IsAPIError reports whether err is an *APIError, unwrapping through
// fmt.Errorf's %w chain the way errors.As does — a small convenience so a
// caller does not need to spell the type assertion.
func IsAPIError(err error) (*APIError, bool) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr, true
	}
	return nil, false
}
