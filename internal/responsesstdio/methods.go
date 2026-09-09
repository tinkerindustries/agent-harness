package responsesstdio

import "encoding/json"

// Method names the client may call. responses.create / .get / .cancel /
// .delete are the Responses API's own REST methods, spelled as JSON-RPC
// methods. responses.append is this protocol's own — the surface has no verb
// for adding input to a response that is already running, because there a
// response is one model call and there is no window in which to add
// anything. It is what steering is (docs/RUN-CONTROL.md).
const (
	MethodInitialize      = "initialize"
	MethodInitialized     = "initialized"
	MethodResponsesCreate = "responses.create"
	MethodResponsesAppend = "responses.append"
	MethodResponsesCancel = "responses.cancel"
	MethodResponsesGet    = "responses.get"
	MethodResponsesDelete = "responses.delete"
	MethodShutdown        = "shutdown"

	// MethodFunctionCall is the one request this side sends the client: run
	// a function tool the client declared in the response's `tools` array.
	// Its params are a function_call output item and its result a
	// function_call_output's own fields, so the shapes are the surface's
	// even though the direction is not.
	MethodFunctionCall = "harness.function_call"
)

// --- initialize ---

// InitializeParams is what the client sends first. Nothing else is answered
// until it has, and until the `initialized` notification follows it.
type InitializeParams struct {
	// ClientInfo names the parent, for logs and for the User-Agent this
	// process sends to Google.
	ClientInfo ClientInfo `json:"client_info"`
	// APIRevision is a provider API revision the client wants pinned, for a
	// provider that versions its surface that way. Empty leaves this
	// harness's own pinned revision in place.
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
	// ModelDetails carries the per-model capability data a bare id list
	// cannot: how large a context window each model accepts and which
	// reasoning.effort values it honours, so a client never has to learn
	// either from a failed create. One entry per name in
	// Models, in the same order — never a parallel map a client has to
	// reconcile against it by name. server_info.protocol and
	// server_info.version remain the only two compatibility fields; this
	// carries capability data, not a version.
	ModelDetails []ModelDetail `json:"model_details,omitempty"`
}

// ModelDetail is one model's entry in InitializeResult.ModelDetails.
type ModelDetail struct {
	ID string `json:"id"`
	// DisplayName is a human-readable name for the model, when its provider
	// has one to offer; omitted for a model this process accepts but cannot
	// describe further.
	DisplayName string `json:"display_name,omitempty"`
	// ContextWindowTokens is the model's total input token budget, so a
	// client can compute a context percentage without hardcoding a figure
	// that drifts as a provider's own limits change. Zero, and omitted, for
	// a model this process has no figure for.
	ContextWindowTokens int `json:"context_window_tokens,omitempty"`
	// ReasoningEfforts is what reasoning.effort may be for this model,
	// because the answer differs between them: DeepSeek takes low/high/max
	// and gemini-3.7-flash takes low/medium/high. Empty when this process
	// has no table for the model, in which case a create naming any effort
	// reaches the API for it to judge rather than being refused here.
	ReasoningEfforts []string `json:"reasoning_efforts,omitempty"`
}

// ServerInfo names this process.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	// Protocol is the surface this process speaks on both sides: the
	// vocabulary over the pipe and the API it calls.
	Protocol string `json:"protocol"`
}

// ServerCapabilities is what this process implements.
type ServerCapabilities struct {
	// Streaming is always true: create returns immediately and the output
	// items arrive as notifications.
	Streaming bool `json:"streaming"`
	// Append says responses.append is implemented — steering.
	Append bool `json:"append"`
	// Cancel says responses.cancel is implemented.
	Cancel bool `json:"cancel"`
	// PreviousResponse says previous_response_id is implemented — continuing
	// a finished response in place. It is bounded by this process: the ids
	// it resolves are the ones this process minted.
	PreviousResponse bool `json:"previous_response"`
	// ResumeSession says harness.resume_session_id is implemented — picking
	// a session up out of the state directory, which is what carries a
	// conversation across a restart of this process.
	ResumeSession bool `json:"resume_session"`
	// MCPServers says an `mcp_server` tool in a create body is dialled.
	MCPServers bool `json:"mcp_servers"`
	// FunctionTools says a `function` tool in a create body is called back
	// over harness.function_call.
	FunctionTools bool `json:"function_tools"`
	// PermissionModes is the set of values harness.permission_mode takes.
	PermissionModes []string `json:"permission_modes"`
}

// --- responses.create ---

// CreateParams is the Responses API's create-response request body, narrowed
// to what this surface honours, plus the `harness` block. Anything the
// surface allows and this does not is rejected by name rather than ignored:
// a client that sent `background` and got a foreground run would have no way
// to tell.
type CreateParams struct {
	Model string `json:"model,omitempty"`
	// Input is the surface's polymorphic input: a bare string, or a list of
	// input items. Both are accepted.
	Input json.RawMessage `json:"input"`
	// Instructions is prepended to the response's first user input rather
	// than replacing this harness's system prompt, which is frozen for a
	// session's life and is the whole of the prompt cache's shared prefix
	// (docs/CACHE.md). docs/STDIO-PROTOCOL.md records the deviation.
	Instructions string `json:"instructions,omitempty"`
	Tools        []Tool `json:"tools,omitempty"`
	// PreviousResponseID continues a response this process already ran, the
	// way the surface's own multi-turn chaining does. It replaces a thread
	// concept: the chain of previous_response_id links is the thread.
	//
	// DeepSeek's own implementation of this field is "not supported
	// (stateless API)", and that is not a contradiction: the state a
	// continuation needs lives here, in this process's own event log,
	// rather than at the provider. The whole conversation is re-sent on
	// every request either way.
	PreviousResponseID string           `json:"previous_response_id,omitempty"`
	Text               *TextConfig      `json:"text,omitempty"`
	Reasoning          *ReasoningConfig `json:"reasoning,omitempty"`
	// MaxOutputTokens caps one model call's output, reasoning included. It
	// is per request, not per run: a response here spans many.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// Stream, when false, holds the JSON-RPC answer until the run ends and
	// returns the whole assembled Response. The notifications are sent
	// either way, so a client that sets it false and ignores them gets
	// exactly the surface's non-streaming behaviour.
	Stream *bool `json:"stream,omitempty"`
	// Store is accepted and ignored. This process always stores: the loop's
	// own state machine is its event log, and a run that kept nothing could
	// not be resumed or even folded (internal/session).
	Store *bool `json:"store,omitempty"`
	// Background is accepted only so it can be refused with a message that
	// says why: every response here is already asynchronous, answered at
	// once and streamed as it goes, so a second asynchrony would mean
	// nothing.
	Background *bool `json:"background,omitempty"`

	Harness *CreateHarness `json:"harness,omitempty"`
}

// TextConfig is the surface's text output configuration. Only `format` is
// honoured, and only its json_schema member means anything here: the schema
// becomes the session's result schema, which is what the Complete tool
// validates against (docs/TOOLS.md).
type TextConfig struct {
	Format *TextFormat `json:"format,omitempty"`
}

// TextFormat is the output format: `text`, `json_object`, or `json_schema`
// with a schema.
type TextFormat struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

// ReasoningConfig is the surface's thinking control: one field carrying both
// the toggle and the effort, with "none" the off position.
type ReasoningConfig struct {
	Effort string `json:"effort,omitempty"`
}

// CreateHarness is the extension block on a create body: the things a
// session on somebody else's machine needs that an HTTP API to a provider's
// machines has no reason to have.
type CreateHarness struct {
	// CWD is the directory the session works in — the parent's own, not a
	// workspace this harness cloned. Required on a create that starts a new
	// session; a continued or resumed one keeps the directory its session
	// started in, since the session's prefix is frozen.
	CWD string `json:"cwd"`
	// ResumeSessionID continues the session of that id, read out of the
	// state directory rather than out of this process's memory. It is what
	// carries a conversation across a restart of this process, which
	// previous_response_id cannot do: a response id is minted in memory and
	// dies with the process, while a session id names a row in the SQLite
	// file under -state-dir.
	//
	// A resumed session keeps everything its prefix is built from — model,
	// working directory, permission mode, deny patterns, result schema and
	// tool array. Naming any of them differently is refused rather than
	// ignored. The tools are the one that takes work: the create must
	// re-declare every mcp_server the session froze, with connection
	// metadata that is good now, and every function tool with the schema it
	// had. docs/STDIO-PROTOCOL.md, "Resuming across process restarts".
	ResumeSessionID string `json:"resume_session_id,omitempty"`
	// PermissionMode is "readonly" or "full", set once for the response
	// chain and never asked about again (docs/TOOLS.md, "Permissions").
	PermissionMode string `json:"permission_mode,omitempty"`
	// Deny is the substring patterns a tool call's descriptor is refused
	// for. It only ever subtracts from what PermissionMode allows.
	Deny []string `json:"deny,omitempty"`
	// MaxSubTurns caps how many sub-turns the run may take. Zero is the
	// harness's own default.
	MaxSubTurns int `json:"max_sub_turns,omitempty"`
	// MessageID is echoed back on the user message item this input becomes,
	// so a parent that optimistically rendered the message can match the
	// echo to it.
	MessageID string `json:"message_id,omitempty"`
	// Title and Description name the run in this harness's own records.
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// Tool is the surface's tool union, narrowed to the two members that mean
// something when the loop runs here: a `function` the client executes, and
// an `mcp_server` this process dials. Every other member — a provider's own
// server-side tools among them — is refused by name.
type Tool struct {
	Type string `json:"type"`

	// Function fields.
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`

	// McpServer fields. Name is shared with the function member, which is
	// what the surface's own schema does. URL/Headers dial an HTTP server;
	// Command/Args/Env dial one over stdio (internal/mcpclient's
	// store.MCPTransportStdio dialer, already used for harness serve's own
	// operator-configured servers). The two pairs are mutually exclusive: a
	// declaration naming both, or neither, is refused.
	URL          string            `json:"url,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	Command      string            `json:"command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	AllowedTools []AllowedTools    `json:"allowed_tools,omitempty"`

	Harness *ToolHarness `json:"harness,omitempty"`
}

// AllowedTools is the per-server tool allowance.
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

// Tool `type` values.
const (
	ToolFunction  = "function"
	ToolMCPServer = "mcp_server"
)

// CreateResult is what responses.create answers with. On a streaming create
// it is the response as at response.created — id, model, in_progress — and
// the output items follow as notifications. On stream:false it is the whole
// finished response.
type CreateResult struct {
	Response Response `json:"response"`
}

// --- responses.append ---

// AppendParams adds input to a response that is already running. It is the
// same `input` union a create takes, so the client builds it the same way.
type AppendParams struct {
	ResponseID string          `json:"response_id"`
	Input      json.RawMessage `json:"input"`
	Harness    *AppendHarness  `json:"harness,omitempty"`
}

// AppendHarness is the extension block on an append.
type AppendHarness struct {
	// MessageID is echoed on the user message item this input becomes, the
	// same way CreateHarness.MessageID is.
	MessageID string `json:"message_id,omitempty"`
}

// AppendResult names the response the input was accepted for and where it
// landed in that session's log. A client that sent an append and got this
// back knows the input is committed; it reaches the model at the next
// sub-turn boundary, never mid-tool-call.
type AppendResult struct {
	ResponseID string `json:"response_id"`
	Seq        int64  `json:"seq"`
}

// --- responses.cancel / get / delete ---

// IDParams is the body of cancel, get and delete: the surface's path
// parameter, which a JSON-RPC call has to carry in the params instead.
type IDParams struct {
	ResponseID string `json:"response_id"`
}

// GetResult and CancelResult both return the `response` resource, as the
// surface's own GET and cancel do.
type GetResult struct {
	Response Response `json:"response"`
}

// --- harness.function_call ---

// FunctionCallParams is a function_call output item, sent to the client for
// execution.
type FunctionCallParams struct {
	ResponseID string          `json:"response_id"`
	Type       string          `json:"type"`
	CallID     string          `json:"call_id"`
	Name       string          `json:"name"`
	Arguments  json.RawMessage `json:"arguments"`
}

// FunctionCallResult is a function_call_output's own fields, returned by the
// client. A client that cannot run the call answers with a JSON-RPC error
// rather than leaving the request pending; the call then fails as a tool
// error and the model sees why, which is the same outcome as a tool that
// errored.
type FunctionCallResult struct {
	Output  []ContentPart `json:"output"`
	IsError bool          `json:"is_error,omitempty"`
}
