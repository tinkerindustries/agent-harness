package wire

import (
	"encoding/json"
	"reflect"
	"testing"
)

// TestContentTextOnlyRoundTrip checks that a text-only Content marshals as a
// bare JSON string — never as a one-element array — and that unmarshalling
// it gives back a text-only Content. The bare-string form is what every
// message the harness sends today must keep serialising as
// (docs/DESIGN.md §3.2, docs/KIMI-INTEGRATION.md §4.5); the golden
// request-body test pins it at the whole-request level.
func TestContentTextOnlyRoundTrip(t *testing.T) {
	in := TextContent("hello")
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(b) == 0 || b[0] != '"' {
		t.Fatalf("text-only content marshalled as %s, want a bare JSON string", b)
	}

	var out Content
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := out.String(); got != "hello" {
		t.Errorf("round-tripped text = %q, want %q", got, "hello")
	}
	if len(out.Parts) != 0 {
		t.Errorf("text-only content round-tripped into %d parts, want none: a bare string must never become a one-element array", len(out.Parts))
	}
}

// TestContentPartsRoundTrip checks that a Content carrying parts marshals as
// the parts array and unmarshals back to the same parts.
func TestContentPartsRoundTrip(t *testing.T) {
	in := Content{Parts: []Part{
		{Type: PartTypeText, Text: "What is in this image?"},
		{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: "data:image/png;base64,iVBORw0KGgo="}},
	}}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if len(b) == 0 || b[0] != '[' {
		t.Fatalf("parts content marshalled as %s, want a JSON array", b)
	}

	var out Content
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(out.Parts, in.Parts) {
		t.Errorf("round-tripped parts = %+v, want %+v", out.Parts, in.Parts)
	}
}

// TestContentZeroValueIsEmptyString pins the zero value's serialisation: an
// assistant message carrying tool_calls must serialise content as "" rather
// than null or an empty array (docs/DESIGN.md §4.4).
func TestContentZeroValueIsEmptyString(t *testing.T) {
	b, err := json.Marshal(Content{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(b) != `""` {
		t.Fatalf("zero Content marshalled as %s, want \"\"", b)
	}
}

// TestContentRoundTripThroughMessage checks the field-level round trip: a
// Message's content stays a bare string on the wire, and a parts content
// comes back as parts after a full request marshal/unmarshal cycle.
func TestContentRoundTripThroughMessage(t *testing.T) {
	text := UserMessage("hi")
	b, err := json.Marshal(text)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Message
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := back.Content.String(); got != "hi" {
		t.Errorf("round-tripped message text = %q, want %q", got, "hi")
	}
	if len(back.Content.Parts) != 0 {
		t.Errorf("text-only message round-tripped into %d parts, want none", len(back.Content.Parts))
	}
}
