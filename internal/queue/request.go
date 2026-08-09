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
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/tools"
)

// Request is a work request as it arrives on the WORK stream
// (docs/DESIGN.md §4.10).
type Request struct {
	RequestID      string          `json:"request_id"`
	Prompt         string          `json:"prompt"`
	Workspace      string          `json:"workspace"`
	Model          string          `json:"model,omitempty"`
	Effort         string          `json:"effort,omitempty"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	Deny           []string        `json:"deny,omitempty"`
	ResultSchema   json.RawMessage `json:"result_schema,omitempty"`
	MaxSubTurns    int             `json:"max_sub_turns,omitempty"`
	DeadlineMS     int64           `json:"deadline_ms,omitempty"`
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
// work list call out: workspace against roots, permission_mode, and
// result_schema as a well-formed schema. A failing request gets a "failed"
// result and a Term, never a retry — it will never parse or authorize
// itself into something valid by being redelivered. It returns the
// workspace resolved to its absolute, symlink-resolved form so the caller
// runs against exactly the path that was checked.
func (r Request) Validate(roots []string) (resolvedWorkspace string, err error) {
	if r.RequestID == "" {
		return "", errors.New("queue: request_id is required")
	}
	if strings.ContainsAny(r.RequestID, requestIDDisallowed) {
		return "", fmt.Errorf("queue: request_id %q contains a character not allowed in a NATS subject token", r.RequestID)
	}
	if strings.TrimSpace(r.Prompt) == "" {
		return "", errors.New("queue: prompt is required")
	}
	if r.Workspace == "" {
		return "", errors.New("queue: workspace is required")
	}

	resolvedWorkspace, err = workspaceUnderRoots(r.Workspace, roots)
	if err != nil {
		return "", err
	}

	switch tools.Mode(r.PermissionMode) {
	case "", tools.ModeReadOnly, tools.ModeDefault, tools.ModeFull:
	default:
		return "", fmt.Errorf("queue: permission_mode %q must be readonly, default, or full", r.PermissionMode)
	}

	if len(r.ResultSchema) > 0 && !isWellFormedSchema(r.ResultSchema) {
		return "", errors.New("queue: result_schema is not a well-formed JSON Schema object")
	}

	if r.MaxSubTurns < 0 {
		return "", errors.New("queue: max_sub_turns must not be negative")
	}
	if r.DeadlineMS < 0 {
		return "", errors.New("queue: deadline_ms must not be negative")
	}

	return resolvedWorkspace, nil
}

// workspaceUnderRoots reports the resolved form of workspace if it sits
// under any configured root, reusing the same symlink-safe containment
// check the tools package applies to every path a session touches — a work
// request's workspace is not exempt from it (docs/DESIGN.md §4.10, "must
// sit under a configured root").
func workspaceUnderRoots(workspace string, roots []string) (string, error) {
	if len(roots) == 0 {
		return "", errors.New("queue: no workspace roots are configured; set DEEPSEEK_WORKSPACE_ROOTS")
	}
	var lastErr error
	for _, root := range roots {
		resolved, err := tools.ResolvePath(root, workspace)
		if err == nil {
			return resolved, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("queue: workspace %q does not sit under any configured root: %w", workspace, lastErr)
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
