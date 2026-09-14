// Package session implements the agent loop: sub-turn iteration against a
// model provider's API through the narrow Client seam, tool execution, and
// the session-runner interface that holds no state outside the session it is
// running (docs/DESIGN.md §4.5). The loop states intent (wire.ChatIntent)
// and never names a provider; cmd/harness chooses which provider's client
// the Runner gets (docs/KIMI-INTEGRATION.md §4.1).
//
// Runner's five jobs are split by file, all on the same type: this file
// holds RunOptions, the Runner type itself, and the settings accessors
// beside it (clientFor, acquireModelSlot, flashModel, compactionThreshold);
// lifecycle.go is Create/FailSetup/Run and the loop
// that drives a run to a terminal result; sinks.go is where a sub-turn's
// output goes (the disk mirror, the hub); tooldispatch.go executes a
// sub-turn's tool calls; livestate.go persists the plan and recent-calls
// roll a running session shows.
package session

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/httplog"
	"github.com/mrgeoffrich/agent-harness/internal/hub"
	"github.com/mrgeoffrich/agent-harness/internal/pricing"
	"github.com/mrgeoffrich/agent-harness/internal/provider"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// seesImages reports whether model reads images natively. It answers per
// model, not per provider — deepseek-flash is a DeepSeek model whose vision
// capability disagrees with its provider's
// (docs/DEEPSEEK-VISION.md) — by consulting internal/provider's one
// model→capability table, provider.SeesImages, the source both this
// function and internal/tools.DefinitionsFor read. It drives both halves of
// the vision split: which tool array the session sends
// (internal/tools.DefinitionsFor) and whether Read returns an image part
// (tools.Executor.SeeImages). An unknown model resolves to false, the
// DeepSeek default; queue validation rejects unknown models before a session
// exists, so nothing real can land here.
func seesImages(model string) bool {
	return provider.SeesImages(model)
}

// CompactionThresholdTokens is the built-in compaction threshold when the
// Runner has no settings resolver attached (the test path). Production
// resolves it through the settings registry (run.compaction_threshold), which
// carries this exact value as its default; the two are pinned equal by
// internal/settings/registry_test.go. It is DeepSeek's recommended Claude Code
// compaction window, 768K of the 1M context (docs/TOOLS.md, "Context and
// compaction").
const CompactionThresholdTokens = 768 * 1024

// KimiK3CompactionThresholdTokens is kimi-k3's own compaction threshold,
// resolving ahead of the global default for that model
// (run.compaction_threshold_kimi_k3 in the settings registry, which carries
// this exact value as its default and pins it equal the same way). K3
// cache-miss input costs about 20x deepseek-flash's (configs/prices.json),
// so a full-prompt miss at DeepSeek's threshold would cost an order of
// magnitude more (docs/KIMI-INTEGRATION.md §3).
const KimiK3CompactionThresholdTokens = 128 * 1024

// RunOptions is everything one session run needs. It is deliberately flat
// rather than reaching into global config, so a Runner can be shared by
// many concurrent calls to Run without any of them touching another's
// state.
type RunOptions struct {
	Model          string
	Effort         string
	Thinking       bool
	MaxTokens      int
	Workspace      string
	PermissionMode tools.Mode
	Deny           []string
	Prompt         string
	ResultSchema   json.RawMessage
	ParentID       string
	JobType        string
	// Title is the run's name, at most agentmeta.MaxTitleWords words, shown
	// bold on the main page. Empty is a producer's choice — a browser start
	// may leave it blank — and the session row then renders without a bold
	// title.
	Title string
	// Description is what change the agent is making, at most
	// agentmeta.MaxDescriptionWords words, shown under the title on the main
	// page. Empty is a producer's choice; the browser falls back to the
	// prompt as the description line.
	Description string
	// Phase is this run's 1-based position in a multi-phase chain. Both this
	// and TotalPhases zero means the run is not part of a chain.
	Phase int
	// TotalPhases is how many phases the chain has. When set, Phase is
	// 1-based and at most TotalPhases (agentmeta.ValidatePhase).
	TotalPhases     int
	ParentAgentType string
	ParentAgentID   string
	// ParentIsUser records that a person started this run directly. It is
	// producer-stamped and inherited by child sessions, like the rest of the
	// provenance triple.
	ParentIsUser bool

	// PromptVariant names the system prompt this run uses. Empty is the
	// shipped prompt; anything else is an eval run (variants.go).
	PromptVariant string

	// ReminderPolicy names the cadence on which the loop re-states a rule
	// further down the conversation. Empty is none, which is what production
	// runs do (reminders.go). A variant may name one, and this overrides it.
	ReminderPolicy string

	// AttachmentNames are the files the request's attachments were
	// materialised into under scratch/attachments/.
	// RenderOpeningMessage names them so the model knows they exist and can
	// pass one to Glance.
	AttachmentNames []string

	// SessionID, when set, is used instead of generating a fresh one. A
	// caller that must know the id before the session row exists — the
	// worker pool acquiring a workspace lease under it before calling Run,
	// say — generates one with NewSessionID and passes it back here so the
	// lease and the session agree.
	SessionID string

	// Tools is the session's tool array — the frozen head's second half —
	// resolved once, by Run or Resume, and passed unchanged through the
	// whole run (docs/MCP.md, "Resolution happens once per run";
	// docs/CACHE.md). Run resolves it from tools.DefinitionsForVariant plus
	// the MCP snapshot at call time and stores the same bytes as the
	// session's tool_schema; Resume instead reads that stored tool_schema
	// back rather than re-resolving, so a server enabled or disabled since
	// the run started cannot change what a resumed session sends. A caller
	// building a RunOptions does not set this field: Run and Resume each
	// overwrite it before runLoop's first request, from their own source of
	// truth.
	Tools []wire.Tool

	// Resuming is set only by Runner.Resume, and only to keep runLoop's
	// empty-prompt wait (waitForFirstSteer, lifecycle.go) from firing on a
	// resume whose session never got past its first sub-turn — a stop that
	// lands before the run's first turn_started commits leaves countTurns at
	// 0, so a resumed startSubTurn is 1 exactly the way a fresh browser
	// start's is, and Resume's own RunOptions carries no Prompt regardless of
	// what the resume's ResumeOptions.Prompt held (deliberately, per the
	// session method's doc comment — it is not a resumed run's task to
	// rewrite). Without this, that resume reads as "operator hasn't typed
	// anything yet" and waits on a steer nobody is going to send
	// (docs/RUN-CONTROL.md "Continuing").
	Resuming bool

	// DebugChurnOnSubTurn, when equal to a sub-turn number, deliberately
	// breaks that one sub-turn's shared prefix before sending it (via
	// cache.Mutate on the opening message) so the churn diagnostic has
	// something real to catch. Zero disables it. Nothing publishes this
	// through a work request; it exists to exercise CACHE.md's diagnostic
	// against the live API on purpose, not as something a production
	// caller would ever set.
	DebugChurnOnSubTurn int
}

// session builds the store.Session row RunOptions describes, at the given
// status and workspace. Create and Run's insert branch both build their row
// through this method, rather than each spelling out the same field list by
// hand, because a hand-copied literal is how ResultSchema once reached one
// insert path (Run) and not another (Create) while PromoteSession wrote
// neither — a queue-driven run's ResultSchema silently never reached the
// row. SystemPrompt and ToolSchema are not resolvable from RunOptions alone
// (they depend on the rendered prompt and the resolved tool array) and stay
// the caller's to set afterward, which is why Create's row leaves them at
// their zero value and Run's does not.
//
// SessionID is resolved here so Create needs no separate id variable; Run
// resolves opts.SessionID itself before calling this, so the same id backs
// its GetSession check and its RunSubagent closure.
//
// Only for a row built fresh from the caller's own RunOptions. compact
// (compact.go) forks a successor from an existing store.Session instead of
// calling this: Runner.Resume builds its RunOptions from the session row
// and deliberately leaves Title, Description, Phase, TotalPhases, and
// Prompt unset (resume.go), because none of those is a resumed run's to
// choose again — so for a resumed run, opts and the session row disagree on
// exactly the fields a compacted successor needs to carry forward. A copy
// of the row itself cannot have that problem.
func (o RunOptions) session(status, workspace string) store.Session {
	sessID := o.SessionID
	if sessID == "" {
		sessID = newID("sess")
	}
	return store.Session{
		ID:              sessID,
		ParentID:        o.ParentID,
		JobType:         o.JobType,
		Task:            o.Prompt,
		Title:           o.Title,
		Description:     o.Description,
		Phase:           o.Phase,
		TotalPhases:     o.TotalPhases,
		ParentAgentType: o.ParentAgentType,
		ParentAgentID:   o.ParentAgentID,
		ParentIsUser:    o.ParentIsUser,
		Model:           o.Model,
		PromptVariant:   o.PromptVariant,
		Effort:          o.Effort,
		Thinking:        o.Thinking,
		Workspace:       workspace,
		PermissionMode:  string(o.PermissionMode),
		DenyPatterns:    o.Deny,
		ResultSchema:    o.ResultSchema,
		Status:          status,
	}
}

// Usage aggregates token accounting across every sub-turn of a run.
type Usage struct {
	CacheHitTokens   int
	CacheMissTokens  int
	CompletionTokens int
	ReasoningTokens  int
	CostUSD          float64
}

func (u *Usage) add(p store.UsagePayload) {
	u.CacheHitTokens += p.PromptCacheHitTokens
	u.CacheMissTokens += p.PromptCacheMissTokens
	u.CompletionTokens += p.CompletionTokens
	u.ReasoningTokens += p.ReasoningTokens
	u.CostUSD += p.CostUSD
}

// RunResult is what Run (or Resume) returns once a session reaches a
// terminal state. SubTurns is the session's absolute sub-turn count at that
// point, so it reads the same way after a Resume as after a fresh Run; Usage
// is only this call's own contribution (the sub-turns it actually ran), not
// the session's lifetime total — store.SessionUsageSummaries has that.
type RunResult struct {
	SessionID string
	Status    string
	Text      string
	Result    json.RawMessage
	Summary   string
	SubTurns  int
	Usage     Usage
	// CompleteStatus is the status argument to Complete, when the model
	// called it: "done" or "gave_up". Empty means Complete was never
	// called, which is a normal outcome — thinking mode cannot force a
	// tool call (docs/DESIGN.md §4.6) — not a sign of anything wrong.
	CompleteStatus string
	// Reason is run_finished's reason for the run ending: "complete",
	// "no_tool_calls", "complete_rejected". Status alone does not separate
	// the ways a run can end without a valid result, and the queue result's
	// error code is derived from this.
	Reason string
}

// Runner executes agent sessions. Its fields are shared, read-mostly
// resources (a store with its own internal synchronisation, a stateless API
// client, a price table); nothing about one call to Run leaks into another.
// That is what lets the worker pool wrap this type without changing it
// (docs/DESIGN.md §4.5).
type Runner struct {
	Store  *store.Store
	Mirror *store.Mirror
	// Client is the provider seam the loop speaks through: it states intent
	// (wire.ChatIntent) and the implementation — *deepseek.Client today,
	// *kimi.Client next — turns that into its provider's request shape, maps
	// usage onto cache hit and miss, and repairs its own response quirks.
	// Declared in this package, implemented in the provider packages, chosen
	// by cmd/harness when the Runner is built (client.go,
	// docs/KIMI-INTEGRATION.md §4.1).
	Client Client

	// ClientFor, when set, resolves the provider client for a model name.
	// The pool serves mixed-model requests from one shared Runner, so the
	// loop routes every request by the model it is about to speak to —
	// deepseek-v4-* to the DeepSeek client, kimi-k3 to the Kimi client —
	// rather than the Runner being pinned to one provider at construction.
	// The closure is built in cmd/harness, where the provider clients live;
	// nil falls back to Client for every model, which every test relies on
	// (docs/KIMI-INTEGRATION.md §4.3).
	ClientFor func(model string) Client

	Prices     *pricing.Table
	FlashModel string

	// Gemini is the client the vision tools use to send images
	// to Google's Gemini API. Nil is a caller's case when none is configured:
	// the tool then reports itself unavailable instead of failing the run
	// (internal/tools, Glance).
	Gemini *gemini.Client

	// MCP is the seam to every configured MCP server (internal/mcpclient
	// implements it, cmd/harness builds and shares the one Manager). Run
	// resolves its Definitions once, at run start, into the session's
	// frozen tool array and read-only map (docs/MCP.md, "Resolution
	// happens once per run"); Resume reads the array back from the stored
	// row but still calls this for the read-only map, which is policy
	// rather than prefix bytes (resume.go). Nil is every existing test
	// path, and a caller with no manager wired: a run then carries no MCP
	// tools at all, and a failure reading it here is logged and treated the
	// same as nil rather than failing the run.
	MCP tools.MCPProvider

	// GeminiModel resolves the vision model name per call — the same
	// read-through-the-store shape as the DeepSeek API key provider, so a
	// model changed from the settings screen (google.vision_model) takes
	// effect without a restart. Nil falls back to gemini.DefaultModel.
	GeminiModel func() (string, error)

	// ToolEnv, when set, is handed to every Executor this Runner builds as
	// tools.Executor.ExtraEnv: the environment variables a session's Bash
	// calls get on top of this process's own. It carries the GitHub App
	// installation token for gh, which cannot be process-wide because an App
	// mints a different token per account. Built in cmd/harness like the rest of the
	// composition; nil is every test and every harness on the github.token
	// path, and a Bash call then inherits this process's environment
	// unchanged.
	ToolEnv func(ctx context.Context, workspace string) []string

	// ToolEnvFilter, when set, is handed to every Executor this Runner
	// builds as tools.Executor.EnvFilter: applied to this process's own
	// environment before a Bash call's subprocess inherits it. It exists
	// for `harness stdio-session`, which must not let a hosted session's
	// Bash calls see the GEMINI_API_KEY or GOOGLE_API_KEY the parent
	// supplied only for this process's own API client (docs/STDIO-PROTOCOL.md,
	// "Trust boundaries"). Nil is every other caller, `harness serve`
	// included, and a Bash call then inherits this process's environment
	// unchanged.
	ToolEnvFilter func(base []string) []string

	// RG is the ripgrep binary the session's Grep calls exec, resolved once
	// when the Runner is built (tools.RipgrepPath, from -rg, then
	// AGENT_HARNESS_RG, then the PATH) and handed to every Executor as
	// tools.Executor.RG. Empty means none was found and Grep falls back to
	// its own walk.
	RG string

	// Hub, when set, is where every committed event and every session
	// state change gets published for a browser to watch live. Nil is a
	// caller with none wired: nothing subscribes, so nothing is published.
	Hub *hub.Hub

	// Recorder, when set, captures every HTTP exchange a session makes into
	// its own file under the recorder's root (docs/DESIGN.md §4.8). Nil is
	// the normal case when capture is off; every use is guarded. The runner
	// opens a session's writer where its row is created and closes it when
	// the run ends, so compacted successors and Task subagents each get
	// their own file.
	Recorder *httplog.Recorder

	CompactionThresholdTokens int

	// Settings, when set, is where the compaction threshold
	// (run.compaction_threshold, plus the per-model key that replaces it for
	// kimi-k3) and the flash model (model.flash) resolve from, read through
	// the store on every call, so a key changed from the settings screen
	// takes effect on the next session without a restart. Nil is the test
	// path: the package constants apply.
	Settings *settings.Resolver

	// ModelLimits caps concurrent in-flight DeepSeek requests per model,
	// shared across every call to Run on this Runner — the semaphore
	// docs/DESIGN.md §4.5 sizes under the account's per-model ceiling. A
	// model absent from the map, or a nil map, is unlimited; that keeps
	// unlimited callers (every test among them) exactly as they were.
	ModelLimits map[string]int

	semsMu sync.Mutex
	sems   map[string]chan struct{}
}

// clientFor returns the provider client a request to model should use:
// ClientFor's answer when a resolver is set, otherwise the Runner's single
// Client. Every request path in the loop resolves through here, so the same
// Runner can serve deepseek-v4-* and kimi-k3 requests from the pool without
// either provider's dialect leaking into the loop
// (docs/KIMI-INTEGRATION.md §4.3).
func (r *Runner) clientFor(model string) Client {
	if r.ClientFor != nil {
		if c := r.ClientFor(model); c != nil {
			return c
		}
	}
	return r.Client
}

// acquireModelSlot blocks until a concurrent-request slot for model is
// free, or ctx is done. The returned release func is always safe to call
// once; a caller with no limit configured gets a no-op.
func (r *Runner) acquireModelSlot(ctx context.Context, model string) (func(), error) {
	limit := r.ModelLimits[model]
	if limit <= 0 {
		return func() {}, nil
	}
	r.semsMu.Lock()
	if r.sems == nil {
		r.sems = make(map[string]chan struct{})
	}
	sem, ok := r.sems[model]
	if !ok {
		sem = make(chan struct{}, limit)
		r.sems[model] = sem
	}
	r.semsMu.Unlock()

	select {
	case sem <- struct{}{}:
		return func() { <-sem }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (r *Runner) flashModel(ctx context.Context) string {
	if r.FlashModel != "" {
		return r.FlashModel
	}
	if r.Settings != nil {
		if m, err := r.Settings.String(ctx, settings.KeyDefaultFlashModel); err == nil && m != "" {
			return m
		}
	}
	return "deepseek-flash"
}

// compactionThreshold resolves the prompt-token ceiling at which a session
// on model compacts its history: the Runner-level override, the model's own
// threshold when it has one (run.compaction_threshold_kimi_k3), the global
// run.compaction_threshold, then the package default.
func (r *Runner) compactionThreshold(ctx context.Context, model string) int {
	if r.CompactionThresholdTokens != 0 {
		return r.CompactionThresholdTokens
	}
	if r.Settings != nil {
		if key, ok := settings.CompactionKeyForModel(model); ok {
			if v, err := r.Settings.Int(ctx, key); err == nil {
				return v
			}
		}
		if v, err := r.Settings.Int(ctx, settings.KeyRunCompactionThreshold); err == nil {
			return v
		}
	}
	if _, ok := settings.CompactionKeyForModel(model); ok {
		return KimiK3CompactionThresholdTokens
	}
	return CompactionThresholdTokens
}
