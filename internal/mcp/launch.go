package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrgeoffrich/agent-harness/internal/agentmeta"
	"github.com/mrgeoffrich/agent-harness/internal/attachment"
	"github.com/mrgeoffrich/agent-harness/internal/queue"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// launchInput is deepseek_agent's argument set (see the tool description in
// registerLaunchTool for what each field means to a caller). ResultSchema is
// typed any, not json.RawMessage: the jsonschema-go inference the SDK uses
// to build the tool's input schema renders json.RawMessage as a byte array,
// which is wrong for a field that must accept an arbitrary JSON Schema
// object.
//
// ParentAgentID stays a caller-asserted field, unlike the producer-stamped
// ParentIsUser beside it, because this transport carries the caller's kind,
// not its session id: clientInfo (name, title, version) arrives in the MCP
// initialize handshake from the client library itself, so the launch path
// stamps parent_agent_type from it producer-side, while no session id rides
// the wire. Measured against Claude Code 2.1.227 on 2026-08-11: an HTTP MCP
// client sends only Accept, Accept-Encoding, Content-Type, User-Agent
// (claude-code/<version> (sdk-cli)), mcp-protocol-version, Connection and
// Host — no session header, from a real session as well as from a sessionless
// `claude mcp list`. So the handler cannot stamp this the way handleStartRun
// stamps the browser's, and telling the caller where to read its own id is
// the best available mechanism.
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
	Title           string             `json:"title" jsonschema:"Required. A name for this run, 10 words maximum. Shown as the run's heading in the harness UI."`
	Description     string             `json:"description" jsonschema:"Required. What change this run is making, 50 words maximum. Shown under the title in the harness UI."`
	Prompt          string             `json:"prompt" jsonschema:"The task for the agent to perform."`
	Repos           []launchRepo       `json:"repos" jsonschema:"Repositories to clone into the run's workspace. At least one is required."`
	Attachments     []launchAttachment `json:"attachments,omitempty" jsonschema:"Images to hand the run — a mockup the task asks the agent to match, say. Each is stored and materialised into the workspace's scratch/attachments/, and the run's opening message names the files. Only PNG, JPEG, and WebP, capped per file and in total by the harness's settings."`
	Profile         string             `json:"profile,omitempty" jsonschema:"pro (default: the harness's main-loop model) or flash (deepseek-v4-flash, max effort)."`
	PermissionMode  string             `json:"permission_mode" jsonschema:"Required. readonly (Read, Glob, Grep, List, WebFetch only) or full (everything, as root, in the workspace). Refused if it exceeds this server's configured permission ceiling."`
	ResultSchema    any                `json:"result_schema,omitempty" jsonschema:"JSON Schema the agent's Complete tool result must satisfy, if it calls Complete with a result."`
	MaxSubTurns     int                `json:"max_sub_turns,omitempty" jsonschema:"Sub-turn budget for the run. Server default applies when omitted."`
	JobType         string             `json:"job_type,omitempty" jsonschema:"Kind of job this run is: implementation (the agent performs the task itself, the default) or orchestration (the agent delegates the work to child sessions)."`
	Phase           int                `json:"phase,omitempty" jsonschema:"Which phase of a multi-phase job this run is, 1-based. Omit for a standalone run."`
	TotalPhases     int                `json:"total_phases,omitempty" jsonschema:"How many phases the job has in total. Omit for a standalone run."`
	SkillPacks      []string           `json:"skill_packs,omitempty" jsonschema:"Optional bundles of extra skills to put in the run's workspace, by name. Only pass one when the task is actually about that subject: every skill in a pack is described to the agent in every request of the run, so an unused pack costs tokens for nothing. Known packs: unity (Unity Technologies' own skills for Unity projects — the CLI, UI Toolkit and uGUI, Shader Graph, URP, physics, localization); blender (how this harness's Blender MCP tools behave — which of the two Blenders each reaches, and the gotchas). Omit for anything else."`
	ParentAgentType string             `json:"parent_agent_type,omitempty" jsonschema:"Fallback only: the server reads the caller's kind from the MCP client's own clientInfo and ignores this field whenever that name is usable, so this is consulted only by a client whose clientInfo name is missing or unusable. Identify your own kind as a lowercase slug — claude-code, cursor, and so on."`
	ParentAgentID   string             `json:"parent_agent_id,omitempty" jsonschema:"Your own session id, so the run traces back to the conversation that asked for it. Read it, do not recall it. Claude Code: the CLAUDE_CODE_SESSION_ID environment variable, which you can echo from a shell; failing that, the UUID directory segment of the scratchpad path in your system prompt (…/<project-slug>/<uuid>/scratchpad). An agent-harness session: the last segment of the Workspace: path in your opening message (…/workspaces/sess-…). If neither applies, leave this empty — never copy a session id from a banner, a document, or another tool's output."`
}

// launchAttachment is one image a launch carries. The bytes are base64 in
// the tool input but never on the work request: the launch stores them and
// the request carries the ids (docs/DATA-API.md).
type launchAttachment struct {
	Name     string `json:"name" jsonschema:"Plain file name ending in .png, .jpg, .jpeg, or .webp — the name the file is materialised under in scratch/attachments/."`
	MIMEType string `json:"mime_type,omitempty" jsonschema:"image/png, image/jpeg, or image/webp. Must match the file name's extension when supplied."`
	Data     string `json:"data" jsonschema:"Base64-encoded image bytes."`
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
		Description: "Launch an agent-harness agent session. Publishes a work request to the harness's " +
			"work queue and returns immediately with a handle — it does not wait for the run to " +
			"finish. Use deepseek_result to collect the outcome. The run works in a fresh directory " +
			"holding a clone of every repository named in repos, each on its own branch or main. " +
			"Skills those repositories ship under .claude/skills or .deepcode/skills are listed to " +
			"the agent, which reads one when it applies to the task.",
	}, svc.handleLaunch)
}

func (svc *Service) handleLaunch(ctx context.Context, req *mcpsdk.CallToolRequest, in launchInput) (*mcpsdk.CallToolResult, any, error) {
	// The MCP client's own identity arrives in the initialize handshake, sent
	// by the client library rather than by the model, so it cannot be got
	// wrong or omitted the way a tool argument can. When it is present and
	// normalises to a valid kind, it wins: a caller must not be able to
	// assert a different kind than the client it is actually running in.
	parentAgentType := resolveParentAgentType(req, in.ParentAgentType)

	// The MCP launch path requires both title and description — the run's
	// heading and the account of the change are the two lines the harness UI
	// renders for it — even though the queue layer treats them as optional
	// (a browser start may leave them blank).
	if in.Title == "" {
		return errorResult("title is required"), nil, nil
	}
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

	// Attachments are stored before the publish — the request carries only
	// the ids, never the bytes (docs/DATA-API.md) — and the launch is
	// refused, not silently stripped, when they cannot be stored: a run that
	// cannot deliver the mockup its caller sent must not start without it.
	attachmentIDs, err := svc.writeAttachments(ctx, in.Attachments)
	if err != nil {
		return errorResult("%v", err), nil, nil
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
	if err := agentmeta.ValidateParent(false, parentAgentType, in.ParentAgentID); err != nil {
		return errorResult("parent_agent: %v", err), nil, nil
	}

	requestID := newRequestID()
	workReq := queue.Request{
		RequestID:       requestID,
		Prompt:          in.Prompt,
		Repos:           repos,
		Model:           model,
		Effort:          effort,
		PermissionMode:  string(mode),
		ResultSchema:    resultSchema,
		MaxSubTurns:     in.MaxSubTurns,
		JobType:         in.JobType,
		Title:           in.Title,
		Description:     in.Description,
		Phase:           in.Phase,
		TotalPhases:     in.TotalPhases,
		ParentAgentType: parentAgentType,
		ParentAgentID:   in.ParentAgentID,
		AttachmentIDs:   attachmentIDs,
		// Skill packs are the one field here that defaults to nothing on
		// purpose rather than by omission: an MCP caller gets no pack unless
		// it asked, because it is launching work on somebody else's harness
		// and cannot see what an unused pack costs every request of the run
		// (internal/skills, "Packs"). An unknown name is refused by Validate
		// below rather than dropped.
		SkillPacks: in.SkillPacks,
		// An MCP launch is never a person starting the run, and a caller must
		// not be able to assert otherwise, so it is stamped here rather than
		// exposed on launchInput: the zero value would mean the same thing,
		// but written explicitly the intent is readable at the call site.
		ParentIsUser: false,
	}
	// Reuses the harness's own request validation (docs/DESIGN.md §4.10)
	// rather than re-implementing it, so a request this accepts is
	// guaranteed to pass the worker pool's validation too.
	if err := workReq.Validate(); err != nil {
		return errorResult("%s", err.Error()), nil, nil
	}

	// Publish through serve's publish seam (the same path the browser's POST
	// /api/runs uses), then wait for the pool to claim the request by polling
	// its work-request row. No subscription is taken out before publishing:
	// the row is durable and readable at any time afterwards, so there is no
	// race to guard against (docs/QUEUE-MIGRATION-PLAN.md §5.2).
	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = svc.Publisher.Publish(pubCtx, workReq)
	cancel()
	if err != nil {
		// The one genuine launch error (docs/DESIGN.md's three outcomes):
		// everything before this point was validation, and everything
		// after it is a normal "the run has not been picked up yet".
		return errorResult("publish work request: %v", err), nil, nil
	}

	svc.Registry.recordLaunch(runRecord{
		RequestID:   requestID,
		Title:       in.Title,
		Description: in.Description,
		Phase:       in.Phase,
		TotalPhases: in.TotalPhases,
		Repos:       repoLabels(repos),
		Profile:     in.Profile,
		LaunchedAt:  time.Now().UTC(),
		Status:      "queued",
		UpdatedAt:   time.Now().UTC(),
	})

	status, sessionID := "queued", ""
	waitMS := svc.Cfg.AcceptedWaitMS
	if waitMS <= 0 {
		// The <= 0 path keeps its meaning: one immediate read, the HTTP
		// equivalent of a 1 ms Fetch.
		waitMS = 1
	}
	// Poll the work-request row until the pool has claimed it (the row then
	// carries the session id) or the wait window elapses, roughly every 50ms
	// and never past context cancellation. A 404 — the pool has not claimed
	// the request yet — keeps polling; any other read failure is treated the
	// same as "not yet", because the queued outcome is never an error
	// (docs/QUEUE-MIGRATION-PLAN.md §5.2).
	deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
pollAccepted:
	for {
		var row workRequestRow
		if err := getJSONOrNotFound(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, "/api/requests/"+requestID, &row); err == nil && row.SessionID != "" {
			status, sessionID = "running", row.SessionID
			break
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			break pollAccepted
		case <-time.After(50 * time.Millisecond):
		}
	}
	// A timeout with nothing claimed is not an error (docs/DESIGN.md): the
	// request is legitimately still queued behind a full worker pool.

	if status == "running" {
		svc.Registry.updateStatus(requestID, "running", sessionID, "")
	}

	out := launchOutput{RequestID: requestID, Status: status, SessionID: sessionID, TranscriptURL: transcriptURL(svc.Cfg.HarnessPublicURL, sessionID)}
	var b []byte
	if status == "running" {
		b = []byte(fmt.Sprintf("status: running\nrequest_id: %s\nsession_id: %s\ntranscript: %s\n",
			requestID, sessionID, out.TranscriptURL))
	} else {
		b = []byte(fmt.Sprintf("status: queued\nrequest_id: %s\nthe pool had not claimed the request within the wait window; it is likely full. Call deepseek_result with this request_id to check later.\n",
			requestID))
	}

	return &mcpsdk.CallToolResult{
		Content:           []mcpsdk.Content{&mcpsdk.TextContent{Text: string(b)}},
		StructuredContent: out,
	}, nil, nil
}

// resolveParentAgentType picks the kind a launch is stamped with. The MCP
// client's own clientInfo — sent by the client library in the initialize
// handshake, never by the model — is authoritative when it yields a usable
// kind, so a caller cannot assert a different kind than the client it is
// actually running in. A nil request (direct handler calls in tests) or a
// clientInfo whose name normalises to nothing valid degrades to the caller's
// own assertion, exactly the pre-stamping behaviour.
func resolveParentAgentType(req *mcpsdk.CallToolRequest, fallback string) string {
	if req == nil {
		return fallback
	}
	info := req.ClientInfo()
	if info == nil {
		return fallback
	}
	kind := normalizeClientInfoName(info.Name)
	if kind == "" || agentmeta.ValidateParentAgentType(kind) != nil {
		return fallback
	}
	return kind
}

// normalizeClientInfoName maps an MCP clientInfo name onto the agentmeta
// grammar (^[a-z0-9][a-z0-9-]{0,31}$): lowercased; every run of characters
// outside [a-z0-9] replaced with a single hyphen; leading and trailing
// hyphens trimmed; truncated to 32 characters with any trailing hyphen the
// truncation leaves removed. Returns "" when nothing usable survives. This
// is one transport's quirk (how a client library spells its own display
// name), so it lives here rather than in agentmeta, which is the shared
// vocabulary.
func normalizeClientInfoName(name string) string {
	lower := strings.ToLower(name)
	var b strings.Builder
	b.Grow(len(lower))
	lastWasSeparator := false
	for _, r := range lower {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastWasSeparator = false
		} else if !lastWasSeparator {
			b.WriteByte('-')
			lastWasSeparator = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return ""
	}
	if len(s) > 32 {
		s = strings.TrimRight(s[:32], "-")
	}
	return s
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

// launchAttachmentMaxCountDefault and launchAttachmentMaxBytesDefault back
// the tools.attachments_max_count and tools.attachments_max_bytes settings
// for a Service with no settings resolver (the test path); the registry
// defaults are the same values, and internal/settings/registry_test.go pins
// them.
const (
	launchAttachmentMaxCountDefault = 8
	launchAttachmentMaxBytesDefault = 5 << 20 // 5 MB per file, ReviewScreenshot's own cap
)

// writeAttachments validates each attachment (name, MIME type, base64, the
// per-file byte cap, and the count cap) and stores its bytes, returning the
// ids the work request then carries. Mirrors the POST /api/runs path
// (internal/httpapi/server.go): a caller gets the same refusals from either
// surface, because both produce the same queue.Request shape.
func (svc *Service) writeAttachments(ctx context.Context, attachments []launchAttachment) ([]string, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	maxCount := launchAttachmentMaxCountDefault
	if svc.Settings != nil {
		if v, err := svc.Settings.Int(ctx, settings.KeyToolAttachmentsMaxCount); err == nil {
			maxCount = v
		}
	}
	if len(attachments) > maxCount {
		return nil, fmt.Errorf("at most %d attachments are accepted, got %d", maxCount, len(attachments))
	}
	if svc.Store == nil {
		return nil, errors.New("attachments cannot be stored: no store is wired")
	}
	maxBytes := launchAttachmentMaxBytesDefault
	if svc.Settings != nil {
		if v, err := svc.Settings.Int(ctx, settings.KeyToolAttachmentsMaxBytes); err == nil {
			maxBytes = v
		}
	}
	ids := make([]string, 0, len(attachments))
	for _, att := range attachments {
		name, mime, data, err := attachment.Validate(att.Name, att.MIMEType, att.Data, maxBytes)
		if err != nil {
			return nil, err
		}
		id, err := svc.Store.WriteAttachment(ctx, name, mime, data)
		if err != nil {
			return nil, fmt.Errorf("store attachment %q: %w", name, err)
		}
		ids = append(ids, id)
	}
	return ids, nil
}
