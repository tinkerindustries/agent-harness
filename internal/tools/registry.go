// Package tools implements the eleven tools in docs/TOOLS.md: schemas that
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

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
	"github.com/mrgeoffrich/deepseek-harness/internal/pricing"
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
)

// Result is what one tool execution returns. Content is what goes back to
// the model as the tool message; IsError and Truncated are metadata the
// runner uses to build the store event.
type Result struct {
	Content   string
	IsError   bool
	Truncated bool
}

func errorResult(format string, args ...any) Result {
	return Result{Content: fmt.Sprintf(format, args...), IsError: true}
}

// truncate caps s at n bytes and labels the result if it cut anything.
func truncate(s string, n int) (string, bool) {
	if len(s) <= n {
		return s, false
	}
	return s[:n] + fmt.Sprintf("\n\n[truncated: %d of %d bytes shown]", n, len(s)), true
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
	BashDefault time.Duration
	BashMax     time.Duration
	Tool        time.Duration
	WebFetch    time.Duration
	Task        time.Duration
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

	// RunSubagent executes Task by delegating to the session loop. It is
	// injected by internal/session, which imports internal/tools; tools
	// cannot import session directly without a cycle.
	RunSubagent func(ctx context.Context, description, prompt, subagentType string) (string, error)

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
	"Read":      execRead,
	"Write":     execWrite,
	"Edit":      execEdit,
	"Bash":      execBash,
	"Glob":      execGlob,
	"Grep":      execGrep,
	"List":      execList,
	"TodoWrite": execTodoWrite,
	"Task":      execTask,
	"WebFetch":  execWebFetch,
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
