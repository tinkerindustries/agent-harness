// Package tools implements the twenty-two tools in docs/TOOLS.md: schemas that
// match the trained-in shape, argument validation in Go, workspace
// confinement, per-tool timeouts and output caps, and the permission policy
// that gates execution without ever changing which tools are on offer
// (docs/CACHE.md).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mrgeoffrich/agent-harness/internal/gemini"
	"github.com/mrgeoffrich/agent-harness/internal/pricing"
	"github.com/mrgeoffrich/agent-harness/internal/settings"
	"github.com/mrgeoffrich/agent-harness/internal/store"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// Output and timeout limits, the built-in defaults when no settings resolver
// is attached (the test path). Production resolves the same values through
// the settings registry — internal/settings carries each of these as the
// default of its tools.* key, and the two are pinned equal by
// internal/settings/registry_test.go — so an operator can change any of them
// from the settings table without a rebuild (docs/TOOLS.md, "Execution
// rules": "Every tool has a wall-clock timeout and an output byte cap, with
// truncation labelled in the result").
const (
	DefaultOutputCap   = 200_000
	DefaultBashTimeout = 2 * time.Minute
	MaxBashTimeout     = 10 * time.Minute
	// DefaultBashWaitDelay bounds how long Bash waits for a command's output
	// pipes to close after the command exits or is cancelled, before it stops
	// waiting and kills the process group (docs/TOOLS.md, "Bash").
	DefaultBashWaitDelay = 2 * time.Second
	DefaultToolTimeout   = 30 * time.Second
	WebFetchTimeout      = 45 * time.Second
	TaskTimeout          = 10 * time.Minute
	// ReviewScreenshotTimeout bounds one Gemini call made by Glance, Ground,
	// or Detect: describing or locating something across up to four images
	// routinely takes longer than the 30-second default tool timeout, so
	// these three get their own (docs/TOOLS.md). The name predates the tools
	// it now bounds — it was ReviewScreenshot's alone — and is kept rather
	// than renamed to match: it is also a stored setting key
	// (tools.reviewscreenshot_timeout), and renaming it would be a silent
	// config reset for every operator who has set one. Crop makes no model
	// call and is not in this group; it keeps the default tool timeout.
	// 120s rather than 60s: a Glance asked to describe a whole page at medium
	// thinking produced 1142 output tokens, 438 of them thinking, and hit the
	// 60s ceiling mid-stream on a live run (sess-24bae5fa, seq 39). The model
	// retried and the retry cost a second full call, so the ceiling was buying
	// a stuck-call guard that a real answer was already exceeding.
	ReviewScreenshotTimeout = 120 * time.Second
	// TranscribeTimeout bounds one Transcribe call — every chunk of it, not
	// one Gemini request. A tall page is several calls, sent concurrently in
	// waves of tools.transcribe_concurrency, so the wall time is a few of
	// ReviewScreenshotTimeout rather than the sum of all of them; this is
	// sized for the slowest wave of a page at the chunk cap, plus the local
	// decode and re-encode of the chunks themselves, which a 12,000-pixel
	// capture makes non-trivial.
	TranscribeTimeout = 300 * time.Second
	// ScreenshotTimeout bounds one capture: launching Chromium, navigating,
	// waiting for the page to settle and encoding the image do not fit in the
	// 30-second default, and the driver's own navigation timeout is derived
	// from this one so a slow page fails with a message rather than being
	// killed silently (docs/TOOLS.md, "Screenshot").
	ScreenshotTimeout = 90 * time.Second
	// DefaultMCPTimeout bounds one MCP tool call (docs/MCP.md, "Calling").
	// Longer than DefaultToolTimeout: the calls that motivated MCP support —
	// rendering a viewport, driving an external application through a
	// browser — routinely run past the harness's own 30-second default for
	// its built-in tools.
	DefaultMCPTimeout = 120 * time.Second
)

// Result is what one tool execution returns. Content is what goes back to
// the model as the tool message; IsError and Truncated are metadata the
// runner uses to build the store event. Diff and ChildSessionID ride along
// for the two tools that have something structured to add on top of Content
// — Edit's line-level diff and Task's spawned session id — so a client
// can shape their blocks without re-deriving either from prose: one shape
// per tool (docs/TOOLS.md).
type Result struct {
	Content        string
	IsError        bool
	Truncated      bool
	Diff           []store.DiffLine
	ChildSessionID string
	// ImageURL is the base64 data URI of an image Read returned as an
	// image_url part, set only on a vision provider (docs/KIMI-INTEGRATION.md
	// §4.5). It never rides with IsError: a Read that fails is a refusal with
	// a text result, not a broken image part. The runner stores Content and
	// ImageURL on the tool_result event and the fold rebuilds the parts array
	// from them (internal/fold/fold.go), so the image bytes live in the event
	// log and a replay reproduces them identically.
	ImageURL string
	// GeminiUsage is the costed token accounting of a Glance, Ground,
	// Detect, or Transcribe call, set only when the tool made a successful
	// request to Gemini (Crop never sets it: it makes no model call). The
	// runner commits it as its own usage event (internal/session/turn.go),
	// so a Gemini call shows up in the session's cost total exactly the way
	// a DeepSeek turn's usage does.
	//
	// One payload, and therefore one event, even for a tool that made many
	// calls: Transcribe sends a request per chunk and sums them here, with
	// Calls recording how many, because the transcript card absorbs one
	// usage block per sub-turn and a later one replaces an earlier one
	// (internal/tools/transcribe.go, sumTranscribeUsage, has the whole
	// argument). A tool that adds a second Gemini call to a result inherits
	// that decision and must sum too, or the card will show the price of
	// whichever call happened to be last.
	GeminiUsage *store.UsagePayload
}

func errorResult(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// truncate caps s at n bytes and labels the result if it cut anything. The
// cut backs off to a rune boundary: s[:n] at an arbitrary byte index can
// leave a partial multi-byte rune at the tail, and encoding/json replaces
// invalid UTF-8 with U+FFFD when it marshals the request, so the model would
// silently receive a mangled final character rather than an error. The
// reported byte count is what was actually kept, not n.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	cut := s[:n]
	for len(cut) > 0 {
		// DecodeLastRuneInString reports (RuneError, 1) for an invalid
		// encoding; a real U+FFFD in the text decodes with size 3, so the
		// size check keeps this from eating one the caller meant to send.
		if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + fmt.Sprintf("\n\n[truncated: %d of %d bytes shown]", len(cut), len(s)), true
}

// CompletePayload is the parsed, schema-validated argument set from a
// successful Complete call.
type CompletePayload struct {
	Summary string
	Result  json.RawMessage
	Status  string
}

// Outcome is what Executor.Execute returns for one tool call.
type Outcome struct {
	Denied          bool
	Rule            string
	Result          Result
	Name            string
	IsComplete      bool
	CompletePayload CompletePayload
}

// Timeouts overrides the package defaults; a zero value in any field keeps
// the default.
type Timeouts struct {
	BashDefault      time.Duration
	BashMax          time.Duration
	BashWaitDelay    time.Duration
	Tool             time.Duration
	WebFetch         time.Duration
	Task             time.Duration
	ReviewScreenshot time.Duration
	Screenshot       time.Duration
	Transcribe       time.Duration
	MCP              time.Duration
}

// ChatClient is the narrow seam the executor's own model calls use —
// WebFetch's summarisation — declared here, where it is consumed, and
// implemented by the provider clients; *deepseek.Client implements it today
// and Kimi K3's client will tomorrow. It is the same shape as
// internal/session's Client seam (docs/KIMI-INTEGRATION.md §4.1): the
// caller states intent (wire.ChatIntent) and the implementation spells its
// provider's request. The session hands its own provider client through,
// so the dynamic type implements this smaller interface.
type ChatClient interface {
	CreateChatCompletion(ctx context.Context, intent wire.ChatIntent) (*wire.ChatCompletionResponse, error)
}

// Executor runs tools against one session's mutable state. An Executor
// belongs to exactly one session; nothing on it is shared across sessions
// (docs/DESIGN.md §4.5).
type Executor struct {
	Workspace    string
	Policy       *Policy
	ResultSchema json.RawMessage
	OutputCap    int
	Timeouts     Timeouts

	Client     ChatClient
	Prices     *pricing.Table
	FlashModel string

	// SeeImages is true when the session's model reads images natively
	// (Kimi K3, Gemini, and deepseek-flash; deepseek-v4-pro does not,
	// docs/DEEPSEEK-VISION.md). Read consults it: on a
	// vision-capable model, reading an image path returns the file as an
	// image_url part instead of the binary-file refusal, and the vision
	// tools that exist only because a model cannot see an image itself
	// (Screenshot, Glance, Ground, Detect, Transcribe, Crop) are not offered
	// at all (internal/tools/definitions.go, docs/KIMI-INTEGRATION.md §4.5,
	// docs/GEMINI-INTEGRATION.md §5.7). Set by internal/session from the
	// session's model at creation and on resume, and never changed
	// mid-session — the tool array and Read's behaviour are both frozen for
	// a session's life (docs/CACHE.md).
	SeeImages bool

	// Gemini is the client Glance, Ground, and Detect use to send images to
	// Google's Gemini API (Crop makes no model call). Nil (a session with no
	// client configured) makes a tool return an ordinary error result saying
	// so, the same shape WebFetch uses for a nil Client — it never panics and
	// never fails the run.
	Gemini *gemini.Client

	// MCP is the seam to every configured MCP server (internal/mcpclient
	// implements it), the one Execute routes a call whose name carries the
	// mcp__ prefix through instead of toolFuncs (docs/MCP.md, "Calling").
	// Nil — no MCP client wired, the shape every existing test and a caller
	// with none wired take — makes such a call return an ordinary error
	// result naming the tool, the same shape WebFetch and Gemini use for a
	// nil dependency: never a panic, never a failed run.
	MCP MCPProvider

	// GeminiModel resolves the vision model name per call, the same
	// read-through-the-store shape as the DeepSeek API key provider, so a
	// model changed in the settings table (google.vision_model) takes
	// effect without a restart. Nil falls back to gemini.DefaultModel.
	GeminiModel func() (string, error)

	// RunSubagent executes Task by delegating to the session loop. It is
	// injected by internal/session, which imports internal/tools; tools
	// cannot import session directly without a cycle. The returned session id
	// is the subagent's own session row — a distinct id from this Executor's
	// session, linked to it as parent (docs/DESIGN.md §4.7) — so a client
	// can render it as a collapsed child transcript.
	RunSubagent func(ctx context.Context, description, prompt, subagentType string) (summary string, sessionID string, err error)

	// ExtraEnv, when set, is asked for environment variables to add to
	// every Bash call's subprocess, resolved at the moment of the call
	// rather than held on the Executor. It exists for the GitHub App
	// credential: an App has no single standing token, so GH_TOKEN cannot be
	// made ambient for the whole process the way a personal access token is
	// — it has to be minted for the account this
	// session's repositories belong to, and re-minted as it expires, which
	// only a call-time resolution can do. The closure is built in
	// cmd/harness, which is where the App provider lives; nil is every other
	// caller, and a Bash call then inherits this process's environment
	// unchanged, exactly as it always has.
	ExtraEnv func(ctx context.Context, workspace string) []string

	// EnvFilter, when set, is applied to this process's own environment
	// before a Bash call's subprocess inherits it, in place of the
	// unfiltered os.Environ() a nil cmd.Env means. It exists for
	// `harness stdio-session`: a hosted session's parent supplies
	// GEMINI_API_KEY (or GOOGLE_API_KEY) only so this process's own API
	// client can reach Google, and a Bash call running "as this process's
	// own user, with the parent's environment" (docs/STDIO-PROTOCOL.md,
	// "Permissions") has no business seeing it. Nil is every other caller —
	// an operator-configured session among them — and a
	// Bash call then inherits this process's environment unchanged, exactly
	// as it always has.
	EnvFilter func(base []string) []string

	// RG is the ripgrep binary Grep execs, resolved once at startup by
	// RipgrepPath from -rg, then AGENT_HARNESS_RG, then the PATH. Empty means
	// none was found, and Grep falls back to its own walk — the same output
	// for the same call, except that the walk cannot filter by file type
	// (internal/tools/grep.go, docs/TOOLS.md).
	RG string

	// Settings, when set, is where the tool limits (output caps, timeouts,
	// WebFetch and vision bounds) resolve from on every call, so a limit
	// changed in the settings table (tools.*) takes effect on the next
	// tool call without a restart. Nil is the test path: the package
	// constants below apply.
	Settings *settings.Resolver

	readsMu sync.Mutex
	reads   map[string]bool

	todosMu    sync.Mutex
	todos      []Todo
	nextTaskID int

	// shellsMu guards the background shells Bash started with
	// run_in_background: true. Ids are minted in call order from
	// nextShellID, the same shape nextTaskID mints TaskCreate's — belongs to
	// the Executor because the Executor belongs to exactly one session
	// (docs/DESIGN.md §4.5), so a shell's id is only ever read back by
	// BashOutput or KillBash calls on that same session.
	shellsMu    sync.Mutex
	shells      map[string]*backgroundShell
	nextShellID int

	// mcpImageMu guards the counter that makes each image an MCP tool
	// returns land on its own path. Naming a file from the call's own
	// arguments alone is not enough: calling one tool twice — render, look,
	// change something, render again — would write both images to the same
	// name, so the second silently replaces the first and the transcript's
	// earlier reference starts pointing at bytes that are not what it
	// showed. The counter belongs to the Executor because an Executor
	// belongs to exactly one session (docs/DESIGN.md §4.5), which is also
	// what keeps the numbers readable rather than globally unique.
	mcpImageMu   sync.Mutex
	nextMCPImage int

	// Glance conversation state, held here because an Executor belongs to
	// exactly one session (docs/DESIGN.md §4.5). See glanceConversation.
	glanceMu            sync.Mutex
	glanceConversations map[string]*glanceConversation
	glanceOrder         []string
}

// NewExecutor returns an Executor rooted at workspace (resolved to an
// absolute, symlink-resolved path) under policy.
func NewExecutor(workspace string, policy *Policy) (*Executor, error) {
	root, err := ResolvePath(workspace, ".")
	if err != nil {
		return nil, fmt.Errorf("tools: resolve workspace %q: %w", workspace, err)
	}
	return &Executor{
		Workspace: root,
		Policy:    policy,
		reads:     make(map[string]bool),
	}, nil
}

// Close ends every background shell this Executor's session started with
// Bash's run_in_background that is still running. A background shell is a
// live process outliving the tool call that started it, so nothing else
// kills it once the run's own goroutine returns — the same "never leave an
// agent behind" rule the harness holds itself to for a spawned session
// applies to a spawned process (CLAUDE.md). internal/session calls this once
// per run, after the loop returns on every path (docs/RUN-CONTROL.md).
func (e *Executor) Close() {
	e.shellsMu.Lock()
	shells := make([]*backgroundShell, 0, len(e.shells))
	for _, bg := range e.shells {
		shells = append(shells, bg)
	}
	e.shellsMu.Unlock()

	for _, bg := range shells {
		bg.mu.Lock()
		done := bg.done
		bg.mu.Unlock()
		if !done {
			bg.killer.forceKill()
		}
	}
}

func (e *Executor) outputCap(ctx context.Context) int {
	if e.OutputCap > 0 {
		return e.OutputCap
	}
	if e.Settings != nil {
		if v, err := e.Settings.Int(ctx, settings.KeyToolOutputCap); err == nil {
			return v
		}
	}
	return DefaultOutputCap
}

func (e *Executor) timeoutFor(ctx context.Context, name string, argsRaw json.RawMessage) time.Duration {
	// An MCP tool's name is dynamic — server and tool names an operator
	// configured, not a literal the switch below can case on — so it is
	// checked ahead of the switch rather than folded into it.
	if _, ok := MCPServerOf(name); ok {
		return e.mcpTimeout(ctx)
	}
	switch name {
	case "Bash":
		def, max := e.bashTimeouts(ctx)
		var args bashArgs
		if json.Unmarshal(argsRaw, &args) == nil && args.TimeoutMS > 0 {
			requested := time.Duration(args.TimeoutMS) * time.Millisecond
			if requested < max {
				return requested
			}
			return max
		}
		return def
	case "WebFetch":
		if e.Timeouts.WebFetch > 0 {
			return e.Timeouts.WebFetch
		}
		if e.Settings != nil {
			if v, err := e.Settings.Duration(ctx, settings.KeyToolWebFetchTimeout); err == nil {
				return v
			}
		}
		return WebFetchTimeout
	case "Task":
		if e.Timeouts.Task > 0 {
			return e.Timeouts.Task
		}
		if e.Settings != nil {
			if v, err := e.Settings.Duration(ctx, settings.KeyToolTaskTimeout); err == nil {
				return v
			}
		}
		return TaskTimeout
	case "Glance", "Ground", "Detect":
		// Crop makes no model call, so it keeps the default tool timeout below
		// rather than this one — it has no Gemini round-trip to bound.
		if e.Timeouts.ReviewScreenshot > 0 {
			return e.Timeouts.ReviewScreenshot
		}
		if e.Settings != nil {
			if v, err := e.Settings.Duration(ctx, settings.KeyToolReviewScreenshotTimeout); err == nil {
				return v
			}
		}
		return ReviewScreenshotTimeout
	case "Transcribe":
		// Not the Glance timeout: Transcribe's budget has to cover every
		// chunk of an image, and inheriting a per-call one would kill a tall
		// page part-way through and bill for the chunks that had already
		// come back.
		if e.Timeouts.Transcribe > 0 {
			return e.Timeouts.Transcribe
		}
		if e.Settings != nil {
			if v, err := e.Settings.Duration(ctx, settings.KeyToolTranscribeTimeout); err == nil {
				return v
			}
		}
		return TranscribeTimeout
	case "Screenshot":
		return e.screenshotTimeout(ctx)
	default:
		if e.Timeouts.Tool > 0 {
			return e.Timeouts.Tool
		}
		if e.Settings != nil {
			if v, err := e.Settings.Duration(ctx, settings.KeyToolTimeout); err == nil {
				return v
			}
		}
		return DefaultToolTimeout
	}
}

// mcpTimeout returns the wall-clock bound on one MCP call: tools.mcp_timeout,
// resolved through settings exactly like every other tool's timeout, longer
// by default than DefaultToolTimeout because the calls that motivated MCP
// support drive external applications — rendering a viewport, driving a
// browser — which routinely run past the harness's own tools' 30-second
// default (docs/MCP.md, "Calling").
func (e *Executor) mcpTimeout(ctx context.Context) time.Duration {
	if e.Timeouts.MCP > 0 {
		return e.Timeouts.MCP
	}
	if e.Settings != nil {
		if v, err := e.Settings.Duration(ctx, settings.KeyToolMCPTimeout); err == nil {
			return v
		}
	}
	return DefaultMCPTimeout
}

// bashTimeouts returns the Bash default and ceiling timeouts, honouring an
// explicit Timeouts field first, then the settings, then the constants.
func (e *Executor) bashTimeouts(ctx context.Context) (def, max time.Duration) {
	def, max = DefaultBashTimeout, MaxBashTimeout
	if e.Timeouts.BashDefault > 0 {
		def = e.Timeouts.BashDefault
	}
	if e.Timeouts.BashMax > 0 {
		max = e.Timeouts.BashMax
	}
	if e.Settings != nil {
		if v, err := e.Settings.Duration(ctx, settings.KeyToolBashTimeout); err == nil {
			def = v
		}
		if v, err := e.Settings.Duration(ctx, settings.KeyToolBashTimeoutMax); err == nil {
			max = v
		}
	}
	return def, max
}

// bashWaitDelay returns how long Bash keeps waiting for a command's output
// pipes to close after the command exits or is cancelled before it gives up,
// honours an explicit Timeouts field first, then the settings, then the
// constant. The registry enforces the bounds on write, so like the timeout
// resolvers above this one does not clamp.
func (e *Executor) bashWaitDelay(ctx context.Context) time.Duration {
	delay := DefaultBashWaitDelay
	if e.Timeouts.BashWaitDelay > 0 {
		delay = e.Timeouts.BashWaitDelay
	}
	if e.Settings != nil {
		if v, err := e.Settings.Duration(ctx, settings.KeyToolBashWaitDelay); err == nil {
			delay = v
		}
	}
	return delay
}

// markRead records that path (already workspace-resolved) has been read
// this session, satisfying the prior-read gate on Write and Edit
// (docs/TOOLS.md).
func (e *Executor) markRead(path string) {
	e.readsMu.Lock()
	defer e.readsMu.Unlock()
	e.reads[path] = true
}

func (e *Executor) wasRead(path string) bool {
	e.readsMu.Lock()
	defer e.readsMu.Unlock()
	return e.reads[path]
}

type toolFunc func(ctx context.Context, e *Executor, args json.RawMessage) Result

var toolFuncs = map[string]toolFunc{
	"Read":       execRead,
	"Write":      execWrite,
	"Edit":       execEdit,
	"Bash":       execBash,
	"BashOutput": execBashOutput,
	"KillBash":   execKillBash,
	"Glob":       execGlob,
	"Grep":       execGrep,
	"List":       execList,
	"TaskCreate": execTaskCreate,
	"TaskGet":    execTaskGet,
	"TaskList":   execTaskList,
	"TaskUpdate": execTaskUpdate,
	"Task":       execTask,
	"WebFetch":   execWebFetch,
	"Glance":     execGlance,
	"Ground":     execGround,
	"Detect":     execDetect,
	"Crop":       execCrop,
	"Transcribe": execTranscribe,
	"Screenshot": execScreenshot,
	// The four fixed MCP tools (mcpresources.go). They are in this table
	// unconditionally — a name the model was never offered is a name it
	// cannot call — while whether they are *offered* is WithMCP's decision.
	"MCPListResources": execMCPListResources,
	"MCPReadResource":  execMCPReadResource,
	"MCPListPrompts":   execMCPListPrompts,
	"MCPGetPrompt":     execMCPGetPrompt,
}

// toolArgs turns the argument text a model emitted into a JSON object. A call
// that needs no arguments may carry no arguments field at all, which arrives
// as the empty string, and every tool below unmarshals what it is handed: an
// empty string is "unexpected end of JSON input", a parse error naming a
// position in a byte stream the model cannot see. A call with no arguments is
// the call with the empty object, which lets a tool whose fields are all
// optional run and lets one with a required field refuse it by name.
func toolArgs(s string) json.RawMessage {
	if strings.TrimSpace(s) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}

// Execute evaluates permission for call, then runs it (or Complete's
// validation) under a per-tool timeout derived from ctx.
func (e *Executor) Execute(ctx context.Context, call wire.ToolCall) Outcome {
	name := call.Function.Name
	argsRaw := toolArgs(call.Function.Arguments)
	descriptor := descriptorFor(name, argsRaw)

	decision := e.Policy.Check(name, descriptor)
	if !decision.Allow {
		return Outcome{
			Denied: true,
			Rule:   decision.Rule,
			Name:   name,
			Result: Result{
				Content: fmt.Sprintf("Denied by permission policy (%s mode): %s", e.Policy.Mode, decision.Rule),
				IsError: true,
			},
		}
	}

	limit := e.timeoutFor(ctx, name, argsRaw)
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	ctx = WithCallID(ctx, call.ID)
	ctx = withToolLimit(ctx, limit)

	// An MCP call is routed to the configured provider instead of
	// toolFuncs, inside the same per-tool timeout and after the same policy
	// check every other tool just passed (docs/MCP.md, "Calling").
	if server, ok := MCPServerOf(name); ok {
		return Outcome{Name: name, Result: e.execMCP(ctx, server, name, argsRaw)}
	}

	if name == "Complete" {
		res, payload, ok := e.execComplete(argsRaw)
		return Outcome{Result: res, Name: name, IsComplete: ok, CompletePayload: payload}
	}

	fn, known := toolFuncs[name]
	if !known {
		return Outcome{Name: name, Result: errorResult("unknown tool %q", name)}
	}
	return Outcome{Name: name, Result: fn(ctx, e, argsRaw)}
}

// toolLimitKey is the context key the running call's own wall-clock limit is
// attached under.
type toolLimitKey struct{}

// withToolLimit attaches the timeout Execute actually applied. A tool that
// reports its own expiry needs the limit that bit, and that is not always the
// one the model asked for: timeoutFor clamps a Bash request down to
// tools.bash_timeout_max, and substitutes tools.bash_timeout for a call that
// named none. docs/CACHE.md keeps both numbers out of the tool description, so
// the expiry message is the only place the model can learn what the bound
// really was.
func withToolLimit(ctx context.Context, limit time.Duration) context.Context {
	return context.WithValue(ctx, toolLimitKey{}, limit)
}

// toolLimitFrom returns the limit Execute applied, and whether one was set at
// all. A tool reached other than through Execute — a test driving the function
// directly — carries no limit, and has to fall back to ctx's own deadline.
func toolLimitFrom(ctx context.Context) (time.Duration, bool) {
	limit, ok := ctx.Value(toolLimitKey{}).(time.Duration)
	return limit, ok
}

// callIDKey is the context key the running call's own id is attached under.
type callIDKey struct{}

// WithCallID attaches the tool call's id to ctx. Execute sets it around every
// call, so anything reached from inside one can name the call it is serving
// without the id being threaded through a signature that no other
// implementation needs. It exists for the MCPProvider that answers a call by
// asking the process's parent to run it (internal/stdiosession): the parent
// has already been told about the call under this id, and a result it cannot
// tie back to that id cannot be rendered against it.
func WithCallID(ctx context.Context, id string) context.Context {
	if id == "" {
		return ctx
	}
	return context.WithValue(ctx, callIDKey{}, id)
}

// CallIDFrom returns the tool call id Execute attached to ctx, and "" when
// there is none — every call from a test or a path that did not go through
// Execute.
func CallIDFrom(ctx context.Context) string {
	id, _ := ctx.Value(callIDKey{}).(string)
	return id
}

// stdoutSinkKey is the context key a live-output sink is attached under.
// Unexported so only WithStdoutSink can set it and only stdoutSinkFromContext
// can read it.
type stdoutSinkKey struct{}

// WithStdoutSink attaches sink to ctx so a tool that produces incremental
// output can forward it as it runs, ahead of the final Result. Bash and MCP
// tool calls are the callers; session builds one sink per tool call, keyed
// to that call's tool_call_id, before invoking Execute. A context with no
// sink attached — a CLI run with no hub, every test — makes the forwarding
// a silent no-op (docs/DESIGN.md §5.2, "streaming command output").
func WithStdoutSink(ctx context.Context, sink func(chunk string)) context.Context {
	return context.WithValue(ctx, stdoutSinkKey{}, sink)
}

func stdoutSinkFromContext(ctx context.Context) func(string) {
	sink, _ := ctx.Value(stdoutSinkKey{}).(func(string))
	return sink
}

// StdoutSinkFrom is stdoutSinkFromContext for the one caller outside this
// package: internal/mcpclient, which forwards a server's progress
// notifications down the same channel a running Bash command's output uses
// (docs/MCP.md, "What a server sends back unasked"). Nil when the context
// carries no sink.
func StdoutSinkFrom(ctx context.Context) func(string) {
	return stdoutSinkFromContext(ctx)
}
