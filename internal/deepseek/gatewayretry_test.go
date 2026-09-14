package deepseek

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// gatewayHTMLBody is the shape a reverse-proxy gateway sends on its own
// fault, not the model backend's: an HTML page rather than the {"error":
// {...}} envelope parseAPIError otherwise expects. The production incident
// this closes carried exactly this (docs/OBSERVED.md, "A gateway 502 ended
// a run at sub-turn 91").
const gatewayHTMLBody = "<html>\r\n<head><title>%d %s</title></head>\r\n<body>\r\n<center><h1>%d %s</h1></center>\r\n<hr><center>openresty</center>\r\n</body>\r\n</html>"

func gatewayPage(status int) string {
	text := http.StatusText(status)
	return fmt.Sprintf(gatewayHTMLBody, status, text, status, text)
}

// TestStreamChatCompletionRetriesGatewayStatusThenSucceeds is the
// production incident (docs/OBSERVED.md, "A gateway 502 ended a run at
// sub-turn 91"): DeepSeek's fronting gateway answered a plain 502 with an
// HTML body, on a request that had not yet reached the model at all. Both
// codes that motivated adding this — the observed 502 and the 504 added for
// consistency with the same gateway fault — must be retried on
// Transport.Do before the stream ever starts, exactly like a pre-stream 503
// already was.
func TestStreamChatCompletionRetriesGatewayStatusThenSucceeds(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			attempts := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts++
				if attempts < 2 {
					w.Header().Set("Content-Type", "text/html")
					w.WriteHeader(status)
					fmt.Fprint(w, gatewayPage(status))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()

			c := NewClient(srv.URL, "test-key")
			shrinkBackoff(c)

			events, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{
				Model: "deepseek-flash", Items: []wire.Item{wire.UserItem("hi")}, MaxTokens: 100,
			})
			if err != nil {
				t.Fatalf("StreamChatCompletion: %v", err)
			}
			got := drainStreamEvents(t, events)

			if attempts != 2 {
				t.Fatalf("server saw %d attempt(s), want 2 (one %d plus the success)", attempts, status)
			}
			var content string
			for _, ev := range got {
				if ev.Type == wire.EventError {
					t.Fatalf("unexpected error event reached the caller: %v (the %d should have been retried invisibly)", ev.Err, status)
				}
				if ev.Type == wire.EventContentDelta {
					content += ev.Content
				}
			}
			if content != "hello" {
				t.Errorf("content = %q, want %q", content, "hello")
			}
		})
	}
}

// TestStreamChatCompletionGatewayStatusExhaustsRetryBudget pins the other
// half: a 502 that never clears burns the retry budget and then ends the
// turn with an error the caller can see, rather than retrying forever. The
// HTML body must still decode into a usable *APIError — parseAPIError falls
// back to the raw, trimmed body when it is not the {"error": {...}}
// envelope — and the retry decision itself must have been made on the
// status code alone, before that body was ever read: Transport.Do checks
// resp.StatusCode against Retryable and only the final, exhausted attempt's
// body reaches parseAPIError.
func TestStreamChatCompletionGatewayStatusExhaustsRetryBudget(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, gatewayPage(http.StatusBadGateway))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	shrinkBackoff(c)
	c.transport.MaxRetries = 2

	_, err := c.StreamChatCompletion(context.Background(), wire.ChatIntent{
		Model: "deepseek-flash", Items: []wire.Item{wire.UserItem("hi")}, MaxTokens: 100,
	})
	if err == nil {
		t.Fatal("expected an error ending the turn")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway {
		t.Errorf("StatusCode = %d, want %d", apiErr.StatusCode, http.StatusBadGateway)
	}
	if attempts != 3 {
		t.Errorf("server saw %d attempt(s), want 3 (MaxRetries=2 retries plus the initial attempt)", attempts)
	}
}
