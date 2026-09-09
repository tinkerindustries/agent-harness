package kimi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// TestRequestFromIntentCarriesReasoningEffortAndNoThinking pins K3's
// reasoning control on the serialised bytes: the intent's effort becomes the
// top-level reasoning_effort, and the intent's Thinking flag — which K3 must
// never see — produces no thinking field at all, because sending `thinking`
// to K3 is an error (third_party/kimi-docs/api/models-overview.md,
// guide/use-reasoning-effort.md).
func TestRequestFromIntentCarriesReasoningEffortAndNoThinking(t *testing.T) {
	intent := wire.ChatIntent{
		Model:     "kimi-k3",
		Items:     []wire.Item{wire.UserItem("hello")},
		Effort:    wire.EffortHigh,
		Thinking:  true,
		MaxTokens: 48000,
	}
	body, err := json.Marshal(requestFromIntent(intent))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(body)
	if !strings.Contains(s, `"reasoning_effort":"high"`) {
		t.Errorf("request body has no top-level reasoning_effort: %s", s)
	}
	if strings.Contains(s, `"thinking"`) {
		t.Errorf("request body carries a thinking field, which kimi-k3 rejects: %s", s)
	}
}

// TestRequestFromIntentOmitsForbiddenSamplingParameters pins that none of
// the sampling parameters K3 fixes server-side are ever sent — temperature,
// top_p, n, presence_penalty, frequency_penalty — because passing any other
// value than the fixed one is an error (third_party/kimi-docs/api/models-overview.md
// "Cannot be modified"). The wire request type has no fields for them, and
// the intent must not smuggle them in.
func TestRequestFromIntentOmitsForbiddenSamplingParameters(t *testing.T) {
	intent := wire.ChatIntent{
		Model:     "kimi-k3",
		Items:     []wire.Item{wire.UserItem("hello")},
		Effort:    wire.EffortMax,
		MaxTokens: 48000,
	}
	body, err := json.Marshal(requestFromIntent(intent))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	s := string(body)
	for _, forbidden := range []string{"temperature", "top_p", `"n"`, "presence_penalty", "frequency_penalty"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("request body carries forbidden sampling parameter %q: %s", forbidden, s)
		}
	}
}

// TestStreamRequestBodyOnTheWire is the same assertion one level up: not the
// translated struct, but the exact bytes the httptest server receives from a
// real StreamChatCompletion call — reasoning_effort present, no thinking, no
// sampling parameters, and stream plus stream_options set the way the
// harness streams everywhere (third_party/kimi-docs/api/chat.md
// "stream_options").
func TestStreamRequestBodyOnTheWire(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-key")
	intent := wire.ChatIntent{
		Model:     "kimi-k3",
		Items:     []wire.Item{wire.UserItem("hello")},
		Effort:    wire.EffortLow,
		Thinking:  true,
		MaxTokens: 48000,
	}
	events, err := c.StreamChatCompletion(context.Background(), intent)
	if err != nil {
		t.Fatalf("StreamChatCompletion: %v", err)
	}
	for range events {
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("server received a body that is not JSON: %v: %s", err, gotBody)
	}
	if _, ok := decoded["reasoning_effort"]; !ok {
		t.Errorf("server received no reasoning_effort field: %s", gotBody)
	}
	if _, ok := decoded["thinking"]; ok {
		t.Errorf("server received a thinking field, which kimi-k3 rejects: %s", gotBody)
	}
	for _, forbidden := range []string{"temperature", "top_p", "n", "presence_penalty", "frequency_penalty"} {
		if _, ok := decoded[forbidden]; ok {
			t.Errorf("server received forbidden sampling parameter %q: %s", forbidden, gotBody)
		}
	}
	if _, ok := decoded["stream"]; !ok {
		t.Errorf("server received no stream field: %s", gotBody)
	}
}
