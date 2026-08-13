package mcp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// minFetchWait is the floor under every Fetch this package issues: a Fetch
// needs a non-zero window to notice a message that is already sitting in the
// stream, and zero would make an already-finished run look queued.
const minFetchWait = 300 * time.Millisecond

type collectInput struct {
	RequestID string `json:"request_id" jsonschema:"The request_id returned by deepseek_agent."`
}

func (svc *Service) registerCollectTool(server *mcpsdk.Server) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "deepseek_result",
		Description: "Collect an agent-harness run's result by request_id. Never blocks: it reads the RESULTS " +
			"stream once and returns whatever is there at that moment. If the run has already finished — even " +
			"long ago, even from a different process — this returns the persisted final result. Otherwise it " +
			"returns a short line pointing at deepseek_status, which reports where the run is up to.",
	}, svc.handleCollect)
}

func (svc *Service) handleCollect(ctx context.Context, _ *mcpsdk.CallToolRequest, in collectInput) (*mcpsdk.CallToolResult, any, error) {
	if in.RequestID == "" {
		return errorResult("request_id is required"), nil, nil
	}

	// The final subject first. An ordered consumer filtered on it replays
	// from the start of the RESULTS stream (default DeliverPolicy is
	// DeliverAllPolicy), and the stream retains 7 days
	// (docs/DESIGN.md §4.10), so this finds a result published long before
	// this call, from a process that was never listening at the time.
	finalConsumer, err := svc.JS.OrderedConsumer(ctx, queue.StreamResults, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{queue.FinalSubject(in.RequestID)},
	})
	if err != nil {
		return errorResult("subscribe for final result: %v", err), nil, nil
	}
	batch, err := finalConsumer.Fetch(1, jetstream.FetchMaxWait(minFetchWait))
	if err != nil {
		return errorResult("fetch final result: %v", err), nil, nil
	}
	for msg := range batch.Messages() {
		var res queue.Result
		if err := json.Unmarshal(msg.Data(), &res); err != nil {
			return errorResult("decode final result: %v", err), nil, nil
		}
		svc.Registry.updateFromResult(in.RequestID, res)
		result := renderFinal(svc.Cfg.HarnessPublicURL, res)
		return result, result.StructuredContent, nil
	}

	// No final result at this instant. The run may still be queued, mid-run,
	// or have failed before its session existed — deepseek_status says which.
	return renderPending(in.RequestID), nil, nil
}
