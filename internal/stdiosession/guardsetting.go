package stdiosession

import (
	"context"
	"net/url"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/session"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// GuardConfig is `harness.guard` on a create: the autonomy guard that screens
// the session's `bash` calls (docs/STDIO-PROTOCOL.md, "The autonomy guard").
type GuardConfig struct {
	// ClassifyURL is the parent's `POST /classify` endpoint, an http or https
	// URL. Empty turns the guard off, including a stored one.
	ClassifyURL string `json:"classify_url"`
	// Scenario is what the classifier is told the session is for: "repo" or
	// "assistant". Required with a ClassifyURL.
	Scenario string `json:"scenario,omitempty"`
	// Engine names the parent's engine in the classifier's request. Empty
	// sends "agent-harness".
	Engine string `json:"engine,omitempty"`
}

// GuardOptions says this process can host the autonomy guard. Options.Guard
// is nil under the subcommands that refuse the setting.
type GuardOptions struct{}

// resolveGuard returns the guard setting a run starts with: the create's when
// it names one, nil to leave the stored one in force.
//
// A setting that would screen calls is refused when this process cannot host
// it, whether it came from the create or from the row, and when the client did
// not claim the approvals capability, because a block verdict would have
// nobody to ask. The permission mode is not consulted: under `readonly`,
// `bash` is refused before the guard is asked.
func (s *Server) resolveGuard(cfg *GuardConfig, stored store.GuardSetting) (*store.GuardSetting, *rpcError) {
	var out *store.GuardSetting
	effective := stored
	if cfg != nil {
		if s.opts.Guard == nil {
			return nil, errorf(CodeUnsupported, "harness.guard: this subcommand does not support the autonomy guard; `harness stdio-session` and `harness gemini-session` do")
		}
		setting, rerr := guardSettingFrom(cfg)
		if rerr != nil {
			return nil, rerr
		}
		out, effective = &setting, setting
	}
	if !effective.Active() {
		return out, nil
	}
	if s.opts.Guard == nil {
		return nil, errorf(CodeUnsupported, "this session carries an autonomy guard setting and this subcommand cannot host it; resume it under `harness stdio-session`")
	}
	s.mu.Lock()
	approvals := s.clientCaps.Approvals
	s.mu.Unlock()
	if !approvals {
		return nil, errorf(CodeInvalidParams, "harness.guard is active and the client did not claim the approvals capability at initialize, so a blocked command would have nobody to ask")
	}
	return out, nil
}

func guardSettingFrom(cfg *GuardConfig) (store.GuardSetting, *rpcError) {
	raw := strings.TrimSpace(cfg.ClassifyURL)
	if raw == "" {
		return store.GuardSetting{}, nil
	}
	if !httpURL(raw) {
		return store.GuardSetting{}, errorf(CodeInvalidParams, "harness.guard.classify_url must be an http or https URL")
	}
	switch cfg.Scenario {
	case "repo", "assistant":
	case "":
		return store.GuardSetting{}, errorf(CodeInvalidParams, "harness.guard.scenario is required with classify_url: \"repo\" or \"assistant\"")
	default:
		return store.GuardSetting{}, errorf(CodeInvalidParams, "harness.guard.scenario %q must be \"repo\" or \"assistant\"", cfg.Scenario)
	}
	return store.GuardSetting{ClassifyURL: raw, Scenario: cfg.Scenario, Engine: strings.TrimSpace(cfg.Engine)}, nil
}

// httpURL reports whether raw is an absolute http or https URL with a host.
func httpURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// storedSession reads the row of a session this create continues.
func (s *Server) storedSession(ctx context.Context, id string) (store.Session, *rpcError) {
	sess, err := s.opts.Store.GetSession(ctx, id)
	if err != nil {
		return store.Session{}, errorf(CodeInternalError, "read session %s: %v", id, err)
	}
	return sess, nil
}

// approverFor is the session.Approver for the run with the given response id:
// it sends harness.approval_request and waits for the answer. A failure to get
// one, including the run being cancelled or the pipe closing, is an error and
// so a refusal. Only an explicit "approve" runs the command.
func (s *Server) approverFor(runID string) session.Approver {
	return func(ctx context.Context, req session.ApprovalRequest) (session.ApprovalAnswer, error) {
		var res ApprovalAnswerResult
		err := s.conn.Call(ctx, MethodApprovalRequest, ApprovalRequestParams{
			ResponseID: runID, Type: "command_approval", CallID: req.CallID,
			Command: req.Command, Cwd: req.Workdir, Verdict: req.Verdict, Reason: req.Reason,
		}, &res)
		if err != nil {
			return session.ApprovalAnswer{}, err
		}
		return session.ApprovalAnswer{Approve: res.Decision == "approve", Message: res.Message}, nil
	}
}
