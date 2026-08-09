// Package queue is the NATS JetStream ingress and result publication layer
// docs/DESIGN.md §4.10 specifies: the WORK and RESULTS streams, the request
// and result wire shapes, subject naming, and the pieces of that layer that
// do not need a broker to test — parsing, validation, and progress
// rate-limiting.
package queue

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// Request is a work request as it arrives on the WORK stream
// (docs/DESIGN.md §4.10).
type Request struct {
	RequestID      string          `json:"request_id"`
	Prompt         string          `json:"prompt"`
	Repos          []Repo          `json:"repos"`
	Model          string          `json:"model,omitempty"`
	Effort         string          `json:"effort,omitempty"`
	PermissionMode string          `json:"permission_mode"`
	Deny           []string        `json:"deny,omitempty"`
	ResultSchema   json.RawMessage `json:"result_schema,omitempty"`
	MaxSubTurns    int             `json:"max_sub_turns,omitempty"`
	DeadlineMS     int64           `json:"deadline_ms,omitempty"`
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
// no request_id to key a work_requests row or a result subject on, so it is
// the one failure the worker cannot turn into a stored, published result —
// it can only Term the message and log (docs/DESIGN.md §4.10, "Term a
// malformed request").
func ParseRequest(data []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(data, &req); err != nil {
		return Request{}, fmt.Errorf("queue: parse request: %w", err)
	}
	return req, nil
}

// requestIDCharset bars characters that are structurally significant in a
// NATS subject. request_id becomes a subject token in every result subject
// this package builds, so a stray "." would silently misroute a result
// rather than fail loudly at validation.
const requestIDDisallowed = ". \t\n\r*>"

// Validate checks the fields docs/DESIGN.md §4.10 and PLAN.md's phase 3
// work list call out: the repositories to clone, permission_mode, and
// result_schema as a well-formed schema. A failing request gets a "failed"
// result and a Term, never a retry — it will never parse or authorize
// itself into something valid by being redelivered.
func (r Request) Validate() error {
	if r.RequestID == "" {
		return errors.New("queue: request_id is required")
	}
	if strings.ContainsAny(r.RequestID, requestIDDisallowed) {
		return fmt.Errorf("queue: request_id %q contains a character not allowed in a NATS subject token", r.RequestID)
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return errors.New("queue: prompt is required")
	}
	if err := validateRepos(r.Repos); err != nil {
		return err
	}

	if r.PermissionMode == "" {
		return errors.New("queue: permission_mode is required: readonly or full")
	}
	if !tools.Mode(r.PermissionMode).Valid() {
		return fmt.Errorf("queue: permission_mode %q must be readonly or full", r.PermissionMode)
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
