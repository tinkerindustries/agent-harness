package wire

import "strings"

// MessagesFromItems renders the loop's canonical items back into a Chat
// Completions messages array.
//
// It exists so that moving the loop's own vocabulary to the Responses shape
// cost the older dialect nothing: internal/deepseek's Chat Completions path
// and internal/kimi call this and send exactly the bytes they always have.
// `harness serve` runs on that path with sessions that predate the change,
// and the head of every request is the frozen prefix its prompt cache is
// built on (docs/DESIGN.md §3.2), so "exactly" is the requirement, not a
// nicety. internal/fold's own test folds a log both ways and compares.
//
// The one piece of real work is that the two vocabularies disagree about how
// much a turn is:
//
//   - Items say it in pieces. A sub-turn is a `reasoning` item, then an
//     assistant `message` item when the model wrote anything, then one
//     `function_call` item per call.
//   - A messages array says it in one. That same sub-turn is a single
//     assistant message carrying `reasoning_content`, `content`, and a
//     `tool_calls` array.
//
// So a contiguous run of assistant-side items collapses into one message,
// flushed when an item arrives that cannot belong to it — a user or system
// message, a tool result, or the end of the list. That mirrors the fold's
// own flushAssistant, which is where this shape came from.
func MessagesFromItems(items []Item) []Message {
	msgs := make([]Message, 0, len(items))

	var reasoning, content strings.Builder
	var toolCalls []ToolCall
	var signature string
	// open says an assistant message is being accumulated. It is not
	// len-derived: a sub-turn that produced no text and no reasoning, only
	// calls, still emits one assistant message with an empty content — and
	// so does one that produced nothing at all but was opened by a
	// reasoning item, which is what keeps this identical to the fold that
	// preceded it.
	open := false

	flush := func() {
		if !open {
			return
		}
		msg := Message{
			Role:      RoleAssistant,
			Content:   TextContent(content.String()),
			ToolCalls: toolCalls,
		}
		if reasoning.Len() > 0 {
			r := reasoning.String()
			msg.ReasoningContent = &r
		}
		if signature != "" {
			s := signature
			msg.ThoughtSignature = &s
		}
		msgs = append(msgs, msg)
		reasoning.Reset()
		content.Reset()
		toolCalls = nil
		signature = ""
		open = false
	}

	for _, item := range items {
		switch item.Type {
		case ItemReasoning:
			open = true
			reasoning.WriteString(item.Content.String())
			if item.ThoughtSignature != "" {
				signature = item.ThoughtSignature
			}

		case ItemFunctionCall:
			open = true
			toolCalls = append(toolCalls, ToolCall{
				ID:       item.CallID,
				Type:     "function",
				Function: ToolCallFunc{Name: item.Name, Arguments: item.Arguments},
			})

		case ItemFunctionCallOutput:
			flush()
			msgs = append(msgs, Message{
				Role:       RoleTool,
				Content:    contentFromItemContent(item.Output),
				ToolCallID: item.CallID,
			})

		case ItemMessage:
			switch item.Role {
			case RoleAssistant:
				open = true
				content.WriteString(item.Content.String())
			default:
				flush()
				msgs = append(msgs, Message{
					Role:    item.Role,
					Content: contentFromItemContent(item.Content),
				})
			}
		}
	}
	flush()
	return msgs
}

// contentFromItemContent turns an item's content into a message's, moving
// each part back into the older dialect's envelope: text parts lose their
// direction, and an image's bare URL is wrapped in the object Chat
// Completions expects. Text-only content stays a bare string, which is what
// all but the vision path sends.
func contentFromItemContent(c *ItemContent) Content {
	if c == nil {
		return TextContent("")
	}
	if len(c.Parts) == 0 {
		return TextContent(c.Text)
	}
	parts := make([]Part, 0, len(c.Parts))
	for _, p := range c.Parts {
		if p.Type == PartInputImage {
			parts = append(parts, Part{Type: PartTypeImageURL, ImageURL: &ImageURL{URL: p.ImageURL}})
			continue
		}
		parts = append(parts, Part{Type: PartTypeText, Text: p.Text})
	}
	return Content{Parts: parts}
}

// SystemPromptOf returns the text of the leading system item and the items
// after it, for a surface that carries the system prompt outside the
// conversation — the Responses API's `instructions`, Gemini's
// `system_instruction`. An item list that does not begin with one comes back
// unchanged with an empty prompt.
func SystemPromptOf(items []Item) (string, []Item) {
	if len(items) == 0 || items[0].Type != ItemMessage || items[0].Role != RoleSystem {
		return "", items
	}
	return items[0].Content.String(), items[1:]
}
