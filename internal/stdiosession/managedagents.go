package stdiosession

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// Anthropic's Managed Agents vocabulary, which `harness claude-session`
// speaks. Its REST methods on POST /v1/sessions and POST
// /v1/sessions/{id}/events are the JSON-RPC methods and its SSE event types
// are the notifications (docs/STDIO-MANAGED-AGENTS.md). None of Anthropic's
// hosted agent runtime is involved: this process runs the identical loop
// stdio-session and gemini-session run, and the client it talks to
// Anthropic through, internal/anthropic, calls the plain Messages API.
// Managed Agents is borrowed here purely as a parent-facing wire vocabulary.

// Method names the client may call. sessions.events is this dialect's own —
// it covers what .append, a continuing create, .cancel and a client answer
// to a pending custom tool call are on the other two dialects, because
// Anthropic's own surface has one event-posting verb rather than four
// (docs/STDIO-MANAGED-AGENTS.md, "The unit of work").
const (
	MethodSessionsCreate = "sessions.create"
	MethodSessionsEvents = "sessions.events"
	MethodSessionsGet    = "sessions.get"
	MethodSessionsDelete = "sessions.delete"
)

// ManagedAgents implements Dialect.
type ManagedAgents struct{}

// NewManagedAgents returns the ManagedAgents dialect. It holds no state.
func NewManagedAgents() ManagedAgents { return ManagedAgents{} }

func (ManagedAgents) Protocol() string { return "anthropic.managed_agents.v1beta" }

// Methods leaves Append and Cancel empty: this dialect answers to neither
// name on the wire (both are folded into sessions.events, intercepted ahead
// of Server.handle's generic switch — see MethodSessionsEvents there), and
// an empty MethodSet member can never match an incoming JSON-RPC method,
// which always carries at least one character.
func (ManagedAgents) Methods() MethodSet {
	return MethodSet{
		Create: MethodSessionsCreate,
		Get:    MethodSessionsGet,
		Delete: MethodSessionsDelete,
	}
}

// NewRunID mints the id a turn is addressed by, this dialect's own
// spelling. It is never serialised as a top-level id — only nested under
// harness.turn_id — because this vocabulary's one client-facing address is
// the session id (AddressID, AddressesSession).
func (ManagedAgents) NewRunID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("stdiosession: crypto/rand unavailable: " + err.Error())
	}
	return "turn_" + hex.EncodeToString(b[:])
}

func (ManagedAgents) AddressID(runID, sessionID string) string { return sessionID }

func (ManagedAgents) AddressesSession() bool { return true }

func (ManagedAgents) Messages() Messages {
	return Messages{
		AlreadyRunning:       "session %s is still running; one process hosts one session, so interrupt it or wait for it to go idle",
		PreviousNotFound:     "no session %q; a session id does not outlive the process that made it unless harness.resume_session_id names it",
		PreviousChangedCWD:   "session %s works in %s; a continued session cannot change directory, and inherits the one its chain started in",
		PreviousChangedModel: "session %s ran on %s; a continued session cannot change model, because the session's prefix is frozen",
		ContextEnded:         "session %s: %v",
		NotFound:             "no session or turn %q",
		AppendNotRunning:     "session %s is %s; post user.message to it once it is idle instead",
		AppendNeedsInput:     "user.message needs some content",
		DeleteStillRunning:   "session %s is still running; interrupt it first",
		EffortRefused:        "agent.model.effort %q: %s accepts %s",
		SchemaFrozen:         "session %s carries its own result schema; a resumed session keeps the one it was started with, and harness.result_schema cannot replace it",
		BothContinuations:    "this dialect has no previous-turn id to continue: post a user.message through sessions.events, or harness.resume_session_id to continue across a restart, never both at once",
	}
}

func (ManagedAgents) DecodeCreate(params json.RawMessage) (*CreateRequest, *rpcError) {
	var p maCreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, errorf(CodeInvalidParams, "sessions.create params: %v", err)
	}
	if len(p.EnvironmentID) > 0 {
		return nil, errorf(CodeUnsupported, "environment_id: this process has no environment resource; harness.cwd fills its role")
	}
	if len(p.VaultIDs) > 0 {
		return nil, errorf(CodeUnsupported, "vault_ids: this process has no vault resource")
	}
	if len(p.Budget) > 0 {
		return nil, errorf(CodeUnsupported, "budget: this process enforces no session-level budget")
	}
	if p.Harness == nil || (p.Harness.CWD == "" && p.Harness.ResumeSessionID == "") {
		return nil, errorf(CodeInvalidParams, "harness.cwd is required, unless this create names harness.resume_session_id: this process works in a directory the client owns and does not choose one for itself")
	}

	model, effort, rerr := maDecodeAgent(p.Agent)
	if rerr != nil {
		return nil, rerr
	}

	if p.Harness.ResumeSessionID != "" {
		// A resuming create inherits the session's own opening message from
		// its stored history; initial_events would be a second one and is
		// refused the same way the other two dialects refuse re-declaring
		// input on a resume that only means to pick a session back up
		// (docs/STDIO-MANAGED-AGENTS.md, "Resuming across process restarts").
		if len(p.InitialEvents) > 1 {
			return nil, errorf(CodeInvalidParams, "initial_events: at most one user.message is accepted")
		}
		prompt := ""
		if len(p.InitialEvents) == 1 {
			prompt, rerr = maDecodeInitialEvent(p.InitialEvents[0])
			if rerr != nil {
				return nil, rerr
			}
		}
		return &CreateRequest{
			Model: model, Prompt: prompt, Effort: effort,
			ResultSchema: p.Harness.ResultSchema,
			Tools:        p.Tools, Stream: p.Stream, Harness: p.Harness.toCreateHarness(),
		}, nil
	}

	if len(p.InitialEvents) == 0 {
		// docs/STDIO-MANAGED-AGENTS.md documents a session created idle,
		// with no turn started, as accepted. This build always starts the
		// first turn at create instead — see "Deviations", which this phase
		// adds a line to for the departure. A client wanting the documented
		// shape today posts user.message through sessions.events immediately
		// after create instead of naming it in initial_events.
		return nil, errorf(CodeUnsupported, "initial_events: this build requires exactly one user.message at create; a session created idle with no turn is not built yet (docs/STDIO-MANAGED-AGENTS.md, Deviations)")
	}
	if len(p.InitialEvents) > 1 {
		return nil, errorf(CodeInvalidParams, "initial_events: at most one user.message is accepted")
	}
	prompt, rerr := maDecodeInitialEvent(p.InitialEvents[0])
	if rerr != nil {
		return nil, rerr
	}

	return &CreateRequest{
		Model:        model,
		Prompt:       prompt,
		Effort:       effort,
		ResultSchema: p.Harness.ResultSchema,
		Tools:        p.Tools,
		Stream:       p.Stream,
		Harness:      p.Harness.toCreateHarness(),
	}, nil
}

// maDecodeAgent reads the create body's `agent` field, accepted in exactly
// one shape (docs/STDIO-MANAGED-AGENTS.md, "The `agent` field"). Absent, it
// answers "", "", nil: the create names no model or effort override and the
// server falls back to its own default model.
func maDecodeAgent(raw json.RawMessage) (model, effort string, rerr *rpcError) {
	if len(raw) == 0 {
		return "", "", nil
	}
	trimmed := strings.TrimSpace(string(raw))
	if len(trimmed) > 0 && trimmed[0] == '"' {
		return "", "", errorf(CodeUnsupported, "agent: a bare agent id names a stored agent resource, which this process has none of; send {\"type\":\"agent_with_overrides\",\"model\":{...}} or omit agent entirely")
	}
	var spec maAgentSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", "", errorf(CodeInvalidParams, "agent: %v", err)
	}
	if spec.Type != maAgentWithOverrides {
		return "", "", errorf(CodeUnsupported, "agent.type %q: only %q is accepted, because there is no agent resource for any other form to reference", spec.Type, maAgentWithOverrides)
	}
	if len(spec.ID) > 0 || len(spec.Version) > 0 {
		return "", "", errorf(CodeUnsupported, "agent_with_overrides.id/.version: there is no agent resource to override")
	}
	if len(spec.System) > 0 {
		return "", "", errorf(CodeUnsupported, "agent_with_overrides.system: this harness's own system prompt is frozen for the session's life")
	}
	if len(spec.Tools) > 0 || len(spec.MCPServers) > 0 || len(spec.Skills) > 0 {
		return "", "", errorf(CodeUnsupported, "agent_with_overrides.tools/.mcp_servers/.skills: this process's tools come from the create body's own tools field and the harness's built-ins, not from an agent resource")
	}
	if spec.Model == nil {
		return "", "", nil
	}
	if len(spec.Model.InferenceGeo) > 0 {
		return "", "", errorf(CodeUnsupported, "model.inference_geo: this process calls one fixed Anthropic endpoint")
	}
	return spec.Model.ID, spec.Model.Effort, nil
}

// maDecodeInitialEvent reads the one accepted member of initial_events: a
// user.message whose content is text alone.
func maDecodeInitialEvent(raw json.RawMessage) (string, *rpcError) {
	var e maWireEvent
	if err := json.Unmarshal(raw, &e); err != nil {
		return "", errorf(CodeInvalidParams, "initial_events: %v", err)
	}
	if e.Type != maEventUserMessage {
		return "", errorf(CodeUnsupported, "initial_events: only %q is accepted; %q is refused here", maEventUserMessage, e.Type)
	}
	return maContentText(e.Content)
}

// maContentText flattens content blocks to the text the loop takes. An image
// or document block is refused rather than dropped: this process reads
// files off its own filesystem with its own tools.
func maContentText(blocks []maContent) (string, *rpcError) {
	var parts []string
	for _, c := range blocks {
		switch c.Type {
		case "text":
			parts = append(parts, c.Text)
		case "image", "document":
			return "", errorf(CodeUnsupported, "a %s content block is not read; this session reads files off its own filesystem with the Read tool", c.Type)
		default:
			return "", errorf(CodeInvalidParams, "content block type %q", c.Type)
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

// DecodeAppend is never reached: Methods leaves Append unset, and
// sessions.events — the one wire method this dialect answers user.message
// on — is handled by Server.events rather than by Server.append.
func (ManagedAgents) DecodeAppend(params json.RawMessage) (*AppendRequest, *rpcError) {
	return nil, errorf(CodeMethodNotFound, "this dialect has no responses.append/interactions.append; post user.message through sessions.events")
}

func (ManagedAgents) DecodeID(method string, params json.RawMessage) (string, *rpcError) {
	var p maIDParams
	if err := json.Unmarshal(params, &p); err != nil {
		return "", errorf(CodeInvalidParams, "%s params: %v", method, err)
	}
	if p.Harness != nil && p.Harness.TurnID != "" {
		return p.Harness.TurnID, nil
	}
	return p.SessionID, nil
}

func (ManagedAgents) Initialize(h Handshake) any {
	details := make([]maModelDetail, 0, len(h.Details))
	for _, d := range h.Details {
		details = append(details, maModelDetail{
			ID:                  d.ID,
			DisplayName:         d.DisplayName,
			ContextWindowTokens: d.ContextWindowTokens,
			EffortLevels:        d.Efforts,
		})
	}
	return maInitializeResult{
		ServerInfo: ServerInfo{
			Name:     h.Name,
			Version:  h.Version,
			Protocol: ManagedAgents{}.Protocol(),
		},
		Capabilities: maServerCapabilities{
			Streaming:       true,
			Events:          true,
			Interrupt:       true,
			ResumeSession:   true,
			MCPServers:      h.MCPServers,
			CustomTools:     true,
			PermissionModes: h.PermissionModes,
		},
		Models:       h.Models,
		DefaultModel: h.DefaultModel,
		ModelDetails: details,
	}
}

// AppendResult builds the sessions.events result row for a posted
// user.message — Server.events reads runID (really the turn id, addressed
// through AddressID) back out under Harness.TurnID and folds this row into
// its own {session_id, results:[...]} envelope.
func (ManagedAgents) AppendResult(runID string, seq int64) any {
	return maEventResultRow{Type: maEventUserMessage, Seq: seq, Harness: &maEventHarness{TurnID: runID}}
}

func (ManagedAgents) NewTranslator(runID, sessionID, model string, emit func(method string, params any)) Translator {
	return newMATranslator(runID, sessionID, model, emit)
}

// CallParams is never reached: a client-declared tool crosses as
// agent.custom_tool_use, a notification the translator builds directly from
// the tool-call event log, not as a call this side sends and waits on
// (docs/STDIO-MANAGED-AGENTS.md, "Client-declared (custom) tools"). It is
// implemented to satisfy Dialect rather than left to panic.
func (ManagedAgents) CallParams(c FunctionCall) any {
	return maCustomToolUse{ID: c.CallID, Name: c.Name, Input: toolArguments(c.Arguments)}
}

// CallContent reads a user.custom_tool_result's content array the same way
// Interactions reads a function_result's: text concatenated, an image split
// out of its mime type and base64 payload.
func (ManagedAgents) CallContent(raw json.RawMessage) (tools.MCPContent, error) {
	var e maWireEvent
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &e); err != nil {
			return tools.MCPContent{}, err
		}
	}
	out := tools.MCPContent{IsError: e.IsError}
	for _, c := range e.Content {
		switch c.Type {
		case "text":
			if out.Text != "" {
				out.Text += "\n"
			}
			out.Text += c.Text
		case "image":
			data, err := decodeBase64(c.Data)
			if err != nil {
				continue
			}
			out.Images = append(out.Images, tools.MCPImage{MIMEType: c.MIMEType, Data: data})
		}
	}
	return out, nil
}
