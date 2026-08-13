package wire_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// partsGoldenRequest builds the request whose serialised bytes are pinned by
// TestRequestBodyPartsGolden: the new multimodal shape from
// third_party/kimi-docs/openapi.json "Message" — content as an array of typed
// parts, legal on any role. Both messages carry a text part and an image_url
// part with a short base64 data URI, on a user message and on a tool-result
// message (which the schema explicitly allows), since a tool result is how a
// future Read of an image reaches the model (docs/KIMI-INTEGRATION.md §4.5).
// The text-only shapes stay pinned by the original golden file.
func partsGoldenRequest(t *testing.T) wire.ChatCompletionRequest {
	t.Helper()
	imagePart := wire.Part{Type: wire.PartTypeImageURL, ImageURL: &wire.ImageURL{URL: "data:image/png;base64,iVBORw0KGgo="}}
	return wire.ChatCompletionRequest{
		Model: "kimi-k3",
		Messages: []wire.Message{
			wire.SystemMessage("You are a helpful assistant."),
			{
				Role: wire.RoleUser,
				Content: wire.Content{Parts: []wire.Part{
					{Type: wire.PartTypeText, Text: "What is in this image?"},
					imagePart,
				}},
			},
			{
				Role:       wire.RoleTool,
				Content:    wire.Content{Parts: []wire.Part{{Type: wire.PartTypeText, Text: "Screenshot captured."}, imagePart}},
				ToolCallID: "call_01",
			},
		},
		MaxTokens: 48000,
	}
}

// TestRequestBodyPartsGolden pins the exact bytes of a request whose messages
// carry parts: the array form, the part field order (type before payload, as
// the vendored schema's examples show), and the tool role carrying parts. It
// is the guard for the new shape — the mirror of TestRequestBodyGolden, which
// keeps pinning the text-only bytes this phase must not change.
func TestRequestBodyPartsGolden(t *testing.T) {
	got, err := json.Marshal(partsGoldenRequest(t))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "request_body_parts.golden.json"))
	if err != nil {
		t.Fatalf("ReadFile golden: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("request body differs from golden file:\ngot:  %s\nwant: %s", got, want)
	}
}
