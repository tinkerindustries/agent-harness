package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/gemini/geminitest"
)

// TestInteractSendsStream pins that every call asks for a stream and accepts
// one. The unary form returns nothing until the model has finished thinking,
// which is what put multi-image reviews past their deadline.
func TestInteractSendsStream(t *testing.T) {
	var got InteractionRequest
	var accept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		w.Write([]byte(geminitest.Stream([]geminitest.Step{{Type: "model_output", Texts: []string{"ok"}}}, "")))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, WithAPIKeyProvider(func() (string, error) { return "gk-test", nil }))
	if _, _, err := c.Interact(context.Background(), "gemini-3.5-flash", "", "hello?", nil); err != nil {
		t.Fatalf("Interact: %v", err)
	}
	if !got.Stream {
		t.Error("request did not set stream: true")
	}
	if accept != "text/event-stream" {
		t.Errorf("Accept = %q, want text/event-stream", accept)
	}
}

// TestDecodeStreamAssemblesDeltas pins the assembly: consecutive text deltas
// are fragments of one answer and concatenate with no separator, thought
// summaries land on the thought step's Summary rather than in the answer,
// and the usage on the completed frame is the call's accounting.
func TestDecodeStreamAssemblesDeltas(t *testing.T) {
	raw := geminitest.Stream([]geminitest.Step{
		{Type: "thought", Summaries: []string{"**Reading the screenshot**\n", "Checking the nav rows."}},
		{Type: "model_output", Texts: []string{"The three primary colors", " are red, yellow, and blue."}},
	}, `{"total_tokens":147,"total_input_tokens":13,"total_output_tokens":12,"total_thought_tokens":122}`)

	resp, err := decodeStream(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("decodeStream: %v", err)
	}
	if want := "The three primary colors are red, yellow, and blue."; resp.Text() != want {
		t.Errorf("Text() = %q, want %q", resp.Text(), want)
	}
	if len(resp.Steps) != 2 {
		t.Fatalf("steps = %d, want 2", len(resp.Steps))
	}
	if resp.Steps[0].Type != "thought" {
		t.Errorf("first step type = %q, want thought", resp.Steps[0].Type)
	}
	if len(resp.Steps[0].Summary) != 1 || !strings.HasSuffix(resp.Steps[0].Summary[0].Text, "Checking the nav rows.") {
		t.Errorf("thought summary = %+v, want the two fragments joined", resp.Steps[0].Summary)
	}
	if len(resp.Steps[0].Content) != 0 {
		t.Errorf("a thought step must carry no answer content, got %+v", resp.Steps[0].Content)
	}
	if resp.Status != "completed" || resp.ID != "v1_test" {
		t.Errorf("status/id = %q/%q, want completed/v1_test", resp.Status, resp.ID)
	}
	if resp.Usage == nil || resp.Usage.TotalThoughtTokens != 122 || resp.Usage.TotalTokens != 147 {
		t.Errorf("usage = %+v, want the completed frame's figures", resp.Usage)
	}
}

// TestDecodeStreamTruncatedIsAnError pins that a stream cut off mid-answer
// fails rather than returning the fragment it managed to read. A truncated
// review that looks like a short one is the failure this whole path exists
// to avoid.
func TestDecodeStreamTruncatedIsAnError(t *testing.T) {
	full := geminitest.Stream([]geminitest.Step{{Type: "model_output", Texts: []string{"half an ans"}}}, "")
	cut := full[:strings.Index(full, "event: interaction.completed")]

	if _, err := decodeStream(strings.NewReader(cut)); err == nil {
		t.Fatal("a stream without an interaction.completed frame must be an error")
	}
}

// TestDecodeStreamIgnoresUnknownFrames pins that the surface may grow
// without breaking this client: an unrecognised event and an unrecognised
// delta type are skipped, and the answer still assembles.
func TestDecodeStreamIgnoresUnknownFrames(t *testing.T) {
	raw := "event: interaction.created\ndata: {\"interaction\":{\"id\":\"v1_test\"},\"event_type\":\"interaction.created\"}\n\n" +
		"event: some.future.event\ndata: {\"whatever\":true}\n\n" +
		"event: step.start\ndata: {\"index\":0,\"step\":{\"type\":\"model_output\"},\"event_type\":\"step.start\"}\n\n" +
		"event: step.delta\ndata: {\"index\":0,\"delta\":{\"type\":\"image\",\"data\":\"...\"},\"event_type\":\"step.delta\"}\n\n" +
		"event: step.delta\ndata: {\"index\":0,\"delta\":{\"text\":\"answer\",\"type\":\"text\"},\"event_type\":\"step.delta\"}\n\n" +
		"event: interaction.completed\ndata: {\"interaction\":{\"status\":\"completed\"},\"event_type\":\"interaction.completed\"}\n\n"

	resp, err := decodeStream(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("decodeStream: %v", err)
	}
	if resp.Text() != "answer" {
		t.Errorf("Text() = %q, want %q", resp.Text(), "answer")
	}
}

// TestDecodeStreamAgainstCaptures runs the decoder over two streams recorded
// verbatim from the live API on 2026-08-14. The fixtures above are a
// reconstruction of the wire format; these are the wire format, and they are
// what catches a frame shape this package guessed at.
func TestDecodeStreamAgainstCaptures(t *testing.T) {
	for _, tc := range []struct {
		file        string
		wantPrefix  string
		wantThought int
		wantSummary bool
	}{
		{"testdata/stream-simple.sse", "The three primary colors", 122, false},
		{"testdata/stream-thought-summary.sse", "To find when and where", 1441, true},
	} {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := decodeStream(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("decodeStream: %v", err)
			}
			if resp.Status != "completed" {
				t.Errorf("status = %q, want completed", resp.Status)
			}
			if !strings.HasPrefix(resp.Text(), tc.wantPrefix) {
				t.Errorf("text = %.60q, want it to start with %q", resp.Text(), tc.wantPrefix)
			}
			if resp.Usage == nil || resp.Usage.TotalThoughtTokens != tc.wantThought {
				t.Errorf("usage = %+v, want total_thought_tokens %d", resp.Usage, tc.wantThought)
			}
			if len(resp.Steps) != 2 || resp.Steps[0].Type != "thought" || resp.Steps[1].Type != "model_output" {
				t.Fatalf("steps = %+v, want a thought step then a model_output step", resp.Steps)
			}
			// The thought step's signature is opaque model state this client
			// does not carry, so the step arrives empty unless the request
			// asked for summaries.
			if got := len(resp.Steps[0].Summary) > 0; got != tc.wantSummary {
				t.Errorf("thought summary present = %v, want %v", got, tc.wantSummary)
			}
			if tc.wantSummary && !strings.Contains(resp.Steps[0].Summary[0].Text, "**Defining the Problem**") {
				t.Errorf("summary = %.80q, want the recorded reasoning summary", resp.Steps[0].Summary[0].Text)
			}
		})
	}
}
