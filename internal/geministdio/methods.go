package geministdio

import "encoding/json"

// Method names the client may call. The interactions.* four are Google's own
// REST methods on `/v1beta/interactions`, spelled as JSON-RPC methods.
// interactions.append is this protocol's own — Google has no verb for adding
// input to an interaction that is already running, because on Google's
// surface an interaction is one model call and there is no window in which to
// add anything. It is what steering is (docs/RUN-CONTROL.md).
const (
	MethodInitialize         = "initialize"
	MethodInitialized        = "initialized"
	MethodInteractionsCreate = "interactions.create"
	MethodInteractionsAppend = "interactions.append"
	MethodInteractionsCancel = "interactions.cancel"
	MethodInteractionsGet    = "interactions.get"
	MethodInteractionsDelete = "interactions.delete"
	MethodShutdown           = "shutdown"

	// MethodFunctionCall is the one request this side sends the client: run
	// a function tool the client declared in the interaction's `tools`
	// array. Its params are a Google FunctionCallStep and its result a
	// Google FunctionResultStep, so the shapes are the vendor's even though
	// the direction is not.
	MethodFunctionCall = "harness.function_call"
)

// --- initialize ---

// InitializeParams is what the client sends first. Nothing else is answered
// until it has, and until the `initialized` notification follows it.
type InitializeParams struct {
	// ClientInfo names the parent, for logs and for the User-Agent this
	// process sends to Google.
	ClientInfo ClientInfo `json:"client_info"`
	// APIRevision is the `Api-Revision` header value the client wants sent
	// to Google. Empty leaves this harness's own pinned revision in place.
	APIRevision string `json:"api_revision,omitempty"`
	// Capabilities is what the client can do. A capability the client does
	// not claim is never exercised: without `function_calls`, a `function`
	// tool in a create body is rejected rather than silently never called.
	Capabilities ClientCapabilities `json:"capabilities"`
}

// ClientInfo names the parent process.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// ClientCapabilities is what the client implements.
type ClientCapabilities struct {
	// FunctionCalls says the client answers harness.function_call requests.
	FunctionCalls bool `json:"function_calls,omitempty"`
}

// InitializeResult is this process's own half of the handshake.
type InitializeResult struct {
	ServerInfo   ServerInfo         `json:"server_info"`
	Capabilities ServerCapabilities `json:"capabilities"`
	// Models is the model list this process will accept in a create body.
	Models []string `json:"models"`
	// DefaultModel is what a create body with no `model` gets.
	DefaultModel string `json:"default_model"`
}

// ServerInfo names this process.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Protocol is the Google surface this process speaks on both sides:
	// the vocabulary over the pipe and the API it calls.
	Protocol string `json:"protocol"`
}

// ServerCapabilities is what this process implements.
type ServerCapabilities struct {
	// Streaming is always true: create returns immediately and the steps
	// arrive as notifications.
	Streaming bool `json:"streaming"`
	// Append says interactions.append is implemented — steering.
	Append bool `json:"append"`
	// Cancel says interactions.cancel is implemented.
	Cancel bool `json:"cancel"`
	// PreviousInteraction says previous_interaction_id is implemented —
	// resuming a finished interaction in place.
	PreviousInteraction bool `json:"previous_interaction"`
	// MCPServers says an `mcp_server` tool in a create body is dialled.
	MCPServers bool `json:"mcp_servers"`
	// FunctionTools says a `function` tool in a create body is called back
	// over harness.function_call.
	FunctionTools bool `json:"function_tools"`
	// PermissionModes is the set of values harness.permission_mode takes.
	PermissionModes []string `json:"permission_modes"`
}

// --- interactions.create ---

// CreateParams is Google's create-interaction request body, narrowed to what
// this surface honours, plus the `harness` block. Anything Google's schema
// allows and this does not is rejected by name rather than ignored: a client
// that sent `agent` and got a model run would have no way to tell.
type CreateParams struct {
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
	// prefix (docs/CACHE.md). docs/STDIO-PROTOCOL.md records the deviation.
	SystemInstruction string `json:"system_instruction,omitempty"`
	Tools             []Tool `json:"tools,omitempty"`
	// PreviousInteractionID continues an interaction this process already
	// ran, the way Google's own multi-turn chaining does. It replaces a
	// thread concept: the chain of previous_interaction_id links is the
	// thread.
	PreviousInteractionID string            `json:"previous_interaction_id,omitempty"`
	ResponseFormat        *ResponseFormat   `json:"response_format,omitempty"`
	GenerationConfig      *GenerationConfig `json:"generation_config,omitempty"`
	// Stream, when false, holds the JSON-RPC answer until the run ends and
	// returns the whole assembled Interaction. The step notifications are
	// sent either way, so a client that sets it false and ignores them gets
	// exactly Google's non-streaming behaviour.
	Stream *bool `json:"stream,omitempty"`
	// Store is accepted and ignored. This process always stores: the loop's
	// own state machine is its event log, and a run that kept nothing could
	// not be resumed or even folded (internal/session).
	Store *bool `json:"store,omitempty"`

	Harness *CreateHarness `json:"harness,omitempty"`
}

// ResponseFormat is Google's text response format: a JSON schema the answer
// must satisfy. It becomes the session's result schema, which is what the
// Complete tool validates against (docs/TOOLS.md).
type ResponseFormat struct {
	Type     string          `json:"type"`
	MIMEType string          `json:"mime_type,omitempty"`
	Schema   json.RawMessage `json:"schema,omitempty"`
}

// GenerationConfig is Google's generation config, narrowed to the two fields
// that mean something to an agent loop.
type GenerationConfig struct {
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	ThinkingLevel   string `json:"thinking_level,omitempty"`
}

// CreateHarness is the extension block on a create body: the things a
// session on somebody else's machine needs that an HTTP API to Google's
// machines has no reason to have.
type CreateHarness struct {
	// CWD is the directory the session works in — the parent's own, not a
	// workspace this harness cloned. Required on a create with no
	// previous_interaction_id; a continued interaction keeps the directory
	// its chain started in, since the session's prefix is frozen.
	CWD string `json:"cwd"`
	// PermissionMode is "readonly" or "full", set once for the interaction
	// chain and never asked about again (docs/TOOLS.md, "Permissions").
	PermissionMode string `json:"permission_mode,omitempty"`
	// Deny is the substring patterns a tool call's descriptor is refused
	// for. It only ever subtracts from what PermissionMode allows.
	Deny []string `json:"deny,omitempty"`
	// MaxSubTurns caps how many sub-turns the run may take. Zero is the
	// harness's own default.
	MaxSubTurns int `json:"max_sub_turns,omitempty"`
	// MessageID is echoed back on the user_input step this input becomes,
	// so a parent that optimistically rendered the message can match the
	// echo to it.
	MessageID string `json:"message_id,omitempty"`
	// Title and Description name the run in this harness's own records.
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// Tool is Google's Tool union, narrowed to the two members that mean
// something when the loop runs here: a `function` the client executes, and an
// `mcp_server` this process dials. Every other member is refused by name.
type Tool struct {
	Type string `json:"type"`

	// Function fields.
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`

	// McpServer fields. Name is shared with the function member, which is
	// what Google's own schema does.
	URL          string            `json:"url,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	AllowedTools []AllowedTools    `json:"allowed_tools,omitempty"`

	Harness *ToolHarness `json:"harness,omitempty"`
}

// AllowedTools is Google's per-server tool allowance.
type AllowedTools struct {
	Mode  string   `json:"mode,omitempty"`
	Tools []string `json:"tools,omitempty"`
}

// ToolHarness is the extension block on a tool.
type ToolHarness struct {
	// ReadOnly marks a server whose tools a readonly-mode session may still
	// call. An MCP server reaches outside the working directory by
	// definition, so readonly refuses it unless the client says otherwise
	// (docs/MCP.md, "Permissions").
	ReadOnly bool `json:"read_only,omitempty"`
}

// Google tool `type` values.
const (
	ToolFunction  = "function"
	ToolMCPServer = "mcp_server"
)

// CreateResult is what interactions.create answers with. On a streaming
// create it is the interaction as at interaction.created — id, model,
// in_progress — and the steps follow as notifications. On stream:false it is
// the whole finished interaction.
type CreateResult struct {
	Interaction Interaction `json:"interaction"`
}

// --- interactions.append ---

// AppendParams adds input to an interaction that is already running. It is
// the same `input` union a create takes, so the client builds it the same
// way.
type AppendParams struct {
	InteractionID string          `json:"interaction_id"`
	Input         json.RawMessage `json:"input"`
	Harness       *AppendHarness  `json:"harness,omitempty"`
}

// AppendHarness is the extension block on an append.
type AppendHarness struct {
	// MessageID is echoed on the user_input step this input becomes, the
	// same way CreateHarness.MessageID is.
	MessageID string `json:"message_id,omitempty"`
}

// AppendResult names the interaction the input was accepted for and where it
// landed in that session's log. A client that sent an append and got this
// back knows the input is committed; it reaches the model at the next
// sub-turn boundary, never mid-tool-call.
type AppendResult struct {
	InteractionID string `json:"interaction_id"`
	Seq           int64  `json:"seq"`
}

// --- interactions.cancel / get / delete ---

// IDParams is the body of cancel, get and delete: Google's path parameter,
// which a JSON-RPC call has to carry in the params instead.
type IDParams struct {
	InteractionID string `json:"interaction_id"`
}

// GetResult and CancelResult both return Google's Interaction resource, as
// Google's own GET and cancel do.
type GetResult struct {
	Interaction Interaction `json:"interaction"`
}

// --- harness.function_call ---

// FunctionCallParams is a Google FunctionCallStep, sent to the client for
// execution.
type FunctionCallParams struct {
	InteractionID string          `json:"interaction_id"`
	Type          string          `json:"type"`
	ID            string          `json:"id"`
	Name          string          `json:"name"`
	Arguments     json.RawMessage `json:"arguments"`
}

// FunctionCallResult is a Google FunctionResultStep, returned by the client.
// A client that cannot run the call answers with a JSON-RPC error rather
// than leaving the request pending; the call then fails as a tool error and
// the model sees why, which is the same outcome as a tool that errored.
type FunctionCallResult struct {
	Result  []Content `json:"result"`
	IsError bool      `json:"is_error,omitempty"`
}
