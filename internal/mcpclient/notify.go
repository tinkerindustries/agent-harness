package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"sync/atomic"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/store"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// A server talks back to its client in four ways that are not a reply to a
// request: progress on a call still running, log messages, notices that its
// tool/prompt/resource lists have changed, and notices that a subscribed
// resource has been updated. This file is where all four land
// (docs/MCP.md, "What a server sends back unasked").
//
// The routing problem they share is that a notification arrives on the
// connection, which this Manager shares between every session in the
// process, while the thing worth telling about it — the transcript — is
// per session. Only progress carries the token needed to bridge that: it
// names the call it belongs to, so a progress notification can be put in
// front of the model that is waiting on that very call. The other three
// carry no such correlation, so they go to the harness log under the
// server's name, beside the stderr that stdio servers already write there.

// logLevel is the severity floor requested from every server that supports
// logging. "info" rather than "debug" because these lines cost the operator
// log volume for every session in the process, and a server's debug stream
// is written for someone reading one server, not for a log shared by all of
// them.
const logLevel = "info"

// progressToken mints the tokens Call attaches to a tool call so the
// notifications a server sends while running it can be matched back to the
// caller waiting on it. Process-wide and monotonic: uniqueness is all that
// is asked of it.
var progressToken atomic.Uint64

// clientOptions builds the handler set every connection to srv carries.
func (m *Manager) clientOptions(srv store.MCPServer) *mcpsdk.ClientOptions {
	return &mcpsdk.ClientOptions{
		// Sampling and elicitation reach these handlers through the SDK's
		// multi-round-trip middleware rather than as standalone requests
		// (sampling.go). srv is the row as it was when this connection was
		// dialled, which is what makes AllowSampling a decision the
		// operator made before the server could ask, rather than one
		// resolved in the middle of answering it.
		CreateMessageWithToolsHandler: func(ctx context.Context, req *mcpsdk.CreateMessageWithToolsRequest) (*mcpsdk.CreateMessageWithToolsResult, error) {
			res, err := m.createMessage(ctx, srv, req)
			if err != nil {
				return nil, err
			}
			return &mcpsdk.CreateMessageWithToolsResult{
				Model: res.Model, Role: res.Role, Content: []mcpsdk.Content{res.Content},
				StopReason: res.StopReason,
			}, nil
		},
		ElicitationHandler: func(ctx context.Context, req *mcpsdk.ElicitRequest) (*mcpsdk.ElicitResult, error) {
			return m.elicit(ctx, srv, req)
		},
		ProgressNotificationHandler: func(_ context.Context, req *mcpsdk.ProgressNotificationClientRequest) {
			m.onProgress(srv.Name, req)
		},
		LoggingMessageHandler: func(_ context.Context, req *mcpsdk.LoggingMessageRequest) {
			if req == nil || req.Params == nil {
				return
			}
			log.Printf("mcpclient: %s: [%s] %s", srv.Name, req.Params.Level, renderLogData(req.Params.Data))
		},
		ToolListChangedHandler: func(_ context.Context, _ *mcpsdk.ToolListChangedRequest) {
			m.onListChanged(srv.Name, "tools")
		},
		PromptListChangedHandler: func(_ context.Context, _ *mcpsdk.PromptListChangedRequest) {
			m.onListChanged(srv.Name, "prompts")
		},
		ResourceListChangedHandler: func(_ context.Context, _ *mcpsdk.ResourceListChangedRequest) {
			m.onListChanged(srv.Name, "resources")
		},
		ResourceUpdatedHandler: func(_ context.Context, req *mcpsdk.ResourceUpdatedNotificationRequest) {
			if req == nil || req.Params == nil {
				return
			}
			log.Printf("mcpclient: %s: resource updated: %s", srv.Name, req.Params.URI)
		},
	}
}

// setLogLevel asks a freshly connected server to send log messages at
// logLevel and above. A server that does not advertise the logging
// capability answers with an error, which is not a problem worth failing a
// dial over — most servers have nothing to say.
func (m *Manager) setLogLevel(ctx context.Context, srv store.MCPServer, sess *mcpsdk.ClientSession) {
	res := sess.InitializeResult()
	if res == nil || res.Capabilities == nil || res.Capabilities.Logging == nil {
		return
	}
	if err := sess.SetLoggingLevel(ctx, &mcpsdk.SetLoggingLevelParams{Level: logLevel}); err != nil {
		log.Printf("mcpclient: %s: set logging level: %v", srv.Name, err)
	}
}

// onProgress delivers one progress notification to whoever is waiting on
// the call it names, and to the harness log when nobody is — a server that
// reports progress against a token this process never issued, or against a
// call that has already answered.
//
// That second case is ordinary rather than exceptional. A server may send
// progress right up to the moment it replies, and the reply can reach the
// caller while the last notification is still queued behind it; the sink is
// released when the call returns, so the straggler goes to the log. Holding
// the sink open longer to catch it would put a line about work in progress
// into a transcript that has already shown the work finishing.
func (m *Manager) onProgress(server string, req *mcpsdk.ProgressNotificationClientRequest) {
	if req == nil || req.Params == nil {
		return
	}
	line := renderProgress(server, req.Params)
	sink := m.progressSink(tokenKey(req.Params.ProgressToken))
	if sink == nil {
		log.Printf("mcpclient: %s", line)
		return
	}
	sink(line + "\n")
}

// onListChanged records that a server's advertised lists have moved on
// since the snapshot in its row. Nothing re-probes here: a run's tool array
// is frozen for the life of the run on purpose (docs/MCP.md, "The tool
// array is built from a stored snapshot"), and changing it underneath a
// session in flight would invalidate the prompt-cache prefix every request
// of that run shares. Marking the row stale is what the /mcp screen reads
// to tell the operator a Refresh is worth pressing.
func (m *Manager) onListChanged(server, what string) {
	log.Printf("mcpclient: %s: %s list changed upstream; the stored snapshot is now stale", server, what)
	if m.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), probeWriteTimeout)
	defer cancel()
	if err := m.Store.SetMCPServerStale(ctx, server, true); err != nil {
		log.Printf("mcpclient: %s: mark stale: %v", server, err)
	}
}

// progressSink returns the live-output callback registered for token, or
// nil when there is none.
func (m *Manager) progressSink(token string) func(string) {
	if token == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sinks[token]
}

// registerSink attaches sink to a freshly minted progress token and returns
// the token with the function that releases it. The release is what keeps
// the map bounded: one entry exists only while one call is in flight.
func (m *Manager) registerSink(sink func(string)) (string, func()) {
	token := "dsh-" + strconv.FormatUint(progressToken.Add(1), 10)
	m.mu.Lock()
	if m.sinks == nil {
		m.sinks = map[string]func(string){}
	}
	m.sinks[token] = sink
	m.mu.Unlock()
	return token, func() {
		m.mu.Lock()
		delete(m.sinks, token)
		m.mu.Unlock()
	}
}

// callSink returns the live-output callback the executor attached to ctx
// for the call in flight, or nil when this call has none — a Refresh probe,
// a CLI run with no hub, or any path that is not a model's tool call.
func callSink(ctx context.Context) func(string) {
	return tools.StdoutSinkFrom(ctx)
}

// renderProgress turns one notification into the line a reader sees.
// Percentage when the server said what the total was, a bare count when it
// did not, and the server's own message when it sent one — which is the
// only part with any content in it, so it goes last where a wrapped line
// still shows it.
func renderProgress(server string, p *mcpsdk.ProgressNotificationParams) string {
	var progress string
	switch {
	case p.Total > 0:
		progress = fmt.Sprintf("%.0f%%", 100*p.Progress/p.Total)
	default:
		progress = strconv.FormatFloat(p.Progress, 'f', -1, 64)
	}
	if p.Message == "" {
		return fmt.Sprintf("[%s] %s", server, progress)
	}
	return fmt.Sprintf("[%s] %s — %s", server, progress, p.Message)
}

// renderLogData flattens a log message's payload, which the protocol allows
// to be any JSON value at all: a bare string stays as it is rather than
// arriving at the log wrapped in quotes, and everything else is encoded.
func renderLogData(data any) string {
	if s, ok := data.(string); ok {
		return s
	}
	b, err := json.Marshal(data)
	if err != nil {
		return fmt.Sprintf("%v", data)
	}
	return string(b)
}

// tokenKey renders a progress token for use as a map key. The protocol
// allows a string or a number; this client only ever issues strings, so
// anything else came from a confused server and matches nothing.
func tokenKey(token any) string {
	s, _ := token.(string)
	return s
}
