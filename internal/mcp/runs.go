package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// runsInput is empty: deepseek_runs takes no arguments, it just reports
// what this process remembers launching.
type runsInput struct{}

func (svc *Service) registerRunsTool(server *mcpsdk.Server) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "deepseek_runs",
		Description: "List the agent-harness runs this MCP server has launched and their last known state. " +
			"State is a snapshot from the last time deepseek_agent or deepseek_result touched each entry, not a " +
			"live query — call deepseek_result to refresh one. This list is this process's own memory, not the " +
			"harness's full history; see harness://sessions for that.",
	}, svc.handleRuns)
}

func (svc *Service) handleRuns(ctx context.Context, _ *mcpsdk.CallToolRequest, _ runsInput) (*mcpsdk.CallToolResult, any, error) {
	records := svc.Registry.list()
	if len(records) == 0 {
		return &mcpsdk.CallToolResult{Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: "no runs launched yet"}}}, nil, nil
	}

	var b strings.Builder
	for _, rec := range records {
		fmt.Fprintf(&b, "%s  %-10s %s", rec.RequestID, rec.Status, rec.Description)
		if rec.SessionID != "" {
			fmt.Fprintf(&b, "  session=%s", rec.SessionID)
		}
		if rec.CompleteStatus != "" {
			fmt.Fprintf(&b, "  complete_status=%s", rec.CompleteStatus)
		}
		b.WriteString("\n")
	}

	return &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: b.String()}},
		StructuredContent: records,
	}, nil, nil
}
