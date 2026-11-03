package stdiosession

import (
	"context"
	"net/url"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/guard"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// GuardConfig is `harness.guard` on a create: the autonomy guard that screens
// the session's `bash` calls (docs/STDIO-PROTOCOL.md, "Permissions").
type GuardConfig struct {
	// Mode is "off", "shadow" or "enforce". A create cannot select "enforce".
	Mode string `json:"mode"`
	// Stage1Endpoint is the host the stage 1 classifier is posted to, without
	// the path. Required with "shadow" unless the session already stores one.
	Stage1Endpoint string `json:"stage1_endpoint,omitempty"`
	// Stage2Model is the Anthropic model stage 2 asks. Empty takes the
	// stored value, then the guard's default.
	Stage2Model string `json:"stage2_model,omitempty"`
}

// GuardOptions says this process can host the autonomy guard. Options.Guard
// is nil under the subcommands that refuse the setting.
type GuardOptions struct {
	// HasAPIKey reports whether ANTHROPIC_API_KEY reached this process.
	HasAPIKey func() bool
}

const guardKeyMessage = "harness.guard needs an Anthropic API key for stage 2: set ANTHROPIC_API_KEY in the environment you spawn this process with"

// resolveGuard returns the guard setting a run starts with.
//
// cfg is the create's setting, stored the setting already on the session
// (zero for a new one) and permission the session's permission mode. A create
// that names no guard returns nil, which leaves the stored setting in force.
// A setting that would screen calls is refused when this process cannot host
// it, whether it came from the create or from the row.
func (s *Server) resolveGuard(cfg *GuardConfig, stored store.GuardSetting, permission tools.Mode) (*store.GuardSetting, *rpcError) {
	var out *store.GuardSetting
	effective := stored
	if cfg != nil {
		if s.opts.Guard == nil {
			return nil, errorf(CodeUnsupported, "harness.guard: this subcommand does not support the autonomy guard; `harness stdio-session` does")
		}
		merged, rerr := mergeGuard(cfg, stored)
		if rerr != nil {
			return nil, rerr
		}
		out, effective = &merged, merged
	}
	if !effective.Active() {
		return out, nil
	}
	if s.opts.Guard == nil {
		return nil, errorf(CodeUnsupported, "this session carries an autonomy guard setting and this subcommand cannot host it; resume it under `harness stdio-session`")
	}
	if permission != tools.ModeFull {
		return nil, errorf(CodeInvalidParams, "harness.guard: the guard screens `bash` and `bash` is refused under %q; use harness.permission_mode %q or send mode %q", permission, tools.ModeFull, store.GuardModeOff)
	}
	if s.opts.Guard.HasAPIKey == nil || !s.opts.Guard.HasAPIKey() {
		return nil, errorf(CodeCredentialsMissing, "%s", guardKeyMessage)
	}
	return out, nil
}

// mergeGuard applies a create's setting over the stored one.
func mergeGuard(cfg *GuardConfig, stored store.GuardSetting) (store.GuardSetting, *rpcError) {
	out := stored
	switch cfg.Mode {
	case store.GuardModeOff, store.GuardModeShadow:
		out.Mode = cfg.Mode
	case store.GuardModeEnforce:
		return out, errorf(CodeUnsupported, "harness.guard.mode %q: a create cannot select it yet; use %q", cfg.Mode, store.GuardModeShadow)
	case "":
		return out, errorf(CodeInvalidParams, "harness.guard.mode is required: %q, %q or %q", store.GuardModeOff, store.GuardModeShadow, store.GuardModeEnforce)
	default:
		return out, errorf(CodeInvalidParams, "harness.guard.mode %q must be %q or %q", cfg.Mode, store.GuardModeOff, store.GuardModeShadow)
	}
	if e := strings.TrimSpace(cfg.Stage1Endpoint); e != "" {
		out.Stage1Endpoint = e
	}
	if m := strings.TrimSpace(cfg.Stage2Model); m != "" {
		out.Stage2Model = m
	}
	if !out.Active() {
		return out, nil
	}
	if out.Stage1Endpoint == "" {
		return out, errorf(CodeInvalidParams, "harness.guard.stage1_endpoint is required with mode %q", out.Mode)
	}
	if u, err := url.Parse(out.Stage1Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return out, errorf(CodeInvalidParams, "harness.guard.stage1_endpoint %q must be an http or https URL", out.Stage1Endpoint)
	}
	if out.Stage2Model == "" {
		out.Stage2Model = guard.DefaultStage2Model
	}
	return out, nil
}

// storedSession reads the row of a session this create continues.
func (s *Server) storedSession(ctx context.Context, id string) (store.Session, *rpcError) {
	sess, err := s.opts.Store.GetSession(ctx, id)
	if err != nil {
		return store.Session{}, errorf(CodeInternalError, "read session %s: %v", id, err)
	}
	return sess, nil
}
