package wire

import (
	"encoding/json"
	"strings"
)

// Item is the loop's canonical conversation vocabulary: one element of a
// Responses API `input` list.
//
// It replaced Message in that role. The loop states its intent as items
// (ChatIntent.Items), internal/fold builds them from the event log, and each
// provider renders them into its own request:
//
//   - internal/deepseek's Responses path serialises them **unchanged** —
//     these types are that surface's own, so there is no translation on the
//     path this repository's stdio entry point runs (docs/DEEPSEEK-RESPONSES.md).
//   - internal/deepseek's Chat Completions path and internal/kimi render them
//     back into a messages array with MessagesFromItems, which produces the
//     bytes those surfaces have always been sent.
//   - internal/gemini renders them into Interactions steps, which it already
//     did from messages and does more directly from these.
//
// Field order is the byte-stability contract the prompt cache depends on
// (docs/DESIGN.md §3.2): this type is serialised straight into a request
// body, so moving a field moves the cache prefix of every session on that
// path.
type Item struct {
	Type      string       `json:"type"`
	Role      string       `json:"role,omitempty"`
	Content   *ItemContent `json:"content,omitempty"`
	CallID    string       `json:"call_id,omitempty"`
	Name      string       `json:"name,omitempty"`
	Arguments string       `json:"arguments,omitempty"`
	Output    *ItemContent `json:"output,omitempty"`

	// ThoughtSignature is Gemini's opaque receipt for a thought step, which
	// must be replayed verbatim on every later request of a session
	// (docs/GEMINI-INTEGRATION.md §5.2). The Responses vocabulary has no
	// field for it — its own `reasoning` item carries `encrypted_content`,
	// which DeepSeek does not support — so it rides here and is **never
	// serialised**: `json:"-"` keeps it out of a request body that is
	// otherwise this struct verbatim. internal/gemini reads it off the Go
	// value; the other providers ignore it.
	ThoughtSignature string `json:"-"`
}

// Item types, from the Responses input-item union. These four are what the
// fold produces; the surface defines three more (custom_tool_call,
// custom_tool_call_output, web_search_call) that nothing here builds.
const (
	ItemMessage            = "message"
	ItemReasoning          = "reasoning"
	ItemFunctionCall       = "function_call"
	ItemFunctionCallOutput = "function_call_output"
)

// Content part types, from the Responses content-part union. The surface
// splits text by direction: what the model reads is input_text, what it
// wrote is output_text, and its chain-of-thought is reasoning_text.
const (
	PartInputText     = "input_text"
	PartOutputText    = "output_text"
	PartReasoningText = "reasoning_text"
	PartInputImage    = "input_image"
)

// ItemContent is the surface's oneOf[string, array of parts], marshalled the
// way Content is for the other dialect: a bare string when there are no
// parts, the array when there are. Text-only content is by far the common
// case and stays one string on the wire.
type ItemContent struct {
	Text  string
	Parts []ItemPart
}

// MarshalJSON emits the string form when Parts is empty and the array form
// otherwise.
func (c ItemContent) MarshalJSON() ([]byte, error) {
	if len(c.Parts) == 0 {
		return json.Marshal(c.Text)
	}
	return json.Marshal(c.Parts)
}

// UnmarshalJSON accepts either form, so an item can be round-tripped.
func (c *ItemContent) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		return json.Unmarshal(b, &c.Text)
	}
	return json.Unmarshal(b, &c.Parts)
}

// String returns the content as plain text: the string form as it stands,
// and the concatenation of the text parts otherwise. Callers that can only
// handle text (compaction summaries, the churn diagnostic) read it through
// this, and an image part contributes nothing rather than a placeholder.
func (c *ItemContent) String() string {
	if c == nil {
		return ""
	}
	if len(c.Parts) == 0 {
		return c.Text
	}
	var out strings.Builder
	for _, p := range c.Parts {
		out.WriteString(p.Text)
	}
	return out.String()
}

// ItemPart is one part of an ItemContent. ImageURL is a bare string carrying
// a base64 data URI, which is the Responses envelope; the Chat Completions
// one wraps the same string in an object with its own `url` field, and
// MessagesFromItems does that wrapping.
type ItemPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// TextItemContent builds text-only content, the shape all but the vision
// path uses.
func TextItemContent(text string) *ItemContent { return &ItemContent{Text: text} }

// SystemItem builds the frozen system prompt's item. It is a message item
// with the system role: the Responses request turns it into `instructions`
// and a Chat Completions one into the first system message, so the model
// sees the same thing either way.
func SystemItem(text string) Item {
	return Item{Type: ItemMessage, Role: RoleSystem, Content: TextItemContent(text)}
}

// UserItem builds a user message item.
func UserItem(text string) Item {
	return Item{Type: ItemMessage, Role: RoleUser, Content: TextItemContent(text)}
}

// AssistantItem builds an assistant message item.
func AssistantItem(text string) Item {
	return Item{Type: ItemMessage, Role: RoleAssistant, Content: &ItemContent{
		Parts: []ItemPart{{Type: PartOutputText, Text: text}},
	}}
}

// ReasoningItem builds a reasoning item carrying one chain-of-thought text.
func ReasoningItem(text string) Item {
	item := Item{Type: ItemReasoning}
	if text != "" {
		item.Content = &ItemContent{Parts: []ItemPart{{Type: PartReasoningText, Text: text}}}
	}
	return item
}

// FunctionCallItem builds a function_call item.
func FunctionCallItem(callID, name, arguments string) Item {
	return Item{Type: ItemFunctionCall, CallID: callID, Name: name, Arguments: arguments}
}

// FunctionCallOutputItem builds a function_call_output item from a tool's
// text and, when the tool produced one, its image.
//
// The image is an input_image part in the item's own output, which is the
// shape the Responses API documents for a tool that produced a picture —
// unlike Chat Completions, where the harness sends an image inside a tool
// message on measured behaviour the vendor's schema contradicts
// (docs/DEEPSEEK-VISION.md §2).
func FunctionCallOutputItem(callID, text, imageURL string) Item {
	item := Item{Type: ItemFunctionCallOutput, CallID: callID}
	if imageURL == "" {
		item.Output = TextItemContent(text)
		return item
	}
	parts := make([]ItemPart, 0, 2)
	if text != "" {
		parts = append(parts, ItemPart{Type: PartInputText, Text: text})
	}
	parts = append(parts, ItemPart{Type: PartInputImage, ImageURL: imageURL})
	item.Output = &ItemContent{Parts: parts}
	return item
}
