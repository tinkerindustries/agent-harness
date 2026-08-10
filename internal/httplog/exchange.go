package httplog

import (
	"net/http"
	"strings"
	"time"
)

// Exchange is one HTTP attempt against the DeepSeek API, serialised as one
// JSON object per line. A retry is its own line, numbered by Attempt; Seq
// counts exchanges from 1 per session.
type Exchange struct {
	Seq         int64             `json:"seq"`
	Attempt     int               `json:"attempt"`
	SessionID   string            `json:"session_id"`
	Method      string            `json:"method"`
	URL         string            `json:"url"`
	ReqHeaders  map[string]string `json:"req_headers"`
	ReqBody     string            `json:"req_body,omitempty"`
	Status      int               `json:"status"`
	RespHeaders map[string]string `json:"resp_headers,omitempty"`
	RespBody    string            `json:"resp_body,omitempty"`
	StartedAt   time.Time         `json:"started_at"`
	TTFBMs      int64             `json:"ttfb_ms"`
	TotalMs     int64             `json:"total_ms"`
	Error       string            `json:"error,omitempty"`
}

// redactedHeaders copies h into a map, replacing the value of every
// sensitive header with "[redacted]" on the recorded copy only. The
// outbound request keeps its real values.
func redactedHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for name, values := range h {
		v := strings.Join(values, ", ")
		if redactedHeader(name) {
			v = "[redacted]"
		}
		out[name] = v
	}
	return out
}

// redactedHeader reports whether a header carries a credential or session
// material that must never reach disk, matched case-insensitively.
func redactedHeader(name string) bool {
	switch strings.ToLower(name) {
	case "authorization", "proxy-authorization", "cookie", "set-cookie":
		return true
	}
	return false
}
