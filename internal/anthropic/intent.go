package anthropic

import (
	"encoding/json"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// systemMessageModels is which of the three models this client hosts
// accepts a mid-conversation `{"role":"system"}` message inside `messages`
// — Opus 5.5 and Fable 5.1, not Sonnet 5
// (docs/ANTHROPIC-INTEGRATION.md, "Request", citing
// <https://platform.claude.com/docs/en/build-with-claude/prompt-caching>,
// "Mid-Conversation System Messages"). A model this map does not name
// renders the item as a user text block instead, the same fallback Sonnet 5
// gets.
var systemMessageModels = map[string]bool{
	ModelOpus55:  true,
	ModelFable51: true,
}

// builtMessage is one message under construction: either blocks this client
// is assembling itself, or raw bytes captured from a prior Anthropic
// response and replayed verbatim (Message.Content's own doc comment).
// Splitting the two lets cache breakpoints be placed on the blocks form
// (applyCacheBreakpoints) without ever decoding the raw form back out of
// its captured bytes.
type builtMessage struct {
	role   string
	blocks []ContentBlock
	raw    json.RawMessage
}

func (m builtMessage) toMessage() Message {
	if len(m.raw) > 0 {
		return Message{Role: m.role, Content: m.raw}
	}
	return Message{Role: m.role, Content: blocksJSON(m.blocks)}
}

// blocksJSON marshals blocks into the request body's content array bytes.
// blocks is a concrete []ContentBlock, never map[string]any, so this is
// deterministic key order, not a round trip that could reorder one
// (docs/DESIGN.md §3.2).
func blocksJSON(blocks []ContentBlock) json.RawMessage {
	if blocks == nil {
		blocks = []ContentBlock{}
	}
	b, err := json.Marshal(blocks)
	if err != nil {
		// blocks is built entirely from this package's own struct literals;
		// nothing in it can fail to marshal.
		return json.RawMessage("[]")
	}
	return b
}

// requestFromIntent turns the loop's provider-neutral intent into a
// Messages API request body (docs/ANTHROPIC-INTEGRATION.md, "Request").
// Field order on the returned value follows MessagesRequest's declaration
// order, the byte-stability contract the prompt cache depends on
// (docs/DESIGN.md §3.2): building the request here must not move a single
// byte of the serialised body from one call to the next for an unchanged
// intent.
func requestFromIntent(intent wire.ChatIntent) MessagesRequest {
	systemPrompt, items := wire.SystemPromptOf(intent.Items)

	req := MessagesRequest{
		Model:     intent.Model,
		MaxTokens: intent.MaxTokens,
		Thinking: &ThinkingConfig{
			Type:    "adaptive",
			Display: "summarized",
			BlockBinding: &BlockBinding{
				PrefixMismatchBehavior: "error",
			},
		},
		Tools: toolsFromWire(intent.Tools),
	}
	if intent.Effort != "" {
		req.OutputConfig = &OutputConfig{Effort: intent.Effort}
	}
	if systemPrompt != "" {
		req.System = []ContentBlock{{Type: "text", Text: systemPrompt}}
	}

	messages := messagesFromItems(items, intent.Model)
	applyCacheBreakpoints(req.System, req.Tools, messages)
	req.Messages = make([]Message, len(messages))
	for i, m := range messages {
		req.Messages[i] = m.toMessage()
	}
	return req
}

// messagesFromItems renders the conversation items (everything after the
// leading system item) into builtMessage values. A sub-turn's assistant
// turn is one message: raw provider blocks replayed verbatim when the
// sub-turn carries them (every sub-turn this client itself produced does),
// or a best-effort reconstruction from text/thinking/tool_use otherwise —
// see assistantBlocksFallback. All of one sub-turn's tool results become
// one user message carrying one tool_result block per call, matching what
// the API expects for a turn's outputs.
func messagesFromItems(items []wire.Item, model string) []builtMessage {
	var out []builtMessage

	var assistant *builtMessage
	// skipRawExtra is true while the assistant message currently open was
	// opened from a reasoning item's raw ProviderBlocks: those already
	// carry the sub-turn's text and tool_use blocks verbatim, so the
	// ItemMessage(assistant) and ItemFunctionCall items fold.go still emits
	// for the same sub-turn must be swallowed rather than appended again.
	skipRawExtra := false
	var toolResults *builtMessage

	flushAssistant := func() {
		if assistant != nil {
			out = append(out, *assistant)
			assistant = nil
		}
		skipRawExtra = false
	}
	flushToolResults := func() {
		if toolResults != nil {
			out = append(out, *toolResults)
			toolResults = nil
		}
	}

	for _, item := range items {
		switch item.Type {
		case wire.ItemReasoning:
			flushToolResults()
			flushAssistant()
			assistant = &builtMessage{role: wire.RoleAssistant}
			if len(item.ProviderBlocks) > 0 {
				assistant.raw = item.ProviderBlocks
				skipRawExtra = true
			} else if text := item.Content.String(); text != "" {
				assistant.blocks = append(assistant.blocks, ContentBlock{
					Type: "thinking", Thinking: text, Signature: item.ThoughtSignature,
				})
			}

		case wire.ItemMessage:
			switch item.Role {
			case wire.RoleAssistant:
				flushToolResults()
				if skipRawExtra {
					continue
				}
				if assistant == nil {
					assistant = &builtMessage{role: wire.RoleAssistant}
				}
				if text := item.Content.String(); text != "" {
					assistant.blocks = append(assistant.blocks, ContentBlock{Type: "text", Text: text})
				}
			case wire.RoleUser:
				flushAssistant()
				flushToolResults()
				out = append(out, builtMessage{role: wire.RoleUser, blocks: contentBlocksFromItem(item.Content)})
			case wire.RoleSystem:
				flushAssistant()
				flushToolResults()
				out = append(out, systemMessageFor(model, item.Content.String()))
			}

		case wire.ItemFunctionCall:
			flushToolResults()
			if skipRawExtra {
				continue
			}
			if assistant == nil {
				assistant = &builtMessage{role: wire.RoleAssistant}
			}
			assistant.blocks = append(assistant.blocks, ContentBlock{
				Type: "tool_use", ID: item.CallID, Name: item.Name, Input: argumentsToObject(item.Arguments),
			})

		case wire.ItemFunctionCallOutput:
			flushAssistant()
			if toolResults == nil {
				toolResults = &builtMessage{role: wire.RoleUser}
			}
			toolResults.blocks = append(toolResults.blocks, ContentBlock{
				Type: "tool_result", ToolUseID: item.CallID, Content: contentBlocksFromItem(item.Output),
			})
		}
	}
	flushAssistant()
	flushToolResults()
	return out
}

// systemMessageFor renders a mid-conversation system-role item — a steer or
// a reminder the loop injected with wire.RoleSystem
// (internal/session/reminders.go) — as a system-role message on the models
// that accept one, and as a user text block on Sonnet 5
// (systemMessageModels' own doc comment).
func systemMessageFor(model, text string) builtMessage {
	if systemMessageModels[model] {
		return builtMessage{role: wire.RoleSystem, blocks: []ContentBlock{{Type: "text", Text: text}}}
	}
	return builtMessage{role: wire.RoleUser, blocks: []ContentBlock{{Type: "text", Text: text}}}
}

// contentBlocksFromItem turns one item's content into the content blocks a
// user message or a tool_result block carries: a single text block for
// plain text, and one block per part — text or image, decoded from the
// data URI wire.PartInputImage already carries — for a multimodal content
// (docs/KIMI-INTEGRATION.md §4.5's shape, read through the Responses
// vocabulary). Empty content becomes no blocks at all, matching
// internal/gemini's contentBlocksFromItem.
func contentBlocksFromItem(c *wire.ItemContent) []ContentBlock {
	if c == nil {
		return nil
	}
	if len(c.Parts) == 0 {
		if c.Text == "" {
			return nil
		}
		return []ContentBlock{{Type: "text", Text: c.Text}}
	}
	blocks := make([]ContentBlock, 0, len(c.Parts))
	for _, p := range c.Parts {
		switch p.Type {
		case wire.PartInputImage:
			if mime, data, ok := decodeDataURI(p.ImageURL); ok {
				blocks = append(blocks, ContentBlock{Type: "image", Source: &ImageSource{Type: "base64", MediaType: mime, Data: data}})
			}
		default:
			if p.Text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: p.Text})
			}
		}
	}
	return blocks
}

// decodeDataURI splits a "data:<mime>;base64,<data>" URL into its mime type
// and base64 payload, which is already the encoding ImageSource.Data wants.
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

// toolsFromWire renders the frozen tool array as input_schema tools, then
// appends the server tools in a fixed order (docs/ANTHROPIC-INTEGRATION.md,
// "Tools"). Parameters is carried through as the same json.RawMessage
// wire.ToolFunction already holds, so a tool's JSON Schema never
// round-trips through a map that could reorder its keys.
func toolsFromWire(tools []wire.Tool) []any {
	out := make([]any, 0, len(tools)+2)
	for _, t := range tools {
		out = append(out, ToolDefinition{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	out = append(out,
		ServerToolDefinition{Type: ToolTypeWebSearch, Name: "web_search"},
		ServerToolDefinition{Type: ToolTypeWebFetch, Name: "web_fetch"},
	)
	return out
}

// argumentsToObject turns wire.ToolCallFunc.Arguments — a JSON string, the
// shape OpenAI-format tool calls carry — into the json.RawMessage object
// ContentBlock.Input wants, carried through as raw bytes so its key order
// survives untouched (docs/GEMINI-INTEGRATION.md §3's identical reasoning
// for Gemini's own arguments object). An empty string becomes "{}" rather
// than an empty json.RawMessage, since Input is a required object on the
// wire.
func argumentsToObject(args string) json.RawMessage {
	if args == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}

// argumentsFromObject is argumentsToObject's inverse, used when a response
// block's already-assembled input must become the string
// wire.ToolCallFunc.Arguments wants.
func argumentsFromObject(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// applyCacheBreakpoints places up to three of the four available cache
// breakpoints (docs/ANTHROPIC-INTEGRATION.md, "Caching"): the last tool in
// the array, the system block, and the last content block of the last
// message — provided that message is one this client built itself rather
// than a raw-replayed assistant turn, which items always end at (a user
// message, a mid-conversation system item, or a sub-turn's tool results;
// fold.go never leaves an unanswered assistant turn as the last item of a
// request's items). Skipping the raw case rather than decoding it back out
// keeps Message.Content's byte-for-byte replay promise intact; it costs one
// possible breakpoint on the rare request that ends there instead.
func applyCacheBreakpoints(system []ContentBlock, tools []any, messages []builtMessage) {
	breakpoint := &CacheControl{Type: "ephemeral"}

	if n := len(tools); n > 0 {
		if t, ok := tools[n-1].(ToolDefinition); ok {
			t.CacheControl = breakpoint
			tools[n-1] = t
		} else if t, ok := tools[n-1].(ServerToolDefinition); ok {
			// Server tools carry no cache_control field on the wire; the
			// breakpoint belongs on the last client-declared tool instead.
			_ = t
			for i := n - 1; i >= 0; i-- {
				if td, ok := tools[i].(ToolDefinition); ok {
					td.CacheControl = breakpoint
					tools[i] = td
					break
				}
			}
		}
	}
	if len(system) > 0 {
		system[len(system)-1].CacheControl = breakpoint
	}
	if len(messages) > 0 {
		last := &messages[len(messages)-1]
		if len(last.raw) == 0 && len(last.blocks) > 0 {
			last.blocks[len(last.blocks)-1].CacheControl = breakpoint
		}
	}
}
