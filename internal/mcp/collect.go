package mcp

import (
	"context"
	"encoding/json"
	"errors"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/queue"
)

type collectInput struct {
	RequestID string `json:"request_id" jsonschema:"The request_id returned by deepseek_agent."`
}

func (svc *Service) registerCollectTool(server *mcpsdk.Server) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "deepseek_result",
		Description: "Collect an agent-harness run's result by request_id. Never blocks: it reads the harness's " +
			"work-request row once and returns whatever is there at that moment. If the run has already finished — even " +
			"long ago, even from a different process — this returns the persisted final result: the row has no retention " +
			"window, so a result stays collectable for longer than the stream it used to be published to. Otherwise it " +
			"returns a short line pointing at deepseek_status, which reports where the run is up to.",
	}, svc.handleCollect)
}

func (svc *Service) handleCollect(ctx context.Context, _ *mcpsdk.CallToolRequest, in collectInput) (*mcpsdk.CallToolResult, any, error) {
	if in.RequestID == "" {
		return errorResult("request_id is required"), nil, nil
	}

	// One read of the work-request row (docs/QUEUE-MIGRATION-PLAN.md §5.1).
	// The row is created at claim time, so a 404 means the request is still
	// queued and has no result yet; a row whose result is empty means the run
	// has not finished (or failed before its session existed). Both are the
	// "no final result at this instant" answer, never an error.
	var row workRequestRow
	if err := getJSONOrNotFound(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, "/api/requests/"+in.RequestID, &row); err != nil {
		if errors.Is(err, errNotFound) {
			return renderPending(in.RequestID), nil, nil
		}
		return errorResult("read work request %s: %v", in.RequestID, err), nil, nil
	}
	if len(row.Result) == 0 {
		return renderPending(in.RequestID), nil, nil
	}

	// The row holds the stored queue.Result JSON, exactly the shape the
	// RESULTS stream used to carry (the wire contract is unchanged — the
	// worker writes the same bytes into the row it used to publish).
	var res queue.Result
	if err := json.Unmarshal(row.Result, &res); err != nil {
		return errorResult("decode final result: %v", err), nil, nil
	}
	svc.Registry.updateFromResult(in.RequestID, res)
	result := renderFinal(svc.Cfg.HarnessPublicURL, res)
	return result, result.StructuredContent, nil
}
