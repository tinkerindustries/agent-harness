package gemini

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseAgenticAPIErrorSSEFramed pins the shape docs/OBSERVED.md measured
// for every omitted or corrupted signature: an SSE-framed body on a plain
// non-2xx status. client.go's own parseAPIError, built for the vision path,
// would fall into its raw-body fallback here and report the literal
// "event: error\ndata: ..." text; this must read the real message instead.
func TestParseAgenticAPIErrorSSEFramed(t *testing.T) {
	for _, tc := range []struct {
		name, body, wantMsg, wantCode string
	}{
		{"corrupted", "event: error\ndata: {\"error\":{\"message\":\"Corrupted thought signature.\",\"code\":\"invalid_request\"},\"event_type\":\"error\"}\n\n", "Corrupted thought signature.", "invalid_request"},
		{"missing", "event: error\ndata: {\"error\":{\"message\":\"Request contains an invalid argument.\",\"code\":\"invalid_request\"},\"event_type\":\"error\"}\n\n", "Request contains an invalid argument.", "invalid_request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			resp, err := http.Get(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			gotErr := parseAgenticAPIError(resp)
			if !strings.Contains(gotErr.Error(), tc.wantMsg) {
				t.Fatalf("error = %v, want it to contain %q", gotErr, tc.wantMsg)
			}
			apiErr, ok := IsAPIError(gotErr)
			if !ok {
				t.Fatalf("error = %v (%T), want an *APIError", gotErr, gotErr)
			}
			if apiErr.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", apiErr.Code, tc.wantCode)
			}
			if apiErr.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", apiErr.StatusCode)
			}
		})
	}
}

// TestParseAgenticAPIErrorPlainJSON pins the other shape a non-2xx response
// carries: the plain {"error":{"code":int,"message":...,"status":...}}
// envelope client.go's own parseAPIError reads — the tool_choice-at-top-level
// 400 docs/OBSERVED.md's "tool_choice" section measured is exactly this
// shape, with no SSE framing at all.
func TestParseAgenticAPIErrorPlainJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"Unknown parameter 'tool_choice'.","code":400,"status":"INVALID_ARGUMENT"}}`))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	gotErr := parseAgenticAPIError(resp)
	if !strings.Contains(gotErr.Error(), "Unknown parameter 'tool_choice'.") {
		t.Fatalf("error = %v, want it to carry the plain-JSON message", gotErr)
	}
	apiErr, ok := IsAPIError(gotErr)
	if !ok {
		t.Fatalf("error = %v (%T), want an *APIError", gotErr, gotErr)
	}
	if apiErr.Code != "INVALID_ARGUMENT" {
		t.Errorf("code = %q, want the status string INVALID_ARGUMENT", apiErr.Code)
	}
}

// TestParseAgenticAPIErrorRawFallback pins that a body matching neither
// shape still surfaces as an error carrying the raw text, rather than a
// silent empty message.
func TestParseAgenticAPIErrorRawFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("upstream connect error"))
	}))
	defer srv.Close()

	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	gotErr := parseAgenticAPIError(resp)
	if !strings.Contains(gotErr.Error(), "upstream connect error") {
		t.Fatalf("error = %v, want the raw body carried through", gotErr)
	}
}
