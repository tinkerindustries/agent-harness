package stdiosession

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// The OpenAI Responses API's vocabulary, which `harness stdio-session`
// speaks. Its REST methods on POST /responses are the JSON-RPC methods and
// its semantic server-sent events are the notifications, so a client written
// against that surface reads these without a translation table
// (docs/STDIO-PROTOCOL.md).
//
// It is the same vocabulary internal/deepseek sends the provider
// (docs/DEEPSEEK-RESPONSES.md) and the same one the loop itself folds into
// (wire.Item). That is a coincidence worth naming rather than a design: this
// surface is what a parent reads, and a Gemini session serves the same loop
// over Google's Interactions vocabulary instead (interactions.go).

// Responses implements Dialect.
type Responses struct{}

// NewResponses returns the Responses dialect. It holds no state.
func NewResponses() Responses { return Responses{} }

func (Responses) Protocol() string { return "openai.responses.v1" }

func (Responses) Methods() MethodSet {
	return MethodSet{
		Create: MethodResponsesCreate,
		Append: MethodResponsesAppend,
		Cancel: MethodResponsesCancel,
		Get:    MethodResponsesGet,
		Delete: MethodResponsesDelete,
	}
}

// NewRunID mints the id a response is addressed by. It is not the session
// id: a chain of responses linked by previous_response_id is one session
// resumed repeatedly, so the two cannot be the same value.
// harness.session_id on every response carries the session's own.
func (Responses) NewRunID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("stdiosession: crypto/rand unavailable: " + err.Error())
	}
	return "resp_" + hex.EncodeToString(b[:])
}

// AddressID returns runID unchanged: a client of this dialect holds the
// response id as its primary address.
func (Responses) AddressID(runID, sessionID string) string { return runID }

func (Responses) AddressesSession() bool { return false }

func (Responses) Messages() Messages {
	return Messages{
		AlreadyRunning:       "response %s is still running; one process hosts one session, so cancel it or wait for it to complete",
		PreviousNotFound:     "no response %q was minted by this process; a response id does not outlive the process that made it, so continue across a restart with harness.resume_session_id instead",
		PreviousChangedCWD:   "response %s works in %s; a continued response cannot change directory, and inherits the one its chain started in",
		PreviousChangedModel: "response %s ran on %s; a continued response cannot change model, because the session's prefix is frozen",
		ContextEnded:         "response %s: %v",
		NotFound:             "no response %q",
		AppendNotRunning:     "response %s is %s; start a new response with previous_response_id set to it instead",
		AppendNeedsInput:     "responses.append needs some input",
		DeleteStillRunning:   "response %s is still running; cancel it first",
		EffortRefused:        "reasoning.effort %q: %s accepts %s",
		SchemaFrozen:         "session %s carries its own result schema; a resumed session keeps the one it was started with, and text.format cannot replace it",
		BothContinuations:    "previous_response_id and harness.resume_session_id both name a conversation to continue; send one. previous_response_id continues a response this process ran, harness.resume_session_id continues a session out of the state directory",
	}
}

func (Responses) DecodeCreate(params json.RawMessage) (*CreateRequest, *rpcError) {
	return decodeCreate(params)
}

func (Responses) DecodeAppend(params json.RawMessage) (*AppendRequest, *rpcError) {
	return decodeAppend(params)
}

func (Responses) DecodeID(method string, params json.RawMessage) (string, *rpcError) {
	var p IDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return "", errorf(CodeInvalidParams, "%s params: %v", method, err)
	}
	return p.ResponseID, nil
}

func (Responses) Initialize(h Handshake) any {
	details := make([]ModelDetail, 0, len(h.Details))
	for _, d := range h.Details {
		details = append(details, ModelDetail{
			ID:                  d.ID,
			DisplayName:         d.DisplayName,
			ContextWindowTokens: d.ContextWindowTokens,
			ReasoningEfforts:    d.Efforts,
		})
	}
	return InitializeResult{
		ServerInfo: ServerInfo{
			Name:     h.Name,
			Version:  h.Version,
			Protocol: Responses{}.Protocol(),
		},
		Capabilities: ServerCapabilities{
			Streaming:        true,
			Append:           true,
			Cancel:           true,
			PreviousResponse: true,
			ResumeSession:    true,
			MCPServers:       h.MCPServers,
			FunctionTools:    true,
			PermissionModes:  h.PermissionModes,
		},
		Models:       h.Models,
		DefaultModel: h.DefaultModel,
		ModelDetails: details,
	}
}

func (Responses) AppendResult(runID string, seq int64) any {
	return AppendResult{ResponseID: runID, Seq: seq}
}

func (Responses) NewTranslator(runID, sessionID, model string, emit func(method string, params any)) Translator {
	return newTranslator(runID, model, emit)
}

func (Responses) CallParams(c FunctionCall) any {
	return FunctionCallParams{
		ResponseID: c.RunID,
		Type:       ItemFunctionCall,
		CallID:     c.CallID,
		Name:       c.Name,
		Arguments:  c.Arguments,
	}
}

func (Responses) CallContent(raw json.RawMessage) (tools.MCPContent, error) {
	var res FunctionCallResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &res); err != nil {
			return tools.MCPContent{}, err
		}
	}
	return contentFrom(res), nil
}

// contentFrom flattens a client's function_call_output into the MCPContent
// the executor turns into a tool result — the same shape internal/mcpclient
// produces from a real server's reply, so the executor cannot tell which
// path a result came back on.
func contentFrom(res FunctionCallResult) tools.MCPContent {
	out := tools.MCPContent{IsError: res.IsError}
	for _, c := range res.Output {
		switch c.Type {
		case PartInputText, PartOutputText, "text":
			if out.Text != "" {
				out.Text += "\n"
			}
			out.Text += c.Text
		case PartInputImage:
			// The surface carries an image as one base64 data URL rather
			// than a mime type beside a payload, so it is split here into
			// what the executor's own image type wants.
			mime, b64, ok := splitDataURI(c.ImageURL)
			if !ok {
				continue
			}
			data, err := decodeBase64(b64)
			if err != nil {
				continue
			}
			out.Images = append(out.Images, tools.MCPImage{MIMEType: mime, Data: data})
		}
	}
	return out
}

// splitDataURI splits a "data:<mime>;base64,<payload>" URL into its mime
// type and payload. The Responses surface carries an image as one such URL,
// where the executor's own image type wants the two separately.
func splitDataURI(uri string) (mime, payload string, ok bool) {
	const prefix = "data:"
	if !strings.HasPrefix(uri, prefix) {
		return "", "", false
	}
	rest := uri[len(prefix):]
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	header := rest[:comma]
	semi := strings.IndexByte(header, ';')
	if semi < 0 || header[semi+1:] != "base64" {
		return "", "", false
	}
	return header[:semi], rest[comma+1:], true
}
