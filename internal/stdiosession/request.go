package stdiosession

import (
	"encoding/json"
	"strings"
)

// CreateRequest is a create body with the vocabulary taken off it: what the
// run is, in the terms the rest of this package and internal/session take it
// in.
//
// The two surfaces this package speaks disagree about where each of these
// lives and what it is called — `instructions` against `system_instruction`,
// `reasoning.effort` against `generation_config.thinking_level`, a top-level
// `max_output_tokens` against a nested one (docs/STDIO-PROTOCOL.md's porting
// table). A decoder reads its own body into this, and the create path below
// it never learns which surface the body came from.
type CreateRequest struct {
	// Model is the name the body asked for, empty for this process's default.
	Model string

	// Prompt is `input` flattened to the one instruction the loop takes.
	// Both surfaces carry a polymorphic input union and both collapse to
	// this: the loop runs the tools, so what a parent sends is a task
	// rather than a conversation to replay.
	Prompt string

	// Instructions is prepended to Prompt rather than replacing the
	// harness's system prompt, which is frozen for a session's life and is
	// the whole of the prompt cache's shared prefix (docs/CACHE.md). The
	// deviation is documented, and doing it here keeps it in one place.
	Instructions string

	// Effort is the thinking control, empty when the body named none.
	Effort string

	// MaxOutputTokens caps one model call's output, reasoning included. It
	// is per request, not per run: a run here spans many.
	MaxOutputTokens int

	// ResultSchema is the schema the Complete tool validates the run's
	// result against (docs/TOOLS.md), already unwrapped from whichever
	// wrapper its surface puts it in. Nil when the body asked for no
	// structured result.
	ResultSchema json.RawMessage

	// PreviousRunID continues a run this process already made. The chain of
	// them is what this protocol has instead of a thread concept.
	PreviousRunID string

	// Tools is the create body's tool array, untranslated: both surfaces
	// declare a `function` and an `mcp_server` with the same keys and the
	// same fields, so this is one type and buildTools, registerServers and
	// checkDeclarations never see a dialect.
	Tools []Tool

	// Stream false holds the JSON-RPC answer until the run ends and returns
	// the whole assembled resource. The notifications are sent either way.
	Stream *bool

	// Harness is this protocol's own extension block, spelled the same in
	// both vocabularies.
	Harness *CreateHarness
}

// AppendRequest is an append body with the vocabulary taken off it.
type AppendRequest struct {
	RunID  string
	Prompt string
	// MessageID is the client's own id for this message, echoed back on the
	// frames that describe it so a parent can match what it sent to what it
	// reads.
	MessageID string
}

// decodeCreate reads a responses.create body into the neutral form.
//
// A field this surface allows and this process cannot honour is refused by
// name rather than ignored: a client that sent `background` and got a
// foreground run would have no way to tell. Which field that is belongs to
// the surface — Google's body carries `agent` and no `background` — which is
// why the refusal lives in the decoder and not in create.
func decodeCreate(params json.RawMessage) (*CreateRequest, *rpcError) {
	var p CreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "responses.create params: %v", err)
	}
	if p.Background != nil && *p.Background {
		return nil, errorf(CodeUnsupported, "background: every response here is already answered at once and streamed as it goes, so there is no foreground to move off")
	}
	prompt, rerr := inputText(p.Input)
	if rerr != nil {
		return nil, rerr
	}
	return &CreateRequest{
		Model:           p.Model,
		Prompt:          prompt,
		Instructions:    p.Instructions,
		Effort:          effortOf(p.Reasoning),
		MaxOutputTokens: p.MaxOutputTokens,
		ResultSchema:    schemaOf(p.Text),
		PreviousRunID:   p.PreviousResponseID,
		Tools:           p.Tools,
		Stream:          p.Stream,
		Harness:         p.Harness,
	}, nil
}

// decodeAppend reads a responses.append body into the neutral form.
func decodeAppend(params json.RawMessage) (*AppendRequest, *rpcError) {
	var p AppendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "responses.append params: %v", err)
	}
	prompt, rerr := inputText(p.Input)
	if rerr != nil {
		return nil, rerr
	}
	req := &AppendRequest{RunID: p.ResponseID, Prompt: prompt}
	if p.Harness != nil {
		req.MessageID = p.Harness.MessageID
	}
	return req, nil
}

// schemaOf is the JSON Schema a create body's text.format named, or nil. It
// is only ever a json_schema format's own schema: `text` and `json_object`
// constrain nothing this harness can hold the Complete tool to.
func schemaOf(t *TextConfig) json.RawMessage {
	if t == nil || t.Format == nil || t.Format.Type != "json_schema" {
		return nil
	}
	return t.Format.Schema
}

// effortOf is the effort a create body asked for, empty when it named none.
func effortOf(r *ReasoningConfig) string {
	if r == nil {
		return ""
	}
	return r.Effort
}

// inputText flattens the surface's polymorphic `input` into the one
// instruction the loop takes. All four forms are read: a bare string, one
// content part, an array of content parts, and an array of input items.
func inputText(raw json.RawMessage) (string, *rpcError) {
	if len(raw) == 0 {
		return "", nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		return "", nil
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", errorf(CodeInvalidParams, "input: %v", err)
		}
		return s, nil
	}
	if trimmed[0] == '{' {
		// One item, or one bare content part. The two are told apart the
		// same way the array branch below tells them apart: by whether the
		// `type` is one of the content-part types.
		return inputElement(raw)
	}
	// An array: the surface allows a list of input items, and a list of
	// content parts is accepted too because a client that already had one
	// should not have to wrap it. They are told apart element by element by
	// the `type` discriminator.
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return "", errorf(CodeInvalidParams, "input: %v", err)
	}
	var parts []string
	for _, el := range raws {
		t, rerr := inputElement(el)
		if rerr != nil {
			return "", rerr
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, "\n\n"), nil
}

// inputElement reads one element of an input array: a content part, or a
// message item whose own content is content parts.
//
// Only `message` items are read. A create body carrying a function_call or a
// function_call_output would be a client trying to replay a conversation
// this process already holds in its own event log, and answering it as if
// the replay were the truth is worse than refusing it: a resumed session
// takes its history from the store, never from the create
// (docs/STDIO-PROTOCOL.md, "Resuming across process restarts").
func inputElement(raw json.RawMessage) (string, *rpcError) {
	var probe struct {
		Type string `json:"type"`
		Role string `json:"role"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", errorf(CodeInvalidParams, "input: %v", err)
	}
	switch probe.Type {
	case PartInputText, PartOutputText, "text", PartInputImage:
		var c ContentPart
		if err := json.Unmarshal(raw, &c); err != nil {
			return "", errorf(CodeInvalidParams, "input: %v", err)
		}
		return contentText([]ContentPart{c})
	case ItemMessage, "":
		// `type` may be omitted on a message item when `role` is present,
		// which is what the surface's own schema says.
		if probe.Type == "" && probe.Role == "" {
			return "", errorf(CodeInvalidParams, "input element has neither a type nor a role")
		}
		var item OutputItem
		if err := json.Unmarshal(raw, &item); err != nil {
			return "", errorf(CodeInvalidParams, "input: %v", err)
		}
		return contentText(item.Content)
	default:
		return "", errorf(CodeUnsupported,
			"input element type %q: only text content and %q items are accepted as input",
			probe.Type, ItemMessage)
	}
}

func contentText(blocks []ContentPart) (string, *rpcError) {
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case PartInputText, PartOutputText, "text":
			parts = append(parts, b.Text)
		case PartInputImage:
			// Not built. An image would have to be materialised into the
			// working directory for the loop to name it in the opening
			// message (internal/attachment), and writing into a directory
			// the client owns is a decision this protocol has not taken.
			return "", errorf(CodeUnsupported, "image input is not implemented: write the file into the working directory and name its path in the text instead")
		default:
			return "", errorf(CodeInvalidParams, "input content type %q", b.Type)
		}
	}
	return strings.Join(parts, "\n"), nil
}
