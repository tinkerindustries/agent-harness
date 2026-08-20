package mcpclient

import (
	"strings"
	"testing"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestFlattenJoinsTextBlocks(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "first"},
			&mcpsdk.TextContent{Text: "second"},
		},
	}
	got := flatten(res)
	want := "first\n\nsecond"
	if got.Text != want {
		t.Fatalf("Text = %q, want %q", got.Text, want)
	}
	if got.IsError {
		t.Fatalf("expected IsError false")
	}
	if len(got.Images) != 0 {
		t.Fatalf("expected no images, got %+v", got.Images)
	}
}

func TestFlattenCollectsImages(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: "here's a picture"},
			&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{1, 2, 3}},
		},
	}
	got := flatten(res)
	if got.Text != "here's a picture" {
		t.Fatalf("Text = %q", got.Text)
	}
	if len(got.Images) != 1 {
		t.Fatalf("expected 1 image, got %+v", got.Images)
	}
	if got.Images[0].MIMEType != "image/png" || string(got.Images[0].Data) != "\x01\x02\x03" {
		t.Fatalf("unexpected image: %+v", got.Images[0])
	}
}

func TestFlattenIsError(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "boom"}},
		IsError: true,
	}
	got := flatten(res)
	if !got.IsError {
		t.Fatalf("expected IsError true")
	}
	if got.Text != "boom" {
		t.Fatalf("Text = %q", got.Text)
	}
}

func TestFlattenUnsupportedContentType(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.AudioContent{MIMEType: "audio/wav", Data: []byte{1}}},
	}
	got := flatten(res)
	if !strings.Contains(got.Text, "unsupported content type") {
		t.Fatalf("Text = %q, want a placeholder naming the unsupported content type", got.Text)
	}
}

func TestFlattenStructuredContentWithNoText(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		StructuredContent: map[string]any{"ok": true},
	}
	got := flatten(res)
	if got.Text == "" || got.Text == "(no content)" {
		t.Fatalf("Text = %q, want the marshalled structured content", got.Text)
	}
	if !strings.Contains(got.Text, `"ok":true`) {
		t.Fatalf("Text = %q, want it to contain the structured content", got.Text)
	}
}

func TestFlattenStructuredContentIgnoredWhenTextPresent(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: "the real answer"}},
		StructuredContent: map[string]any{"ignored": true},
	}
	got := flatten(res)
	if got.Text != "the real answer" {
		t.Fatalf("Text = %q, want the text content to win over structured content", got.Text)
	}
}

func TestFlattenEmptyResultBecomesNoContent(t *testing.T) {
	got := flatten(&mcpsdk.CallToolResult{})
	if got.Text != "(no content)" {
		t.Fatalf("Text = %q, want \"(no content)\"", got.Text)
	}
	if len(got.Images) != 0 {
		t.Fatalf("expected no images, got %+v", got.Images)
	}
}

func TestFlattenImagesOnlyDoesNotBecomeNoContent(t *testing.T) {
	res := &mcpsdk.CallToolResult{
		Content: []mcpsdk.Content{&mcpsdk.ImageContent{MIMEType: "image/png", Data: []byte{1}}},
	}
	got := flatten(res)
	if got.Text == "(no content)" {
		t.Fatalf("an image-only result must not be reported as having no content")
	}
}
