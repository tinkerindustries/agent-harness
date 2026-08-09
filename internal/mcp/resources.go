package mcp

import (
	"context"
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/hub"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

func (svc *Service) registerResources(server *mcpsdk.Server) {
	server.AddResource(&mcpsdk.Resource{
		URI:         "harness://sessions",
		Name:        "sessions",
		Description: "The harness's session list (proxies GET /api/sessions).",
		MIMEType:    "application/json",
	}, svc.readSessions)

	server.AddResourceTemplate(&mcpsdk.ResourceTemplate{
		URITemplate: "harness://session/{id}/transcript",
		Name:        "session-transcript",
		Description: "One session's event log, rendered as readable markdown.",
		MIMEType:    "text/markdown",
	}, svc.readSessionTranscript)
}

func (svc *Service) readSessions(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	body, err := getRaw(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, "/api/sessions")
	if err != nil {
		return nil, fmt.Errorf("proxy GET /api/sessions: %w", err)
	}
	return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{
		{URI: req.Params.URI, MIMEType: "application/json", Text: string(body)},
	}}, nil
}

// sessionIDFromTranscriptURI extracts <id> from harness://session/<id>/transcript.
func sessionIDFromTranscriptURI(uri string) (string, bool) {
	const prefix = "harness://session/"
	const suffix = "/transcript"
	if !strings.HasPrefix(uri, prefix) || !strings.HasSuffix(uri, suffix) {
		return "", false
	}
	id := strings.TrimSuffix(strings.TrimPrefix(uri, prefix), suffix)
	if id == "" {
		return "", false
	}
	return id, true
}

func (svc *Service) readSessionTranscript(ctx context.Context, req *mcpsdk.ReadResourceRequest) (*mcpsdk.ReadResourceResult, error) {
	id, ok := sessionIDFromTranscriptURI(req.Params.URI)
	if !ok {
		return nil, mcpsdk.ResourceNotFoundError(req.Params.URI)
	}

	var sess hub.SessionState
	if err := getJSON(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, "/api/sessions/"+id, &sess); err != nil {
		return nil, fmt.Errorf("get session %s: %w", id, err)
	}

	events, err := svc.fetchAllEvents(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("get events for %s: %w", id, err)
	}

	md := renderTranscriptMarkdown(sess, events)
	return &mcpsdk.ReadResourceResult{Contents: []*mcpsdk.ResourceContents{
		{URI: req.Params.URI, MIMEType: "text/markdown", Text: md},
	}}, nil
}

// eventsPage mirrors internal/httpapi's unexported wire shape for
// GET /api/sessions/{id}/events. It is redefined here rather than imported
// because that type is a private implementation detail of the HTTP handler,
// not a shared wire type like the ones in internal/queue; store.Event, which
// carries the actual per-event payload, is reused as-is.
type eventsPage struct {
	Events []store.Event `json:"events"`
	Next   *int64        `json:"next,omitempty"`
}

// maxTranscriptPages bounds how many /events pages this fetches for one
// resource read, so a pathologically long session cannot turn a resource
// read into an unbounded fetch loop.
const maxTranscriptPages = 20

// fetchAllEvents pages through GET /api/sessions/{id}/events until the
// server stops returning a next cursor.
func (svc *Service) fetchAllEvents(ctx context.Context, id string) ([]store.Event, error) {
	var all []store.Event
	from := int64(0)
	for page := 0; page < maxTranscriptPages; page++ {
		var p eventsPage
		path := fmt.Sprintf("/api/sessions/%s/events?from=%d&limit=5000", id, from)
		if err := getJSON(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, path, &p); err != nil {
			return nil, err
		}
		all = append(all, p.Events...)
		if p.Next == nil {
			break
		}
		from = *p.Next
	}
	return all, nil
}
