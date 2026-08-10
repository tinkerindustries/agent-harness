package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// stopInput is deepseek_stop's argument set: a session_id or a request_id
// (resolved to its session the way deepseek_result resolves one, through the
// harness's own API), and an optional reason carried verbatim into the
// cancelled result.
type stopInput struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"The session_id of the run to stop, as returned by deepseek_agent or deepseek_status."`
	RequestID string `json:"request_id,omitempty" jsonschema:"Alternative to session_id: the request_id returned by deepseek_agent. Resolved to the session it produced."`
	Reason    string `json:"reason,omitempty" jsonschema:"Optional reason for the stop, recorded on the cancelled result so a reader knows why the run ended."`
}

// stopOutput is deepseek_stop's structured content: the acceptance the
// endpoint returned. It is an acceptance, not an outcome — the run is still
// ending, and its terminal state arrives later.
type stopOutput struct {
	SessionID string `json:"session_id"`
	Stopping  bool   `json:"stopping"`
}

// workRequestRow mirrors the part of GET /api/requests/{request_id}'s wire
// shape (internal/httpapi.workRequestRow) that request-to-session resolution
// needs: the row's id, the session it produced (absent for a request that
// never ran), and its status. Redefined here rather than imported, the same
// way statusResponse redefines the status endpoint's shape.
type workRequestRow struct {
	RequestID string `json:"request_id"`
	SessionID string `json:"session_id,omitempty"`
	Status    string `json:"status"`
}

func (svc *Service) registerStopTool(server *mcpsdk.Server) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "deepseek_stop",
		Description: "Stop a deepseek-harness run, by session_id or request_id. Posts to the harness's run-control " +
			"endpoint and returns immediately with whether the stop was accepted — it never blocks, and an accepted " +
			"stop means the run is ending, not that it has ended. A run may still finish on its own inside the " +
			"grace period, and stopping is irreversible. Call deepseek_status afterwards to see what actually " +
			"happened: the session's status moves to cancelled once the stop lands, whether the run gave up first " +
			"or the harness finished it without the goroutine.",
	}, svc.handleStop)
}

func (svc *Service) handleStop(ctx context.Context, _ *mcpsdk.CallToolRequest, in stopInput) (*mcpsdk.CallToolResult, any, error) {
	if in.SessionID == "" && in.RequestID == "" {
		return errorResult("session_id or request_id is required"), nil, nil
	}

	sessionID := in.SessionID
	if sessionID == "" {
		resolved, err := svc.resolveSession(ctx, in.RequestID)
		if err != nil {
			return errorResult("resolve request %s: %v", in.RequestID, err), nil, nil
		}
		sessionID = resolved
	}

	token, err := svc.controlToken(ctx)
	if err != nil {
		return errorResult("%v", err), nil, nil
	}

	var out stopOutput
	if err := postJSON(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, "/api/sessions/"+sessionID+"/stop", token,
		stopSessionRequest{Reason: in.Reason}, &out); err != nil {
		return errorResult("stop session %s: %v", sessionID, err), nil, nil
	}

	text := fmt.Sprintf("status: stopping\nsession_id: %s\nA stop was accepted for this run; it is ending. "+
		"It has not necessarily ended yet — call deepseek_status with the request_id to see the outcome.\n", sessionID)
	return &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		StructuredContent: out,
	}, nil, nil
}

// stopSessionRequest is the JSON body POST /api/sessions/{id}/stop accepts:
// the operator's reason, optional. Sent as a bare object when there is no
// reason, mirroring what the CLI sends.
type stopSessionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// resolveSession turns a request_id into the session it produced, through
// the same HTTP lookup deepseek_result's request-to-session resolution uses
// (GET /api/requests/{request_id}): the row carries the session the request
// ran as. A request that never produced a session — still queued, or failed
// before its session existed — cannot be stopped, and that is reported
// plainly rather than reaching for the stop endpoint with nothing to stop.
func (svc *Service) resolveSession(ctx context.Context, requestID string) (string, error) {
	var row workRequestRow
	if err := getJSON(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, "/api/requests/"+requestID, &row); err != nil {
		return "", err
	}
	if row.SessionID == "" {
		return "", fmt.Errorf("request %s has no session yet (status %s); nothing to stop", requestID, row.Status)
	}
	return row.SessionID, nil
}

// controlToken returns the bearer token deepseek_stop sends: the process's
// configured token when set, otherwise GET /api/control-token on the
// harness's own base URL (the MCP server normally runs on the same host, so
// the loopback route works). An empty token from either source is an error —
// a harness that never generated one must fail closed, never receive an
// empty bearer that would 503 anyway.
func (svc *Service) controlToken(ctx context.Context) (string, error) {
	if svc.Cfg.ControlToken != "" {
		return svc.Cfg.ControlToken, nil
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := getJSON(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, "/api/control-token", &out); err != nil {
		return "", fmt.Errorf("fetch control token: %w", err)
	}
	if out.Token == "" {
		return "", errors.New("the harness has no control token configured")
	}
	return out.Token, nil
}

// steerInput is deepseek_steer's argument set: a session_id or a request_id
// (resolved to its session exactly as deepseek_stop resolves one), and the
// text of the instruction, carried verbatim into the user message the model
// sees.
type steerInput struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"The session_id of the running run to steer, as returned by deepseek_agent or deepseek_status."`
	RequestID string `json:"request_id,omitempty" jsonschema:"Alternative to session_id: the request_id returned by deepseek_agent. Resolved to the session it produced."`
	Text      string `json:"text" jsonschema:"The instruction to append to the running run, verbatim. It is not a command the run stops to obey."`
}

// steerOutput is deepseek_steer's structured content: the acceptance the
// endpoint returned, including the seq the caller's text landed at. Steering
// is not idempotent — two steers are two instructions — so the seq is how a
// caller tells its own steer from any other.
type steerOutput struct {
	SessionID string `json:"session_id"`
	Seq       int64  `json:"seq"`
}

// registerSteerTool wires deepseek_steer into the server, beside
// deepseek_stop. The description has to carry the one thing an agent will
// otherwise get wrong: the run does not stop to read this, and it reaches
// the model at the next sub-turn boundary, which may be a minute or more away
// if a long tool call is in flight (docs/RUN-CONTROL.md "MCP and CLI").
func (svc *Service) registerSteerTool(server *mcpsdk.Server) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "deepseek_steer",
		Description: "Append an instruction to a running deepseek-harness run, by session_id or request_id. The run does NOT stop to read this: " +
			"the text is queued and reaches the model at the next sub-turn boundary, which may be a minute or more away if a long tool call is " +
			"in flight. It is an instruction the model sees as a new user message, not a command the harness executes. Returns immediately with " +
			"the acceptance and the seq the text landed at; call deepseek_status afterwards to see the run continue.",
	}, svc.handleSteer)
}

func (svc *Service) handleSteer(ctx context.Context, _ *mcpsdk.CallToolRequest, in steerInput) (*mcpsdk.CallToolResult, any, error) {
	if in.SessionID == "" && in.RequestID == "" {
		return errorResult("session_id or request_id is required"), nil, nil
	}
	if strings.TrimSpace(in.Text) == "" {
		return errorResult("text is required"), nil, nil
	}

	sessionID := in.SessionID
	if sessionID == "" {
		resolved, err := svc.resolveSession(ctx, in.RequestID)
		if err != nil {
			return errorResult("resolve request %s: %v", in.RequestID, err), nil, nil
		}
		sessionID = resolved
	}

	token, err := svc.controlToken(ctx)
	if err != nil {
		return errorResult("%v", err), nil, nil
	}

	var out steerOutput
	if err := postJSON(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, "/api/sessions/"+sessionID+"/steer", token,
		steerSessionRequest{Text: in.Text, Source: "mcp"}, &out); err != nil {
		return errorResult("steer session %s: %v", sessionID, err), nil, nil
	}

	text := fmt.Sprintf("status: accepted\nsession_id: %s\nseq: %d\nThe steer is queued and will reach the model at the next sub-turn boundary "+
		"— it does not stop the run to read it, so a long tool call in flight delays it by up to that call's whole remaining time.\n",
		sessionID, out.Seq)
	return &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		StructuredContent: out,
	}, nil, nil
}

// steerSessionRequest is the JSON body POST /api/sessions/{id}/steer accepts:
// the text verbatim plus the source naming where the steer came from.
type steerSessionRequest struct {
	Text   string `json:"text"`
	Source string `json:"source,omitempty"`
}
