// Package guard is the client of the parent's `POST /classify` endpoint: the
// autonomy guard's whole mechanism in this repository. The classifier, its
// rules and its policy are the endpoint's. This package sends one bash call
// to it and reports what came back, and never decides anything itself.
package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// Verdicts a Classify call reports.
const (
	// Allow is the endpoint's `allow`.
	Allow = "allow"
	// Block is the endpoint's `block`.
	Block = "block"
	// Unreachable is everything else: a transport failure, a status other
	// than 200, a body that does not parse, an answer that names neither
	// verdict, or no answer within the deadline.
	Unreachable = "unreachable"
)

// DefaultTimeout is how long one Classify call waits for an answer.
const DefaultTimeout = 20 * time.Second

const (
	maxAnswerBytes = 64 << 10
	maxReasonRunes = 1000
	branchTimeout  = 2 * time.Second
)

// Request is the body of `POST /classify`.
type Request struct {
	Command string `json:"command"`
	Shell   string `json:"shell"`
	CWD     string `json:"cwd"`
	// Branch is the branch checked out in CWD, absent when it is not a
	// repository or HEAD is detached.
	Branch string `json:"branch,omitempty"`
	// Scenario is "repo" or "assistant".
	Scenario  string `json:"scenario"`
	Engine    string `json:"engine"`
	SessionID string `json:"session_id"`
	CallID    string `json:"call_id"`
}

// Result is what the endpoint answered, or why it did not.
type Result struct {
	// Verdict is Allow, Block or Unreachable.
	Verdict string
	// Reason is the endpoint's explanation for a Block, cut to a bounded
	// length. Empty for an Allow, and for Unreachable (see Err).
	Reason string
	// DecidedBy and GuardVersion are the endpoint's own, when it answered.
	DecidedBy    string
	GuardVersion string
	// Err says why the endpoint counts as unreachable. It never carries the
	// request or the answer's body.
	Err string
}

// Client posts to one `/classify` endpoint.
type Client struct {
	// URL is the full endpoint URL, as the parent gave it.
	URL string
	// HTTP is the client used. Nil takes a fresh http.Client with no
	// overall timeout of its own; the call's deadline is Timeout.
	HTTP *http.Client
	// Timeout bounds one Classify call. Zero takes DefaultTimeout.
	Timeout time.Duration
}

type answer struct {
	Verdict      string `json:"verdict"`
	Reason       string `json:"reason"`
	DecidedBy    string `json:"decided_by"`
	GuardVersion string `json:"guard_version"`
}

// Classify asks the endpoint about one command.
func (c *Client) Classify(ctx context.Context, req Request) Result {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(req)
	if err != nil {
		return unreachable("encode the request: %v", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return unreachable("build the request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	client := c.HTTP
	if client == nil {
		client = &http.Client{}
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return unreachable("no answer within %s", timeout)
		}
		return unreachable("request failed: %v", urlErrorCause(err))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes+1))
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return unreachable("no answer within %s", timeout)
		}
		return unreachable("read the answer: %v", urlErrorCause(err))
	}
	if resp.StatusCode != http.StatusOK {
		return unreachable("status %d", resp.StatusCode)
	}
	if len(raw) > maxAnswerBytes {
		return unreachable("the answer is larger than %d bytes", maxAnswerBytes)
	}
	var a answer
	if err := json.Unmarshal(raw, &a); err != nil {
		return unreachable("the answer is not JSON")
	}
	switch a.Verdict {
	case Allow, Block:
	default:
		return unreachable("the answer's verdict is neither allow nor block")
	}
	return Result{
		Verdict: a.Verdict, Reason: truncate(a.Reason, maxReasonRunes),
		DecidedBy: truncate(a.DecidedBy, 64), GuardVersion: truncate(a.GuardVersion, 128),
	}
}

func unreachable(format string, args ...any) Result {
	return Result{Verdict: Unreachable, Err: fmt.Sprintf(format, args...)}
}

// urlErrorCause drops the URL net/http puts in front of a transport error.
func urlErrorCause(err error) error {
	if ue, ok := err.(interface{ Unwrap() error }); ok {
		if inner := ue.Unwrap(); inner != nil {
			return inner
		}
	}
	return err
}

func truncate(s string, runes int) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= runes {
		return s
	}
	return string(r[:runes]) + "…"
}

// Branch names the branch checked out in dir, or "" when dir is not in a
// repository, git is missing, or HEAD is detached.
func Branch(ctx context.Context, dir string) string {
	ctx, cancel := context.WithTimeout(ctx, branchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if name == "HEAD" {
		return ""
	}
	return name
}
