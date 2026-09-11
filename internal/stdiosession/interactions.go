package stdiosession

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// Google's Interactions API vocabulary, which `harness gemini-session`
// speaks. Its REST methods on POST /v1beta/interactions are the JSON-RPC
// methods and its streaming SSE event names are the notifications, so a
// client written against <https://ai.google.dev/api/interactions> reads this
// without a translation table (docs/STDIO-INTERACTIONS.md).
//
// It is the vocabulary internal/gemini already speaks to Google underneath,
// which is why a Gemini session can be driven in it end to end. That is a
// convenience rather than a passthrough: the loop runs the tools here, so an
// interaction on this surface is a whole agentic run where Google's own is
// one model call, and the sub-turn number on every step is what says so.

// Method names the client may call. The interactions.* four are Google's own
// REST methods on `/v1beta/interactions`, spelled as JSON-RPC methods.
// interactions.append is this protocol's own — Google has no verb for adding
// input to an interaction that is already running, because on Google's
// surface an interaction is one model call and there is no window in which to
// add anything. It is what steering is (docs/RUN-CONTROL.md).
const (
	MethodInteractionsCreate = "interactions.create"
	MethodInteractionsAppend = "interactions.append"
	MethodInteractionsCancel = "interactions.cancel"
	MethodInteractionsGet    = "interactions.get"
	MethodInteractionsDelete = "interactions.delete"
)

// Interactions implements Dialect.
type Interactions struct{}

// NewInteractions returns the Interactions dialect. It holds no state.
func NewInteractions() Interactions { return Interactions{} }

func (Interactions) Protocol() string { return "google.interactions.v1beta" }

func (Interactions) Methods() MethodSet {
	return MethodSet{
		Create: MethodInteractionsCreate,
		Append: MethodInteractionsAppend,
		Cancel: MethodInteractionsCancel,
		Get:    MethodInteractionsGet,
		Delete: MethodInteractionsDelete,
	}
}

// NewRunID mints the id an interaction is addressed by. It is not the
// session id: a chain of interactions linked by previous_interaction_id is
// one session resumed repeatedly, so the two cannot be the same value.
// harness.session_id on every interaction carries the session's own.
func (Interactions) NewRunID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("stdiosession: crypto/rand unavailable: " + err.Error())
	}
	return "int_" + hex.EncodeToString(b[:])
}

func (Interactions) Messages() Messages {
	return Messages{
		AlreadyRunning:       "interaction %s is still running; one process hosts one session, so cancel it or wait for it to complete",
		PreviousNotFound:     "no interaction %q was minted by this process; an interaction id does not outlive the process that made it, so continue across a restart with harness.resume_session_id instead",
		PreviousChangedCWD:   "interaction %s works in %s; a continued interaction cannot change directory, and inherits the one its chain started in",
		PreviousChangedModel: "interaction %s ran on %s; a continued interaction cannot change model, because the session's prefix is frozen",
		ContextEnded:         "interaction %s: %v",
		NotFound:             "no interaction %q",
		AppendNotRunning:     "interaction %s is %s; start a new interaction with previous_interaction_id set to it instead",
		AppendNeedsInput:     "interactions.append needs some input",
		DeleteStillRunning:   "interaction %s is still running; cancel it first",
		EffortRefused:        "generation_config.thinking_level %q: %s accepts %s",
		SchemaFrozen:         "session %s carries its own result schema; a resumed session keeps the one it was started with, and response_format cannot replace it",
		BothContinuations:    "previous_interaction_id and harness.resume_session_id both name a conversation to continue; send one. previous_interaction_id continues an interaction this process ran, harness.resume_session_id continues a session out of the state directory",
	}
}

// iactCreateParams is Google's create-interaction request body, narrowed to
// what this surface honours, plus the `harness` block. Anything Google's
// schema allows and this does not is rejected by name rather than ignored: a
// client that sent `agent` and got a model run would have no way to tell.
type iactCreateParams struct {
	Model string `json:"model,omitempty"`
	// Agent is accepted only so it can be refused with a message that says
	// why. Google's managed agents run on Google's machines; this process
	// runs a loop on the parent's.
	Agent string `json:"agent,omitempty"`
	// Input is Google's polymorphic input: a bare string, one Content, an
	// array of Content, or an array of Step. All four are accepted.
	Input json.RawMessage `json:"input"`
	// SystemInstruction is prepended to the interaction's first user input
	// rather than replacing this harness's system prompt, which is frozen
	// for a session's life and is the whole of the prompt cache's shared
	// prefix (docs/CACHE.md).
	SystemInstruction     string                `json:"system_instruction,omitempty"`
	Tools                 []Tool                `json:"tools,omitempty"`
	PreviousInteractionID string                `json:"previous_interaction_id,omitempty"`
	ResponseFormat        *iactResponseFormat   `json:"response_format,omitempty"`
	GenerationConfig      *iactGenerationConfig `json:"generation_config,omitempty"`
	Stream                *bool                 `json:"stream,omitempty"`
	// Store is accepted and ignored. This process always stores: the loop's
	// own state machine is its event log, and a run that kept nothing could
	// not be resumed or even folded (internal/session).
	Store   *bool          `json:"store,omitempty"`
	Harness *CreateHarness `json:"harness,omitempty"`
}

// iactResponseFormat is Google's text response format: a JSON schema the
// answer must satisfy. It becomes the session's result schema, which is what
// the Complete tool validates against (docs/TOOLS.md).
type iactResponseFormat struct {
	Type     string          `json:"type"`
	MIMEType string          `json:"mime_type,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
}

// iactGenerationConfig is Google's generation config, narrowed to the two
// fields that mean something to an agent loop.
type iactGenerationConfig struct {
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	ThinkingLevel   string `json:"thinking_level,omitempty"`
}

// iactAppendParams adds input to an interaction that is already running. It
// is the same `input` union a create takes, so the client builds it the same
// way.
type iactAppendParams struct {
	InteractionID string          `json:"interaction_id"`
	Input         json.RawMessage `json:"input"`
	Harness       *AppendHarness  `json:"harness,omitempty"`
}

// iactIDParams is the body of cancel, get and delete: Google's path
// parameter, which a JSON-RPC call has to carry in the params instead.
type iactIDParams struct {
	InteractionID string `json:"interaction_id"`
}

// iactCreateResult is what interactions.create answers with. On a streaming
// create it is the interaction as at interaction.created — id, model,
// in_progress — and the steps follow as notifications. On stream:false it is
// the whole finished interaction. GET and cancel answer with the same shape,
// as Google's own do.
type iactCreateResult struct {
	Interaction iactInteraction `json:"interaction"`
}

// iactAppendResult names the interaction the input was accepted for and
// where it landed in that session's log.
type iactAppendResult struct {
	InteractionID string `json:"interaction_id"`
	Seq           int64  `json:"seq"`
}

// iactInitializeResult is this process's own half of the handshake.
type iactInitializeResult struct {
	ServerInfo   ServerInfo             `json:"server_info"`
	Capabilities iactServerCapabilities `json:"capabilities"`
	Models       []string               `json:"models"`
	DefaultModel string                 `json:"default_model"`
	ModelDetails []iactModelDetail      `json:"model_details,omitempty"`
}

// iactServerCapabilities is what this process implements. It differs from
// the Responses one in a single key: the continuation capability is named
// for the id it enables.
type iactServerCapabilities struct {
	Streaming           bool     `json:"streaming"`
	Append              bool     `json:"append"`
	Cancel              bool     `json:"cancel"`
	PreviousInteraction bool     `json:"previous_interaction"`
	ResumeSession       bool     `json:"resume_session"`
	MCPServers          bool     `json:"mcp_servers"`
	FunctionTools       bool     `json:"function_tools"`
	PermissionModes     []string `json:"permission_modes"`
}

// iactModelDetail is one model's entry in the handshake. The effort set
// lands under `thinking_levels`, which is what Google's own generation
// config calls it.
type iactModelDetail struct {
	ID                  string   `json:"id"`
	DisplayName         string   `json:"display_name,omitempty"`
	ContextWindowTokens int      `json:"context_window_tokens,omitempty"`
	ThinkingLevels      []string `json:"thinking_levels,omitempty"`
}

// iactFunctionCallParams is a Google FunctionCallStep, sent to the client
// for execution.
type iactFunctionCallParams struct {
	InteractionID string          `json:"interaction_id"`
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Arguments     json.RawMessage `json:"arguments"`
}

// iactFunctionCallResult is a Google FunctionResultStep, returned by the
// client. A client that cannot run the call answers with a JSON-RPC error
// rather than leaving the request pending; the call then fails as a tool
// error and the model sees why, which is the same outcome as a tool that
// errored.
type iactFunctionCallResult struct {
	Result  []iactContent `json:"result"`
	IsError bool          `json:"is_error,omitempty"`
}

func (Interactions) DecodeCreate(params json.RawMessage) (*CreateRequest, *rpcError) {
	var p iactCreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "interactions.create params: %v", err)
	}
	if p.Agent != "" {
		return nil, errorf(CodeUnsupported, "agent: a managed agent runs on Google's machines, and this process runs the loop on the parent's; name a model instead")
	}
	prompt, rerr := iactInputText(p.Input)
	if rerr != nil {
		return nil, rerr
	}
	req := &CreateRequest{
		Model:         p.Model,
		Prompt:        prompt,
		Instructions:  p.SystemInstruction,
		PreviousRunID: p.PreviousInteractionID,
		Tools:         p.Tools,
		Stream:        p.Stream,
		Harness:       p.Harness,
	}
	if g := p.GenerationConfig; g != nil {
		req.Effort = g.ThinkingLevel
		req.MaxOutputTokens = g.MaxOutputTokens
	}
	if f := p.ResponseFormat; f != nil {
		req.ResultSchema = f.Schema
	}
	return req, nil
}

func (Interactions) DecodeAppend(params json.RawMessage) (*AppendRequest, *rpcError) {
	var p iactAppendParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "interactions.append params: %v", err)
	}
	prompt, rerr := iactInputText(p.Input)
	if rerr != nil {
		return nil, rerr
	}
	req := &AppendRequest{RunID: p.InteractionID, Prompt: prompt}
	if p.Harness != nil {
		req.MessageID = p.Harness.MessageID
	}
	return req, nil
}

func (Interactions) DecodeID(method string, params json.RawMessage) (string, *rpcError) {
	var p iactIDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return "", errorf(CodeInvalidParams, "%s params: %v", method, err)
	}
	return p.InteractionID, nil
}

func (Interactions) Initialize(h Handshake) any {
	details := make([]iactModelDetail, 0, len(h.Details))
	for _, d := range h.Details {
		details = append(details, iactModelDetail{
			ID:                  d.ID,
			DisplayName:         d.DisplayName,
			ContextWindowTokens: d.ContextWindowTokens,
			ThinkingLevels:      d.Efforts,
		})
	}
	return iactInitializeResult{
		ServerInfo: ServerInfo{
			Name:     h.Name,
			Version:  h.Version,
			Protocol: Interactions{}.Protocol(),
		},
		Capabilities: iactServerCapabilities{
			Streaming:           true,
			Append:              true,
			Cancel:              true,
			PreviousInteraction: true,
			ResumeSession:       true,
			MCPServers:          h.MCPServers,
			FunctionTools:       true,
			PermissionModes:     h.PermissionModes,
		},
		Models:       h.Models,
		DefaultModel: h.DefaultModel,
		ModelDetails: details,
	}
}

func (Interactions) AppendResult(runID string, seq int64) any {
	return iactAppendResult{InteractionID: runID, Seq: seq}
}

func (Interactions) NewTranslator(runID, model string, emit func(method string, params any)) Translator {
	return newIactTranslator(runID, model, emit)
}

func (Interactions) CallParams(c FunctionCall) any {
	return iactFunctionCallParams{
		InteractionID: c.RunID,
		Type:          iactStepFunctionCall,
		// The id is the one the client already saw on this call's
		// function_call step, so the answer it renders lands under the
		// right call (internal/tools, WithCallID).
		ID:        c.CallID,
		Name:      c.Name,
		Arguments: c.Arguments,
	}
}

func (Interactions) CallContent(raw json.RawMessage) (tools.MCPContent, error) {
	var res iactFunctionCallResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			return tools.MCPContent{}, err
		}
	}
	out := tools.MCPContent{IsError: res.IsError}
	for _, c := range res.Result {
		switch c.Type {
		case "text":
			if out.Text != "" {
				out.Text += "\n"
			}
			out.Text += c.Text
		case "image":
			// Google carries an image as a mime type beside a payload,
			// where the Responses surface carries one data URL, so there is
			// nothing to split here.
			data, err := decodeBase64(c.Data)
			if err != nil {
				continue
			}
			out.Images = append(out.Images, tools.MCPImage{MIMEType: c.MIMEType, Data: data})
		}
	}
	return out, nil
}

// iactInputText flattens Google's polymorphic `input` into the one
// instruction the loop takes. All four forms Google accepts are read: a bare
// string, one Content, an array of Content, and an array of Step.
func iactInputText(raw json.RawMessage) (string, *rpcError) {
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
		return iactInputElement(raw)
	}
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return "", errorf(CodeInvalidParams, "input: %v", err)
	}
	var parts []string
	for _, el := range raws {
		t, rerr := iactInputElement(el)
		if rerr != nil {
			return "", rerr
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, "\n\n"), nil
}

// iactInputElement reads one element of an input array: a Content block, or
// a user_input Step whose own content is Content blocks.
//
// Only `user_input` steps are read. A create body carrying a function_call
// or a function_result would be a client trying to replay a conversation
// this process already holds in its own event log.
func iactInputElement(raw json.RawMessage) (string, *rpcError) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", errorf(CodeInvalidParams, "input: %v", err)
	}
	switch probe.Type {
	case "text", "image":
		var c iactContent
		if err := json.Unmarshal(raw, &c); err != nil {
			return "", errorf(CodeInvalidParams, "input: %v", err)
		}
		return iactContentText([]iactContent{c})
	case iactStepUserInput:
		var step iactStep
		if err := json.Unmarshal(raw, &step); err != nil {
			return "", errorf(CodeInvalidParams, "input: %v", err)
		}
		return iactContentText(step.Content)
	default:
		return "", errorf(CodeUnsupported,
			"input element type %q: only text content and %q steps are accepted as input",
			probe.Type, iactStepUserInput)
	}
}

// iactContentText flattens content blocks to the text the loop takes. An
// image in a create body is refused rather than dropped: this process reads
// images off the filesystem with its own tools, and one inlined here would
// be silently ignored.
func iactContentText(blocks []iactContent) (string, *rpcError) {
	var parts []string
	for _, c := range blocks {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image":
			return "", errorf(CodeUnsupported, "an image in `input` is not read; this session reads images off its own filesystem with the Read tool")
		}
	}
	return strings.Join(parts, "\n\n"), nil
}
