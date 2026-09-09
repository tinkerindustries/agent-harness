package gemini

import (
	"encoding/json"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// requestFromIntent turns the session's provider-neutral intent into
// Gemini's agentic request shape: system-role messages become the top-level
// system_instruction, every other message becomes one or more Step values
// in Input, and effort becomes generation_config.thinking_level rather than
// reasoning_effort (docs/GEMINI-INTEGRATION.md §5.3). intent.Thinking is
// deliberately ignored: a thought step always precedes the turn's action on
// this surface, even at thinking_level "low" (docs/OBSERVED.md, "A thought
// step always precedes the turn's action"), so there is no on/off toggle to
// spell — the same reason internal/kimi/intent.go ignores it for K3.
//
// Field order on the returned value follows ChatInteractionRequest's
// declaration order, the byte-stability contract the prompt cache depends
// on (docs/DESIGN.md §3.2): building the request here must not move a
// single byte of the serialised body from one call to the next for an
// unchanged intent.
func requestFromIntent(intent wire.ChatIntent) ChatInteractionRequest {
	var systemInstruction strings.Builder
	var input []any
	// callName tracks the function name a call_id belongs to, from the
	// function_call step that introduced it, so the function_result step
	// answering it can carry the same optional Name (docs/OBSERVED.md's
	// captured round trip has it on both steps). It is not required by the
	// schema; a call_id this map has never seen just answers with no name.
	callName := map[string]string{}

	for _, item := range intent.Items {
		switch item.Type {
		case wire.ItemMessage:
			switch item.Role {
			case wire.RoleSystem:
				if systemInstruction.Len() > 0 {
					systemInstruction.WriteString("\n\n")
				}
				systemInstruction.WriteString(item.Content.String())
			case wire.RoleUser:
				input = append(input, UserInputStep{Type: StepTypeUserInput, Content: contentBlocksFromItem(item.Content)})
			case wire.RoleAssistant:
				input = append(input, ModelOutputStep{Type: StepTypeModelOutput, Content: contentBlocksFromItem(item.Content)})
			}
		case wire.ItemReasoning:
			// A signature is a step of its own, ordered ahead of whatever it
			// produced — exactly the shape the fixtures show: one thought
			// step, then the function_call or model_output step(s) that
			// followed it (docs/OBSERVED.md, "the parallel-call signature
			// rule holds"). A reasoning item carrying no signature is one
			// from a provider that does not mint them, and contributes no
			// step: this surface has nowhere to put bare reasoning text.
			if item.ThoughtSignature != "" {
				input = append(input, ThoughtStep{Type: StepTypeThought, Signature: item.ThoughtSignature})
			}
		case wire.ItemFunctionCall:
			callName[item.CallID] = item.Name
			input = append(input, FunctionCallStep{
				Type:      StepTypeFunctionCall,
				ID:        item.CallID,
				Name:      item.Name,
				Arguments: argumentsToObject(item.Arguments),
			})
		case wire.ItemFunctionCallOutput:
			input = append(input, FunctionResultStep{
				Type:   StepTypeFunctionResult,
				CallID: item.CallID,
				Name:   callName[item.CallID],
				Result: contentBlocksFromItem(item.Output),
			})
		}
	}

	req := ChatInteractionRequest{
		Model:             intent.Model,
		SystemInstruction: systemInstruction.String(),
		Store:             false,
		Input:             input,
		Tools:             toolsFromWire(intent.Tools),
	}
	// generation_config is always sent on this path, because
	// thinking_summaries is always wanted: absent it, the API returns
	// thought steps with a signature and no summary, and a session records
	// its reasoning as empty text with nothing for the UI to show
	// (types.go, GenerationConfig). thinking_level and max_output_tokens
	// keep their own omitempty, so an intent that asks for the API's
	// defaults on both still leaves them out of the object rather than
	// pinning them — what it no longer does is omit the object itself.
	req.GenerationConfig = &GenerationConfig{
		ThinkingLevel:     thinkingLevelFromEffort(intent.Effort),
		MaxOutputTokens:   intent.MaxTokens,
		ThinkingSummaries: ThinkingSummariesAuto,
	}
	return req
}

// thinkingLevelFromEffort maps intent.Effort onto
// generation_config.thinking_level. wire's own effort constants are "low"
// and "high" — the same strings ThinkingLevelLow and ThinkingLevelHigh
// already spell — so both pass through unchanged; "" passes through too,
// which omits generation_config entirely and leaves thinking_level to the
// API's own default. wire.EffortMax ("max") has no counterpart in Gemini's
// four-level enum (minimal/low/medium/high, docs/GEMINI-INTEGRATION.md
// §5.3), so it collapses to the highest level Gemini has. This mapping is a
// guess: no source pins what a Gemini session should ask for when the loop
// requests "max" effort, because routing a real session onto this client is
// Phase 5's job, not this one's. Any other value (including "medium" and
// "minimal", which are meaningful to Gemini but not to wire) passes through
// verbatim, so a caller that already knows the Gemini-native spelling is not
// fought.
func thinkingLevelFromEffort(effort string) string {
	if effort == wire.EffortMax {
		return ThinkingLevelHigh
	}
	return effort
}

// contentBlocksFromWire turns one message's Content into the Content blocks
// a Step carries. Plain text content — every message the harness sends
// today except an image tool result on a vision-capable session
// (docs/KIMI-INTEGRATION.md §4.5) — becomes a single text block; an empty
// text with no parts becomes no blocks at all, so an assistant turn with
// nothing to say (all tool calls, no text) does not send a spurious empty
// text block. A parts array converts each part: text parts become text
// blocks, image_url parts become image blocks by decoding the data URI
// image_url already carries (docs/GEMINI-INTEGRATION.md §5.7 names the
// target shape). seesImages() became true for Gemini in Phase 7
// (internal/session/runner.go), so this path is now reachable for real —
// TestRequestFromIntentFunctionResultImage and its MCP counterpart
// (intent_test.go) exercise it against the shape internal/fold's
// toolResultMessage actually builds, and produce byte-for-byte the
// function_result image shape docs/OBSERVED.md measured against the live
// API: {"type":"image","mime_type":...,"data":...}, no resolution key (§7's
// decision: a Read/MCP result carries one ad hoc image, not the multi-image
// batch the vision tools scrutinise, so there is no "first image" to prefer
// and the field is left unset, the documented API default). A video part
// has no Gemini counterpart in the shapes this harness sends and is dropped
// rather than guessed at.
// contentBlocksFromItem is contentBlocksFromWire for the loop's own item
// content: the same blocks, read out of the Responses part vocabulary, where
// text is typed by direction and an image is a bare data URL rather than an
// object wrapping one.
func contentBlocksFromItem(c *wire.ItemContent) []Content {
	if c == nil {
		return nil
	}
	if len(c.Parts) == 0 {
		if c.Text == "" {
			return nil
		}
		return []Content{{Type: ContentTypeText, Text: c.Text}}
	}
	blocks := make([]Content, 0, len(c.Parts))
	for _, p := range c.Parts {
		switch p.Type {
		case wire.PartInputImage:
			if mime, data, ok := decodeDataURI(p.ImageURL); ok {
				blocks = append(blocks, Content{Type: ContentTypeImage, MIMEType: mime, Data: data})
			}
		default:
			if p.Text != "" {
				blocks = append(blocks, Content{Type: ContentTypeText, Text: p.Text})
			}
		}
	}
	return blocks
}

func contentBlocksFromWire(c wire.Content) []Content {
	if len(c.Parts) == 0 {
		if c.Text == "" {
			return nil
		}
		return []Content{{Type: ContentTypeText, Text: c.Text}}
	}
	blocks := make([]Content, 0, len(c.Parts))
	for _, p := range c.Parts {
		switch p.Type {
		case wire.PartTypeText:
			if p.Text != "" {
				blocks = append(blocks, Content{Type: ContentTypeText, Text: p.Text})
			}
		case wire.PartTypeImageURL:
			if p.ImageURL == nil {
				continue
			}
			if mime, data, ok := decodeDataURI(p.ImageURL.URL); ok {
				blocks = append(blocks, Content{Type: ContentTypeImage, MIMEType: mime, Data: data})
			}
		}
	}
	return blocks
}

// decodeDataURI splits a "data:<mime>;base64,<data>" URL into its mime type
// and base64 payload, which is already the encoding Content.Data wants, so
// the bytes pass through unchanged rather than being decoded and
// re-encoded.
func decodeDataURI(uri string) (mime, data string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", false
	}
	rest := uri[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	header, payload := rest[:comma], rest[comma+1:]
	mimeType, encoding, ok := strings.Cut(header, ";")
	if !ok || encoding != "base64" {
		return "", "", false
	}
	return mimeType, payload, true
}

// toolsFromWire flattens the shared tool schema into Gemini's shape: name,
// description, and parameters directly on the tool rather than nested under
// a function object (docs/GEMINI-INTEGRATION.md §3). Parameters is carried
// through as the same json.RawMessage wire.ToolFunction already holds, so
// the JSON Schema never round-trips through a map that could reorder its
// keys (docs/DESIGN.md §3.2).
func toolsFromWire(tools []wire.Tool) []FunctionTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]FunctionTool, len(tools))
	for i, t := range tools {
		out[i] = FunctionTool{
			Type:        t.Type,
			Name:        t.Function.Name,
			Description: t.Function.Description,
			Parameters:  t.Function.Parameters,
		}
	}
	return out
}

// argumentsToObject turns wire.ToolCallFunc.Arguments — a JSON string, the
// shape OpenAI-format tool calls carry — into the json.RawMessage object
// FunctionCallStep.Arguments wants. It is carried through as raw bytes,
// never parsed into a map[string]any and re-marshalled, so the object's key
// order survives untouched (docs/GEMINI-INTEGRATION.md §3, §5.6). An empty
// string — a call assembled with no arguments frames — becomes "{}" rather
// than an empty json.RawMessage, since FunctionCallStep.Arguments is a
// required field on the wire and an empty byte slice is not valid JSON.
func argumentsToObject(args string) json.RawMessage {
	if args == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}

// argumentsFromObject is argumentsToObject's inverse: it turns a
// FunctionCallStep's Arguments object — captured either from the unary
// response's step or, in the streaming path, from a fully-assembled
// arguments_delta string that already is valid JSON text — into the string
// wire.ToolCallFunc.Arguments wants, again as a direct byte carry with no
// map round trip.
func argumentsFromObject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}
