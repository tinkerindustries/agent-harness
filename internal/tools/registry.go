// Package tools implements the twelve tools in docs/TOOLS.md: schemas that
// match the trained-in shape, argument validation in Go, workspace
// confinement, per-tool timeouts and output caps, and the permission policy
// that gates execution without ever changing which tools are on offer
// (docs/CACHE.md).
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/gemini"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
	"github.com/mrgeoffrich/deepseek-harness/internal/store"
)

// Output and timeout limits (docs/TOOLS.md, "Execution rules": "Every tool
// has a wall-clock timeout and an output byte cap, with truncation labelled
// in the result").
const (
	DefaultOutputCap   = 200_000
	DefaultBashTimeout = 2 * time.Minute
	MaxBashTimeout     = 10 * time.Minute
	DefaultToolTimeout = 30 * time.Second
	WebFetchTimeout    = 45 * time.Second
	TaskTimeout        = 10 * time.Minute
	// ReviewScreenshotTimeout bounds one Gemini call: generating a diagnosis
	// of up to four screenshots routinely takes longer than the 30-second
	// default tool timeout, so it gets its own (docs/TOOLS.md,
	// "ReviewScreenshot").
	ReviewScreenshotTimeout = 60 * time.Second
)

// Result is what one tool execution returns. Content is what goes back to
// the model as the tool message; IsError and Truncated are metadata the
// runner uses to build the store event. Diff and ChildSessionID ride along
// for the two tools that have something structured to add on top of Content
// — Edit's line-level diff and Task's spawned session id — so the browser
// can shape their blocks without re-deriving either from prose: one shape
// per tool (docs/TOOLS.md).
type Result struct {
	Content        string
	IsError        bool
	Truncated      bool
	Diff           []store.DiffLine
	ChildSessionID string
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
	Tool             time.Duration
	WebFetch         time.Duration
	Task             time.Duration
	ReviewScreenshot time.Duration
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

	Client     *deepseek.Client
	Prices     *pricing.Table
	FlashModel string

	// Gemini is the client the ReviewScreenshot tool uses to send screenshots
	// to Google's Gemini API. Nil (a session with no client configured) makes
	// the tool return an ordinary error result saying so, the same shape
	// WebFetch uses for a nil Client — it never panics and never fails the
	// run.
	Gemini *gemini.Client

	// GeminiModel resolves the vision model name per call, the same
	// read-through-the-store shape as the DeepSeek API key provider, so a
	// model changed with `harness config set google.vision_model` takes
	// effect without a restart. Nil falls back to gemini.DefaultModel.
	GeminiModel func() (string, error)

	// RunSubagent executes Task by delegating to the session loop. It is
	// injected by internal/session, which imports internal/tools; tools
	// cannot import session directly without a cycle. The returned session id
	// is the subagent's own session row — a distinct id from this Executor's
	// session, linked to it as parent (docs/DESIGN.md §4.7) — so the browser
	// can render it as a collapsed child transcript.
	RunSubagent func(ctx context.Context, description, prompt, subagentType string) (summary string, sessionID string, err error)

	readsMu sync.Mutex
	reads   map[string]bool

	todosMu sync.Mutex
	todos   []Todo
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
		OutputCap: DefaultOutputCap,
		reads:     make(map[string]bool),
	}, nil
}

func (e *Executor) outputCap() int {
	if e.OutputCap > 0 {
		return e.OutputCap
	}
	return DefaultOutputCap
}

func (e *Executor) timeoutFor(name string, argsRaw json.RawMessage) time.Duration {
	switch name {
	case "Bash":
		def := e.Timeouts.BashDefault
		if def == 0 {
			def = DefaultBashTimeout
		}
		max := e.Timeouts.BashMax
		if max == 0 {
			max = MaxBashTimeout
		}
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
		return WebFetchTimeout
	case "Task":
		if e.Timeouts.Task > 0 {
			return e.Timeouts.Task
		}
		return TaskTimeout
	case "ReviewScreenshot":
		if e.Timeouts.ReviewScreenshot > 0 {
			return e.Timeouts.ReviewScreenshot
		}
		return ReviewScreenshotTimeout
	default:
		if e.Timeouts.Tool > 0 {
			return e.Timeouts.Tool
		}
		return DefaultToolTimeout
	}
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
	"Read":             execRead,
	"Write":            execWrite,
	"Edit":             execEdit,
	"Bash":             execBash,
	"Glob":             execGlob,
	"Grep":             execGrep,
	"List":             execList,
	"TodoWrite":        execTodoWrite,
	"Task":             execTask,
	"WebFetch":         execWebFetch,
	"ReviewScreenshot": execReviewScreenshot,
}

// Execute evaluates permission for call, then runs it (or Complete's
// validation) under a per-tool timeout derived from ctx.
func (e *Executor) Execute(ctx context.Context, call deepseek.ToolCall) Outcome {
	name := call.Function.Name
	argsRaw := json.RawMessage(call.Function.Arguments)
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

	ctx, cancel := context.WithTimeout(ctx, e.timeoutFor(name, argsRaw))
	defer cancel()

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

// stdoutSinkKey is the context key a live-output sink is attached under.
// Unexported so only WithStdoutSink can set it and only stdoutSinkFromContext
// can read it.
type stdoutSinkKey struct{}

// WithStdoutSink attaches sink to ctx so a tool that produces incremental
// output can forward it as it runs, ahead of the final Result. Bash is
// currently the only caller; session builds one sink per tool call, keyed to
// that call's tool_call_id, before invoking Execute. A context with no sink
// attached — every existing caller, every test — makes the forwarding a
// silent no-op (docs/DESIGN.md §5.2, "streaming command output").
func WithStdoutSink(ctx context.Context, sink func(chunk string)) context.Context {
	return context.WithValue(ctx, stdoutSinkKey{}, sink)
}

func stdoutSinkFromContext(ctx context.Context) func(string) {
	sink, _ := ctx.Value(stdoutSinkKey{}).(func(string))
	return sink
}
