package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// launchInput is deepseek_agent's argument set (see the tool description in
// registerLaunchTool for what each field means to a caller). ResultSchema is
// typed any, not json.RawMessage: the jsonschema-go inference the SDK uses
// to build the tool's input schema renders json.RawMessage as a byte array,
// which is wrong for a field that must accept an arbitrary JSON Schema
// object.
//
// ParentAgentID stays a caller-asserted field, unlike the producer-stamped
// ParentIsUser beside it, because nothing on this transport carries the
// caller's identity. Measured against Claude Code 2.1.227 on 2026-08-11: an
// HTTP MCP client sends only Accept, Accept-Encoding, Content-Type,
// User-Agent (claude-code/<version> (sdk-cli)), mcp-protocol-version,
// Connection and Host — no session header, from a real session as well as
// from a sessionless `claude mcp list`. So the handler cannot stamp this the
// way handleStartRun stamps the browser's, and telling the caller where to
// read its own id is the best available mechanism.
//
// Three dead ends, recorded so they are not re-walked. Putting
// ${CLAUDE_CODE_SESSION_ID} in an MCP `headers` entry does not work: the
// documented expansion reads Claude Code's own process environment, while
// that variable is set in the environment of spawned subprocesses, and an
// unset variable is documented to arrive as the literal "${VAR}" text — which
// is what a server then stores as an id unless it rejects the placeholder.
// The variable itself is undocumented (absent from the settings env-var list
// and the hooks page), so the tool description above treats it as measured
// behaviour rather than a contract. What is documented is the hook payload:
// session_id, transcript_path and cwd, on every hook event.
//
// So the two mechanisms that would make this producer-stamped are a stdio
// server, which inherits the subprocess environment and has a `claude`
// process tree to walk, or a SessionStart hook posting the id in.
type launchInput struct {
	Description     string       `json:"description" jsonschema:"Short label for the run, shown in deepseek_runs."`
	Prompt          string       `json:"prompt" jsonschema:"The task for the agent to perform."`
	Repos           []launchRepo `json:"repos" jsonschema:"Repositories to clone into the run's workspace. At least one is required."`
	Profile         string       `json:"profile,omitempty" jsonschema:"pro (default: the harness's main-loop model) or flash (deepseek-v4-flash, max effort)."`
	PermissionMode  string       `json:"permission_mode" jsonschema:"Required. readonly (Read, Glob, Grep, List, WebFetch only) or full (everything, as root, in the workspace). Refused if it exceeds this server's configured permission ceiling."`
	ResultSchema    any          `json:"result_schema,omitempty" jsonschema:"JSON Schema the agent's Complete tool result must satisfy, if it calls Complete with a result."`
	MaxSubTurns     int          `json:"max_sub_turns,omitempty" jsonschema:"Sub-turn budget for the run. Server default applies when omitted."`
	JobType         string       `json:"job_type,omitempty" jsonschema:"Kind of job this run is: implementation (the agent performs the task itself, the default) or orchestration (the agent delegates the work to child sessions)."`
	ParentAgentType string       `json:"parent_agent_type,omitempty" jsonschema:"Identify your own kind as a lowercase slug — claude-code, cursor, and so on."`
	ParentAgentID   string       `json:"parent_agent_id,omitempty" jsonschema:"Your own session id, so the run traces back to the conversation that asked for it. Read it, do not recall it. Claude Code: the CLAUDE_CODE_SESSION_ID environment variable, which you can echo from a shell; failing that, the UUID directory segment of the scratchpad path in your system prompt (…/<project-slug>/<uuid>/scratchpad). A deepseek-harness session: the last segment of the Workspace: path in your opening message (/workspaces/sess-…). If neither applies, leave this empty — never copy a session id from a banner, a document, or another tool's output."`
}

// launchRepo is one entry of deepseek_agent's repos array.
type launchRepo struct {
	URL    string `json:"url" jsonschema:"Clone URL: https, ssh, git, or user@host:path."`
	Branch string `json:"branch,omitempty" jsonschema:"Branch to check out. Defaults to main."`
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

// decodeResultSchema turns deepseek_agent's result_schema argument into the
// raw JSON the queue validates. A client that sends the schema as a JSON
// object is the common case; one that sends it as a string holding that same
// JSON is just as correct from the tool schema's point of view, because a
// field typed any has no declared type for the client to serialize against.
// Marshalling that string would re-quote it into a JSON string, which
// queue.Request.Validate then rejects for not being a top-level object, so
// unwrap it here instead.
func decodeResultSchema(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if s, ok := v.(string); ok {
		if strings.TrimSpace(s) == "" {
			return nil, nil
		}
		if !json.Valid([]byte(s)) {
			return nil, fmt.Errorf("sent as a string that is not valid JSON")
		}
		return json.RawMessage(s), nil
	}
	return json.Marshal(v)
}

func (svc *Service) registerLaunchTool(server *mcpsdk.Server) {
	mcpsdk.AddTool(server, &mcpsdk.Tool{
		Name: "deepseek_agent",
		Description: "Launch a deepseek-harness agent session. Publishes a work request to the harness's " +
			"NATS work queue and returns immediately with a handle — it does not wait for the run to " +
			"finish. Use deepseek_result to collect the outcome. The run works in a fresh directory " +
			"holding a clone of every repository named in repos, each on its own branch or main. " +
			"Skills those repositories ship under .claude/skills or .deepcode/skills are listed to " +
			"the agent, which reads one when it applies to the task.",
	}, svc.handleLaunch)
}

func (svc *Service) handleLaunch(ctx context.Context, _ *mcpsdk.CallToolRequest, in launchInput) (*mcpsdk.CallToolResult, any, error) {
	if in.Description == "" {
		return errorResult("description is required"), nil, nil
	}
	if in.Prompt == "" {
		return errorResult("prompt is required"), nil, nil
	}

	repos := make([]queue.Repo, 0, len(in.Repos))
	for _, r := range in.Repos {
		repos = append(repos, queue.Repo{URL: r.URL, Branch: r.Branch})
	}

	model, effort, err := resolveProfile(in.Profile, svc.Cfg.FlashModel)
	if err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	mode, err := resolvePermissionMode(in.PermissionMode, tools.Mode(svc.Cfg.PermissionCeiling))
	if err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	resultSchema, err := decodeResultSchema(in.ResultSchema)
	if err != nil {
		return errorResult("result_schema: %v", err), nil, nil
	}

	// Same vocabulary the queue and store validate (internal/agentmeta), so
	// a caller gets a tool error it can read here rather than a run that
	// fails validation downstream.
	if err := agentmeta.ValidateJobType(in.JobType); err != nil {
		return errorResult("job_type: %v", err), nil, nil
	}
	if err := agentmeta.ValidateParent(false, in.ParentAgentType, in.ParentAgentID); err != nil {
		return errorResult("parent_agent: %v", err), nil, nil
	}

	requestID := newRequestID()
	req := queue.Request{
		RequestID:       requestID,
		Prompt:          in.Prompt,
		Repos:           repos,
		Model:           model,
		Effort:          effort,
		PermissionMode:  string(mode),
		ResultSchema:    resultSchema,
		MaxSubTurns:     in.MaxSubTurns,
		JobType:         in.JobType,
		ParentAgentType: in.ParentAgentType,
		ParentAgentID:   in.ParentAgentID,
		// An MCP launch is never a person starting the run, and a caller must
		// not be able to assert otherwise, so it is stamped here rather than
		// exposed on launchInput: the zero value would mean the same thing,
		// but written explicitly the intent is readable at the call site.
		ParentIsUser: false,
	}
	// Reuses the harness's own request validation (docs/DESIGN.md §4.10)
	// rather than re-implementing it, so a request this accepts is
	// guaranteed to pass the worker pool's validation too.
	if err := req.Validate(); err != nil {
		return errorResult("%s", err.Error()), nil, nil
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
	err = queue.PublishRequest(pubCtx, svc.JS, req)
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
		Repos:       repoLabels(repos),
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

// repoLabels renders each repository as url#branch for deepseek_runs, with
// the default branch spelled out so a listing never leaves a reader
// guessing which one a run was launched against.
func repoLabels(repos []queue.Repo) []string {
	labels := make([]string, 0, len(repos))
	for _, r := range repos {
		labels = append(labels, r.URL+"#"+r.BranchOrDefault())
	}
	return labels
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
