package session

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/fold"
	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/gemini/geminitest"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// TestRunStoresGeminiThoughtSignature is the capture-to-store half of
// docs/GEMINI-INTEGRATION.md §7 "Phase 6": a real *gemini.Client streaming a
// thought step (geminitest.Stream's fixed "sig" signature, per its own
// wire-format comment) followed by a model_output step must leave a
// reasoning_delta event in the log carrying ThoughtSignature, and folding
// that log must produce an assistant message with Message.ThoughtSignature
// set — the whole round trip turn.go's stream loop and runSubTurn exist to
// carry (internal/wire.EventThoughtSignatureDelta in, store.ReasoningDeltaPayload
// out).
func TestRunStoresGeminiThoughtSignature(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(geminitest.Stream([]geminitest.Step{
			{Type: "thought", Summaries: []string{"thinking it over"}},
			{Type: "model_output", Texts: []string{"all done"}},
		}, `{"total_tokens":10,"total_input_tokens":5,"total_cached_tokens":0,"total_output_tokens":5,"total_thought_tokens":0}`)))
	}))
	defer srv.Close()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "harness.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	r := &Runner{
		Store:      st,
		Mirror:     store.NewMirror(filepath.Join(dir, "mirror")),
		Client:     gemini.NewClient(srv.URL, gemini.WithAPIKeyProvider(func() (string, error) { return "gk-test", nil })),
		Prices:     testPrices(),
		FlashModel: "test-model",
	}

	res, err := r.Run(t.Context(), RunOptions{
		Model: "test-model", Effort: wire.EffortHigh, Thinking: true, MaxTokens: 4000,
		Workspace: t.TempDir(), PermissionMode: tools.ModeFull, Prompt: "go",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Status != store.StatusOK {
		t.Fatalf("expected status ok, got %s", res.Status)
	}

	events, err := r.Store.GetEvents(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	var found bool
	for _, ev := range events {
		if ev.Kind != store.KindReasoningDelta {
			continue
		}
		var p store.ReasoningDeltaPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("decode reasoning_delta: %v", err)
		}
		found = true
		if p.Text != "thinking it over" {
			t.Errorf("reasoning_delta.text = %q, want %q", p.Text, "thinking it over")
		}
		if p.ThoughtSignature != "sig" {
			t.Errorf("reasoning_delta.thought_signature = %q, want %q (geminitest's fixed value)", p.ThoughtSignature, "sig")
		}
	}
	if !found {
		t.Fatal("expected a reasoning_delta event, found none")
	}

	sess, err := r.Store.GetSession(t.Context(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	items, err := fold.Fold(sess, events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	var sawReasoning bool
	for _, item := range items {
		if item.Type != wire.ItemReasoning {
			continue
		}
		sawReasoning = true
		if item.ThoughtSignature != "sig" {
			t.Errorf("reasoning item ThoughtSignature = %q, want %q", item.ThoughtSignature, "sig")
		}
	}
	if !sawReasoning {
		t.Fatal("expected a reasoning item in the fold")
	}
}
