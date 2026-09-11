package stdiosession

import (
	"encoding/json"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// A Dialect is one parent-facing vocabulary: its method names, its request
// bodies, its handshake result, its prose, and the factory for the per-run
// Translator. Everything else in this package — the framing, the run
// lifecycle, the tool array, MCP registration, resume, permissions — is the
// same whichever one a process speaks.
//
// Two implement it. responses.go is the OpenAI Responses API's vocabulary,
// which `harness stdio-session` speaks; interactions.go is Google's
// Interactions API's, which `harness gemini-session` speaks. Which one a
// process gets is decided at the composition point, from the subcommand it
// was spawned as (cmd/harness/stdiosession.go).
//
// A Dialect holds no per-run state, so one value serves the process.
//
// Everything it returns as `any` is a value of its own wire types, which this
// package marshals without inspecting. The two vocabularies disagree about
// shape as well as about names — a bare error frame against a whole resource,
// one delta frame against three — so a shared struct would have to be the
// union of both and would misdescribe each.
type Dialect interface {
	// Protocol is what the handshake reports as server_info.protocol:
	// "openai.responses.v1" or "google.interactions.v1beta". With
	// server_info.version it is all a client has to compare against
	// (docs/STDIO-PROTOCOL.md).
	Protocol() string

	// NewRunID mints the id a run is addressed by, in this vocabulary's own
	// spelling — `resp_…` or `int_…`. It is not the session id: a chain of
	// runs linked by the continuation id is one session resumed repeatedly.
	NewRunID() string

	// Methods names the five verbs a client calls. The handshake pair,
	// shutdown and harness.function_call are spelled the same in both and
	// are not here.
	Methods() MethodSet

	// Messages is the prose this surface puts on the errors the shared
	// paths raise.
	Messages() Messages

	// DecodeCreate, DecodeAppend and DecodeID read this surface's own
	// bodies into the neutral forms. A field the surface allows and this
	// process cannot honour is refused here rather than below: the
	// Responses body carries `background` and Google's carries `agent`, and
	// neither exists on the other.
	DecodeCreate(params json.RawMessage) (*CreateRequest, *rpcError)
	DecodeAppend(params json.RawMessage) (*AppendRequest, *rpcError)
	DecodeID(method string, params json.RawMessage) (string, *rpcError)

	// Initialize renders the handshake result. Three parts of it differ:
	// the protocol string, the continuation capability's key
	// (`previous_response` against `previous_interaction`), and the key a
	// model's effort set lands under (`reasoning_efforts` against
	// `thinking_levels`).
	Initialize(h Handshake) any

	// AppendResult is what an accepted append answers with: the run id
	// under this surface's own key, and where the input landed in the log.
	AppendResult(runID string, seq int64) any

	// NewTranslator opens the per-run renderer.
	NewTranslator(runID, model string, emit func(method string, params any)) Translator

	// CallParams builds the params of the one request this side sends, and
	// CallContent reads the client's answer into what the executor turns
	// into a tool result. The two are a pair: an image crosses as one
	// base64 data URL on the Responses surface and as a mime type beside a
	// payload on Google's.
	CallParams(c FunctionCall) any
	CallContent(raw json.RawMessage) (tools.MCPContent, error)
}

// MethodSet is the five verbs in one vocabulary's spelling.
type MethodSet struct {
	Create string
	Append string
	Cancel string
	Get    string
	Delete string
}

// Messages is the prose each surface puts on the errors raised by the paths
// both share.
//
// They are whole format strings rather than a noun the shared code
// interpolates. The two surfaces do not differ only in a word — one says "a
// response" where the other says "an interaction", and one names
// `previous_response_id` where the other names `previous_interaction_id` — and
// a template that pretended otherwise would produce "a interaction" the first
// time somebody added a message to it.
type Messages struct {
	// AlreadyRunning takes the running run's id.
	AlreadyRunning string
	// PreviousNotFound takes the continuation id the create named.
	PreviousNotFound string
	// PreviousChangedCWD takes the run's id and the directory its chain
	// started in.
	PreviousChangedCWD string
	// PreviousChangedModel takes the run's id and the model it ran on.
	PreviousChangedModel string
	// ContextEnded takes the run's id and the context's error, for a
	// non-streaming create whose caller went away.
	ContextEnded string
	// NotFound takes the id a method named.
	NotFound string
	// AppendNotRunning takes the run's id and its status.
	AppendNotRunning string
	// AppendNeedsInput takes nothing.
	AppendNeedsInput string
	// DeleteStillRunning takes the run's id.
	DeleteStillRunning string
	// EffortRefused takes the effort, the model, and the accepted set.
	EffortRefused string
	// SchemaFrozen takes the session id, for a resume that tried to replace
	// the result schema the session was started with.
	SchemaFrozen string
	// BothContinuations takes nothing. A create may name the continuation
	// id or harness.resume_session_id, never both.
	BothContinuations string
}

// Handshake is everything the server knows about itself at initialize. A
// Dialect renders it; the data is the same either way.
type Handshake struct {
	Name            string
	Version         string
	Models          []string
	DefaultModel    string
	Details         []ModelDetails
	MCPServers      bool
	PermissionModes []string
}

// ModelDetails is the neutral per-model row the handshake publishes. Which
// key the efforts land under is the Dialect's.
type ModelDetails struct {
	ID                  string
	DisplayName         string
	ContextWindowTokens int
	Efforts             []string
}

// FunctionCall is one client-declared function tool call to hand back over
// the pipe.
type FunctionCall struct {
	RunID     string
	CallID    string
	Name      string
	Arguments json.RawMessage
}

// modelDetailsFor is what the handshake says about every hosted model, before
// a Dialect spells it. The order is the order the models were given, which is
// the order the handshake's own `models` array carries.
func modelDetailsFor(models []string) []ModelDetails {
	out := make([]ModelDetails, 0, len(models))
	for _, m := range models {
		out = append(out, ModelDetails{
			ID:                  m,
			DisplayName:         displayName(m),
			ContextWindowTokens: contextWindowTokens(m),
			Efforts:             reasoningEfforts(m),
		})
	}
	return out
}
