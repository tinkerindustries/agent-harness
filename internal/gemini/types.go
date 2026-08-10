package gemini

import "strings"

// Content types for the items in an interaction's input array and in a
// response step's content.
const (
	ContentTypeText  = "text"
	ContentTypeImage = "image"
)

// GenerateContentRequest is the request body for POST /v1beta/interactions
// (docs/gemini-3.5-flash-ui-review-prompting.md's sources document this
// surface, so the JSON is snake_case throughout). Field order below is what
// encoding/json emits, so the same value always produces the same bytes.
// There is deliberately no temperature, top_p, or top_k field: the doc is
// explicit that Gemini 3.x must not receive them.
type GenerateContentRequest struct {
	Model             string            `json:"model"`
	SystemInstruction string            `json:"system_instruction,omitempty"`
	Input             []Content         `json:"input"`
	GenerationConfig  *GenerationConfig `json:"generation_config,omitempty"`
}

// Content is one item of an interaction's input: a text part or an image
// part. ContentTypeImage parts carry MIMEType, Data (base64), and an
// optional per-image Resolution; ContentTypeText parts carry Text.
type Content struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	MIMEType   string `json:"mime_type,omitempty"`
	Data       string `json:"data,omitempty"`
	Resolution string `json:"resolution,omitempty"`
}

// GenerationConfig holds the only generation parameter this harness ever
// sets. thinking_level replaces the deprecated thinking_budget; the doc says
// mixing the two is a 400, so thinking_budget has no field here.
type GenerationConfig struct {
	ThinkingLevel string `json:"thinking_level,omitempty"`
}

// GenerateContentResponse is the response body of an interactions call. The
// model's text sits in the steps whose Type is "model_output", in their
// content's text parts; Text() concatenates those.
type GenerateContentResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Steps  []Step `json:"steps"`
}

// Step is one step of an interaction. Only model_output steps carry the
// model's answer; the others (thought, function_call, ...) are skipped by
// Text().
type Step struct {
	Type    string    `json:"type"`
	Content []Content `json:"content,omitempty"`
}

// Text returns the model's output as the concatenation of every text part
// of every model_output step, or "" when there is none.
func (r *GenerateContentResponse) Text() string {
	var parts []string
	for _, s := range r.Steps {
		if s.Type != "model_output" {
			continue
		}
		for _, p := range s.Content {
			if p.Type == ContentTypeText && p.Text != "" {
				parts = append(parts, p.Text)
			}
		}
	}
	return strings.Join(parts, "\n")
}
