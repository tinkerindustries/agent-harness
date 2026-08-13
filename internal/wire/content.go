package wire

import "encoding/json"

// Part types carried by a multimodal content array. Kimi K3 reads image and
// video parts; DeepSeek accepts text only
// (third_party/kimi-docs/openapi.json "Message",
// guide/use-kimi-vision-model.md, docs/KIMI-INTEGRATION.md §4.5).
const (
	PartTypeText     = "text"
	PartTypeImageURL = "image_url"
	PartTypeVideoURL = "video_url"
)

// Content is a message's content: a plain text string, or — for multimodal
// input — an array of typed parts. The API's schema is oneOf[string, array],
// and the array form is legal on any role, tool included
// (third_party/kimi-docs/openapi.json "Message").
//
// MarshalJSON emits a bare JSON string whenever Parts is empty — exactly the
// bytes a string content has always produced, so every message the harness
// sends today serialises unchanged (docs/DESIGN.md §3.2) — and the parts
// array when Parts is non-empty, in which case Text is not sent. The zero
// value marshals as "", which is what an assistant message carrying
// tool_calls must serialise as (docs/DESIGN.md §4.4). UnmarshalJSON accepts
// both forms.
type Content struct {
	Text  string
	Parts []Part
}

// TextContent builds text-only content, the shape every message the harness
// produces today uses. The fold, the session loop, and the tool results all
// construct content through this (docs/KIMI-INTEGRATION.md §4.5).
func TextContent(text string) Content { return Content{Text: text} }

// String returns the content as plain text. For text-only content this is
// the content itself; for a parts content it is the content's own Text,
// which callers that can only handle text (compaction summaries, the eval
// judge) read instead of the parts. No message the harness builds today
// carries parts, so this never drops anything.
func (c Content) String() string { return c.Text }

// MarshalJSON implements the oneOf[string, array] shape: a bare JSON string
// with no parts, the array with.
func (c Content) MarshalJSON() ([]byte, error) {
	if len(c.Parts) == 0 {
		return json.Marshal(c.Text)
	}
	return json.Marshal(c.Parts)
}

// UnmarshalJSON accepts both forms of the schema.
func (c *Content) UnmarshalJSON(b []byte) error {
	if len(b) == 0 {
		return nil
	}
	if b[0] == '"' {
		return json.Unmarshal(b, &c.Text)
	}
	return json.Unmarshal(b, &c.Parts)
}

// Part is one element of a multimodal content array. Field order below is
// what encoding/json emits and matches the schema's example shape — type
// first, then the part's own payload — so a parts array serialises
// byte-stably like every other request field (third_party/kimi-docs/openapi.json
// "Message", guide/use-kimi-vision-model.md). Exactly one payload field is
// set, the one named by Type.
type Part struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
	VideoURL *VideoURL `json:"video_url,omitempty"`
}

// ImageURL is the payload of an image_url part. URL is a base64 data URI
// (data:image/<fmt>;base64,...) or an uploaded file id; plain URLs and SVG
// are rejected by the API
// (third_party/kimi-docs/guide/use-kimi-vision-model.md).
type ImageURL struct {
	URL string `json:"url"`
}

// VideoURL is the payload of a video_url part. URL references an uploaded
// file by id (ms://<file-id>)
// (third_party/kimi-docs/guide/use-kimi-vision-model.md).
type VideoURL struct {
	URL string `json:"url"`
}
