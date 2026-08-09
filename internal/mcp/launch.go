package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// launchInput is deepseek_agent's argument set (see the tool description in
// registerLaunchTool for what each field means to a caller). ResultSchema is
// typed any, not json.RawMessage: the jsonschema-go inference the SDK uses
// to build the tool's input schema renders json.RawMessage as a byte array,
// which is wrong for a field that must accept an arbitrary JSON Schema
// object.
type launchInput struct {
	Description    string `json:"description" jsonschema:"Short label for the run, shown in deepseek_runs."`
	Prompt         string `json:"prompt" jsonschema:"The task for the agent to perform."`
	Workspace      string `json:"workspace" jsonschema:"A workspace NAME, not a path. See the harness://workspaces resource for valid names."`
	Profile        string `json:"profile,omitempty" jsonschema:"pro (default: the harness's main-loop model) or flash (deepseek-v4-flash, high effort)."`
	PermissionMode string `json:"permission_mode,omitempty" jsonschema:"readonly, default, or full. Clamped to this server's configured permission ceiling."`
	ResultSchema   any    `json:"result_schema,omitempty" jsonschema:"JSON Schema the agent's Complete tool result must satisfy, if it calls Complete with a result."`
	MaxSubTurns    int    `json:"max_sub_turns,omitempty" jsonschema:"Sub-turn budget for the run. Server default applies when omitted."`
	DeadlineMS     int64  `json:"deadline_ms,omitempty" jsonschema:"Wall-clock deadline for the run, in milliseconds. Server default applies when omitted."`
}

// launchOutput is deepseek_agent's structured content: the handle a caller
// needs to collect the run later, machine-readable without parsing the text
// footer.
type launchOutput struct {
	RequestID     string `json:"request_id"`
	Status        string `json:"status"` // "queued" or "running"
	SessionID     string `json:"session_id,omitempty"`
	TranscriptURL string `json:"transcript_url,omitempty"`
}

func (svc *Service) registerLaunchTool(server *mcpsdk.Server) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "deepseek_agent",
		Description: "Launch a deepseek-harness agent session. Publishes a work request to the harness's " +
			"NATS work queue and returns immediately with a handle — it does not wait for the run to " +
			"finish. Use deepseek_result to collect the outcome.",
	}, svc.handleLaunch)
}

func (svc *Service) handleLaunch(ctx context.Context, _ *mcpsdk.CallToolRequest, in launchInput) (*mcpsdk.CallToolResult, any, error) {
	if in.Description == "" {
		return errorResult("description is required"), nil, nil
	}
	if in.Prompt == "" {
		return errorResult("prompt is required"), nil, nil
	}

	workspace, err := resolveWorkspaceName(svc.Cfg.WorkspaceRoots, in.Workspace)
	if err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	model, effort, err := resolveProfile(in.Profile, svc.Cfg.FlashModel)
	if err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	mode, err := resolvePermissionMode(in.PermissionMode, tools.Mode(svc.Cfg.PermissionCeiling))
	if err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	var resultSchema json.RawMessage
	if in.ResultSchema != nil {
		resultSchema, err = json.Marshal(in.ResultSchema)
		if err != nil {
			return errorResult("result_schema: %v", err), nil, nil
		}
	}

	requestID := newRequestID()
	req := queue.Request{
		RequestID:      requestID,
		Prompt:         in.Prompt,
		Workspace:      workspace,
		Model:          model,
		Effort:         effort,
		PermissionMode: string(mode),
		ResultSchema:   resultSchema,
		MaxSubTurns:    in.MaxSubTurns,
		DeadlineMS:     in.DeadlineMS,
	}
	// Reuses the harness's own request validation (docs/DESIGN.md §4.10)
	// rather than re-implementing it, so a request this accepts is
	// guaranteed to pass the worker pool's validation too.
	if _, err := req.Validate(svc.Cfg.WorkspaceRoots); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	data, err := json.Marshal(req)
	if err != nil {
		return errorResult("encode work request: %v", err), nil, nil
	}

	// Subscribe to the accepted subject before publishing (the way
	// cmd/harness/publish.go does for the final subject), so a worker that
	// picks this request up immediately cannot be missed.
	subCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	acceptedConsumer, err := svc.JS.OrderedConsumer(subCtx, queue.StreamResults, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{queue.AcceptedSubject(requestID)},
	})
	cancel()
	if err != nil {
		return errorResult("subscribe for accepted: %v", err), nil, nil
	}

	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = svc.JS.Publish(pubCtx, queue.RequestSubject(requestID), data)
	cancel()
	if err != nil {
		// The one genuine launch error (docs/DESIGN.md's three outcomes):
		// everything before this point was validation, and everything
		// after it is a normal "the run has not been picked up yet".
		return errorResult("publish work request: %v", err), nil, nil
	}

	svc.Registry.recordLaunch(runRecord{
		RequestID:   requestID,
		Description: in.Description,
		Workspace:   in.Workspace,
		Profile:     in.Profile,
		LaunchedAt:  time.Now().UTC(),
		Status:      "queued",
		UpdatedAt:   time.Now().UTC(),
	})

	status, sessionID := "queued", ""
	waitMS := svc.Cfg.AcceptedWaitMS
	if waitMS <= 0 {
		waitMS = 1
	}
	batch, err := acceptedConsumer.Fetch(1, jetstream.FetchMaxWait(time.Duration(waitMS)*time.Millisecond))
	if err == nil {
		for msg := range batch.Messages() {
			var accepted queue.Accepted
			if json.Unmarshal(msg.Data(), &accepted) == nil {
				status, sessionID = "running", accepted.SessionID
			}
		}
	}
	// A timeout with nothing delivered is not an error (docs/DESIGN.md):
	// the request is legitimately still queued behind a full worker pool.
	// batch.Error() is deliberately not inspected here.

	if status == "running" {
		svc.Registry.updateStatus(requestID, "running", sessionID, "")
	}

	out := launchOutput{RequestID: requestID, Status: status, SessionID: sessionID, TranscriptURL: transcriptURL(svc.Cfg.HarnessPublicURL, sessionID)}
	var b []byte
	if status == "running" {
		b = []byte(fmt.Sprintf("status: running\nrequest_id: %s\nsession_id: %s\ntranscript: %s\n",
			requestID, sessionID, out.TranscriptURL))
	} else {
		b = []byte(fmt.Sprintf("status: queued\nrequest_id: %s\nno accepted message within the wait window; the pool is likely full. Call deepseek_result with this request_id to check later.\n",
			requestID))
	}

	return &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: string(b)}},
		StructuredContent: out,
	}, nil, nil
}

// newRequestID generates a work request's idempotency key, prefixed to make
// its MCP origin obvious in logs and transcripts alongside cmd/harness
// publish's own "req-" prefix.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("mcp: crypto/rand unavailable: " + err.Error())
	}
	return "mcp-" + hex.EncodeToString(b[:])
}
