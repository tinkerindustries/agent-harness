package mcp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// minFetchWait is the floor under any Fetch wait this package issues, even
// when a caller asks for wait_ms 0: a Fetch needs a non-zero window to
// notice a message that is already sitting in the stream, and zero would
// make an already-finished run look queued.
const minFetchWait = 300 * time.Millisecond

type collectInput struct {
	RequestID string `json:"request_id" jsonschema:"The request_id returned by deepseek_agent."`
	WaitMS    int64  `json:"wait_ms,omitempty" jsonschema:"How long to wait for the run to finish before falling back to a running/queued status, in milliseconds. Capped by the server."`
}

func (svc *Service) registerCollectTool(server *mcpsdk.Server) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "deepseek_result",
		Description: "Collect a deepseek-harness run's result by request_id. Safe to call more than once: it only " +
			"reads the RESULTS stream and the harness's read-only API, never starts or steers anything. If the run " +
			"has already finished — even long ago, even from a different process — this returns the persisted final " +
			"result. Otherwise it reports the most recent progress, or that the request is still queued.",
	}, svc.handleCollect)
}

func (svc *Service) handleCollect(ctx context.Context, _ *mcpsdk.CallToolRequest, in collectInput) (*mcpsdk.CallToolResult, any, error) {
	if in.RequestID == "" {
		return errorResult("request_id is required"), nil, nil
	}

	wait := time.Duration(in.WaitMS) * time.Millisecond
	waitCap := time.Duration(svc.Cfg.CollectWaitCapMS) * time.Millisecond
	if waitCap > 0 && wait > waitCap {
		wait = waitCap
	}
	if wait < minFetchWait {
		wait = minFetchWait
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
	batch, err := finalConsumer.Fetch(1, jetstream.FetchMaxWait(wait))
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

	// No final yet. Fall back to the most recent progress message, if any.
	progConsumer, err := svc.JS.OrderedConsumer(ctx, queue.StreamResults, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{queue.ProgressSubject(in.RequestID)},
		DeliverPolicy:  jetstream.DeliverLastPolicy,
	})
	if err == nil {
		pbatch, err := progConsumer.Fetch(1, jetstream.FetchMaxWait(minFetchWait))
		if err == nil {
			for msg := range pbatch.Messages() {
				var p queue.Progress
				if json.Unmarshal(msg.Data(), &p) == nil {
					svc.Registry.updateStatus(in.RequestID, "running", p.SessionID, "")
					return renderRunning(svc.Cfg.HarnessPublicURL, in.RequestID, p), nil, nil
				}
			}
		}
	}

	return renderQueued(in.RequestID), nil, nil
}
