// Package queue is the work queue layer docs/DESIGN.md §4.10 specifies:
// the request and result wire shapes, validation, and the store-backed
// queue (the work_queue table in internal/store) that replaced the WORK and
// RESULTS streams.
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/agentmeta"
	"github.com/mrgeoffrich/deepseek-harness/internal/promptvariant"
	"github.com/mrgeoffrich/deepseek-harness/internal/provider"
	"github.com/mrgeoffrich/deepseek-harness/internal/skills"
	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// Request is a work request as it arrives on the WORK stream
// (docs/DESIGN.md §4.10).
type Request struct {
	RequestID       string          `json:"request_id"`
	Prompt          string          `json:"prompt"`
	Repos           []Repo          `json:"repos"`
	Model           string          `json:"model,omitempty"`
	Effort          string          `json:"effort,omitempty"`
	PermissionMode  string          `json:"permission_mode"`
	Deny            []string        `json:"deny,omitempty"`
	ResultSchema    json.RawMessage `json:"result_schema,omitempty"`
	MaxSubTurns     int             `json:"max_sub_turns,omitempty"`
	DeadlineMS      int64           `json:"deadline_ms,omitempty"`
	JobType         string          `json:"job_type,omitempty"`
	ParentAgentType string          `json:"parent_agent_type,omitempty"`
	ParentAgentID   string          `json:"parent_agent_id,omitempty"`
	// Title is the run's name, at most agentmeta.MaxTitleWords words, shown
	// bold on the main page. Absent is allowed at this layer — the browser
	// start and the CLI may leave it blank — and the queue enforces only the
	// word cap and the no-newline rule (agentmeta.ValidateTitle).
	Title string `json:"title,omitempty"`
	// Description is what change the agent is making, at most
	// agentmeta.MaxDescriptionWords words, shown under the title on the main
	// page. Absent is allowed at this layer for the same reason a blank
	// prompt is: presence is the producer's call.
	Description string `json:"description,omitempty"`
	// Phase is this run's 1-based position in a multi-phase chain. Both this
	// and TotalPhases zero means the run is not part of a chain.
	Phase int `json:"phase,omitempty"`
	// TotalPhases is how many phases the chain has. When set, Phase is
	// 1-based and at most TotalPhases (agentmeta.ValidatePhase).
	TotalPhases int `json:"total_phases,omitempty"`
	// ParentIsUser records that a person started this run directly. It is set
	// by the producer — a browser start, the MCP tool, or the CLI — and never
	// by the calling agent, which is what makes it trustworthy where
	// parent_agent_type is not.
	ParentIsUser bool `json:"parent_is_user,omitempty"`
	// PromptVariant names an alternative system prompt for an eval run
	// (internal/session/variants.go). Empty is the shipped prompt, which is
	// what every production request sends.
	PromptVariant string `json:"prompt_variant,omitempty"`
	// ReminderPolicy names the cadence on which the loop re-states a rule
	// mid-conversation (internal/session/reminders.go). Empty is none.
	ReminderPolicy string `json:"reminder_policy,omitempty"`
	// AttachmentIDs names the images the request carries. The bytes never
	// ride the queue request — one request is one row of the work_queue
	// table, and a multi-megabyte payload would bloat it — they live in the
	// store's attachments table, written by the producer (POST /api/runs,
	// the MCP launch tool) before enqueuing. The worker reads the rows back
	// and internal/workspace materialises them into scratch/attachments/
	// during Prepare, so the request stays small and `harness export` —
	// which derives from the store — stays complete (docs/DATA-API.md).
	AttachmentIDs []string `json:"attachment_ids,omitempty"`
	// SkillPacks names the optional bundles of shipped skills this run's
	// workspace gets, on top of the always-on tree every run receives
	// (internal/skills, "Packs"). Empty — the default on every producer — is
	// a run that gets none of them, which is what keeps a pack's descriptions
	// out of the opening message of every run that has nothing to do with it.
	// Set by the producer: the browser's start form has a checkbox per pack,
	// the MCP launch tool a skill_packs argument, and harness run and
	// harness publish a -skill-pack flag.
	//
	// Unlike the tool array, this varies per run without costing anything:
	// the catalogue rides in the opening user message, not the frozen head
	// (internal/skills, docs/CACHE.md), so two runs whose packs differ still
	// share a system prompt and a tool array. That is the whole reason this
	// can be per-request where an MCP server's tools cannot (docs/MCP.md,
	// "Configuration is global").
	SkillPacks []string `json:"skill_packs,omitempty"`
	// ResumeSessionID continues an existing session instead of creating one
	// (docs/RUN-CONTROL.md, "Continuing"). Set, this request builds no
	// workspace and clones nothing: the session's own frozen row supplies the
	// workspace, the model, the effort, the permission mode, the deny
	// patterns and the tool array, because a resumed session's prefix cannot
	// change (docs/CACHE.md). Prompt is the continuation message. Repos must
	// be empty, and that is the only rule Validate relaxes — the producer
	// copies permission_mode and model off the session row before publishing,
	// so a resume request satisfies every other check unchanged rather than
	// weakening it for the producers that do start a session.
	ResumeSessionID string `json:"resume_session_id,omitempty"`
}

// Repo is one checkout a request asks for. The worker clones each one into
// the run's own workspace directory before the session starts
// (docs/DESIGN.md §4.10).
type Repo struct {
	URL    string `json:"url"`
	Branch string `json:"branch,omitempty"`
}

// DefaultBranch is cloned when a Repo names no branch.
const DefaultBranch = "main"

// BranchOrDefault is the ref to clone, filling in DefaultBranch.
func (r Repo) BranchOrDefault() string {
	if r.Branch == "" {
		return DefaultBranch
	}
	return r.Branch
}

// Dir is the directory name the repository is cloned into: the last path
// segment of the URL without its .git suffix. Two repos in one request that
// would land on the same name are rejected at validation rather than one
// silently shadowing the other.
func (r Repo) Dir() string {
	u := strings.TrimRight(r.URL, "/")
	u = strings.TrimSuffix(u, ".git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	return u
}

// ParseRequest decodes a work request's JSON body. A syntax error here has
// no request_id to key a work_requests row on, so it is the one failure the
// worker cannot turn into a stored, published result — it can only Term the
// message and log (docs/DESIGN.md §4.10, "Term a malformed request").
func ParseRequest(data []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return Request{}, fmt.Errorf("queue: parse request: %w", err)
	}
	return req, nil
}

// requestIDCharset bars characters that are structurally significant in a
// URL path. request_id is a path segment in GET /api/requests/{request_id}
// and the other request-keyed endpoints, so a stray "." or "*" would
// silently misroute a read (or fail a client's URL build) rather than fail
// loudly at validation.
const requestIDDisallowed = ". \t\n\r*>"

// Validate checks the fields docs/DESIGN.md §4.10 calls out: the
// repositories to clone, permission_mode, and
// result_schema as a well-formed schema. The prompt is deliberately not
// required: a browser start may create the run first and let the operator
// type the first message into the session (docs/RUN-CONTROL.md "Start").
// A failing request gets a "failed" result and a Term, never a retry — it
// will never parse or authorize itself into something valid by being
// redelivered.
func (r Request) Validate() error {
	if r.RequestID == "" {
		return errors.New("queue: request_id is required")
	}
	if strings.ContainsAny(r.RequestID, requestIDDisallowed) {
		return fmt.Errorf("queue: request_id %q contains a character not allowed in a URL path segment", r.RequestID)
	}
	// A resume names a session instead of naming repositories: its workspace
	// already exists, with the clones the original request made still in it,
	// and re-cloning over them is neither wanted nor possible. Repos carried
	// alongside one are refused rather than ignored, so a producer that meant
	// to start a run and set both fields hears about it here.
	if r.ResumeSessionID != "" {
		if len(r.Repos) > 0 {
			return errors.New("queue: repos must be empty on a resume request: a resumed session keeps the workspace it already has")
		}
		// Skill packs are refused on a resume for the same reason repos are,
		// and it is the same reason twice: the workspace already exists. A
		// resume builds none, so there is nothing for Install to write into,
		// and the packs the original request asked for are still sitting in
		// it. Refusing rather than ignoring is what stops a producer
		// believing it added a pack mid-session.
		if len(r.SkillPacks) > 0 {
			return errors.New("queue: skill_packs must be empty on a resume request: a resumed session keeps the skills its workspace already has")
		}
	} else if err := validateRepos(r.Repos); err != nil {
		return err
	}

	if r.PermissionMode == "" {
		return errors.New("queue: permission_mode is required: readonly or full")
	}
	if !tools.Mode(r.PermissionMode).Valid() {
		return fmt.Errorf("queue: permission_mode %q must be readonly or full", r.PermissionMode)
	}

	// A named model must be in the model→provider table: an unknown model
	// fails loudly at validation rather than reaching the provider and
	// failing there, and rather than silently defaulting to a provider
	// (internal/provider, docs/KIMI-INTEGRATION.md §4.3). Empty means "the
	// default model" and is resolved by the worker.
	if r.Model != "" {
		if _, err := provider.ModelFor(r.Model); err != nil {
			return fmt.Errorf("queue: %w", err)
		}
	}

	if len(r.ResultSchema) > 0 && !isWellFormedSchema(r.ResultSchema) {
		return errors.New("queue: result_schema is not a well-formed JSON Schema object")
	}

	if r.MaxSubTurns < 0 {
		return errors.New("queue: max_sub_turns must not be negative")
	}
	if r.DeadlineMS < 0 {
		return errors.New("queue: deadline_ms must not be negative")
	}
	if err := agentmeta.ValidateJobType(r.JobType); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	if err := agentmeta.ValidateTitle(r.Title); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	if err := agentmeta.ValidateDescription(r.Description); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	if err := agentmeta.ValidatePhase(r.Phase, r.TotalPhases); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	if err := agentmeta.ValidateParent(r.ParentIsUser, r.ParentAgentType, r.ParentAgentID); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	if err := promptvariant.Validate(r.PromptVariant); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	if err := promptvariant.ValidateReminderPolicy(r.ReminderPolicy); err != nil {
		return fmt.Errorf("queue: %w", err)
	}
	if err := validateAttachmentIDs(r.AttachmentIDs); err != nil {
		return err
	}
	if err := skills.ValidatePacks(r.SkillPacks); err != nil {
		return fmt.Errorf("queue: %w", err)
	}

	return nil
}

// validateAttachmentIDs rejects an id a producer could not have minted: it
// must be non-empty, free of whitespace, and not duplicated. The count and
// per-file byte caps are enforced where attachments are accepted (POST
// /api/runs and the MCP launch tool), because they are settings; this check
// only guarantees the ids are well-formed.
func validateAttachmentIDs(ids []string) error {
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if id == "" {
			return fmt.Errorf("queue: attachment_ids[%d] is empty", i)
		}
		if strings.ContainsAny(id, " \t\n\r") {
			return fmt.Errorf("queue: attachment_ids[%d] %q contains whitespace", i, id)
		}
		if seen[id] {
			return fmt.Errorf("queue: attachment_ids[%d] %q is duplicated", i, id)
		}
		seen[id] = true
	}
	return nil
}

// repoSchemes are the transports a request may name. The set is an
// allowlist because git treats some transports as a command to run:
// "ext::sh -c ..." clones by executing a shell, and a local path clones
// whatever the harness's filesystem holds.
var repoSchemes = []string{"https://", "http://", "ssh://", "git://"}

// scpLike matches git's alternative remote syntax, user@host:path.
var scpLike = regexp.MustCompile(`^[A-Za-z0-9._~-]+@[A-Za-z0-9._-]+:[^\s]+$`)

func validateRepos(repos []Repo) error {
	if len(repos) == 0 {
		return errors.New("queue: repos is required: name at least one repository to clone")
	}
	dirs := make(map[string]bool, len(repos))
	for i, repo := range repos {
		if err := repo.validate(); err != nil {
			return fmt.Errorf("queue: repos[%d]: %w", i, err)
		}
		dir := repo.Dir()
		if dirs[dir] {
			return fmt.Errorf("queue: repos[%d]: %q and an earlier repository both clone into %q", i, repo.URL, dir)
		}
		dirs[dir] = true
	}
	return nil
}

func (r Repo) validate() error {
	if r.URL == "" {
		return errors.New("url is required")
	}
	if !isSupportedRepoURL(r.URL) {
		return fmt.Errorf("url %q must be an http(s), ssh, git, or user@host:path remote", r.URL)
	}
	if err := validateBranchName(r.BranchOrDefault()); err != nil {
		return err
	}
	if dir := r.Dir(); dir == "" || dir == "." || dir == ".." || strings.ContainsAny(dir, `/\`) {
		return fmt.Errorf("url %q does not end in a usable directory name", r.URL)
	}
	return nil
}

func isSupportedRepoURL(url string) bool {
	for _, scheme := range repoSchemes {
		if strings.HasPrefix(url, scheme) && len(url) > len(scheme) {
			return true
		}
	}
	return scpLike.MatchString(url)
}

// validateBranchName rejects what git itself rejects in a ref name, plus a
// leading dash, which git clone would read as an option rather than a
// branch.
func validateBranchName(branch string) error {
	switch {
	case branch == "":
		return errors.New("branch must not be empty")
	case strings.HasPrefix(branch, "-"):
		return fmt.Errorf("branch %q must not start with a dash", branch)
	case strings.ContainsAny(branch, " \t\n\r~^:?*[\\"), strings.Contains(branch, ".."):
		return fmt.Errorf("branch %q contains a character git does not allow in a ref name", branch)
	}
	return nil
}

// isWellFormedSchema checks that schema parses as JSON and is a top-level
// object, which is what every JSON Schema document is. It stops short of
// validating against the JSON Schema meta-schema — Complete's own
// ValidateAgainstSchema already covers the subset this harness supports,
// and a full meta-schema check would reject documents that subset doesn't
// need to reject.
func isWellFormedSchema(schema json.RawMessage) bool {
	var v any
	if err := json.Unmarshal(schema, &v); err != nil {
		return false
	}
	_, ok := v.(map[string]any)
	return ok
}
