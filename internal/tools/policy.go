package tools

import "strings"

// Mode is the permission mode a session holds for its whole life
// (docs/TOOLS.md, "Permissions"). Modes gate execution, never the tool
// array sent to the model: all sixteen tools ship in every mode, and a
// disallowed call is refused at execution time.
type Mode string

const (
	ModeReadOnly Mode = "readonly"
	ModeFull     Mode = "full"
)

// Valid reports whether m is one of the two modes. There is no zero-value
// fallback: every ingress requires the mode explicitly, so an empty value
// is a rejected request rather than a silent choice (docs/TOOLS.md).
func (m Mode) Valid() bool {
	return m == ModeReadOnly || m == ModeFull
}

// Resolver is consulted for a call the policy would otherwise deny. The CLI
// registers one to prompt a terminal user; queue-driven sessions register
// none, so their denials are final (docs/DESIGN.md §4.6).
type Resolver func(toolName, descriptor string) bool

// Decision is the result of evaluating one tool call against a Policy.
type Decision struct {
	Allow bool
	// Rule names why, for both an allow ("read-only tools always run") and
	// a deny ("Bash denied in readonly mode"). Denials show it to the model.
	Rule string
}

// Policy is the permission policy a session is given at creation
// (docs/TOOLS.md, "Permissions"). It is immutable after construction; the
// Resolver field is the only thing that varies the outcome of a decision
// call to call.
type Policy struct {
	Mode Mode
	// Deny holds substring patterns matched against a call's descriptor
	// (docs/TOOLS.md does not pin the matching rule; substring-of-command
	// for Bash and substring-of-"Tool arg" otherwise is what this
	// implementation chose). Deny only ever
	// subtracts from what Mode allows; it can never widen it.
	Deny     []string
	Resolver Resolver
}

// alwaysAllowed tools have no side effects outside the session's own
// bookkeeping and run in every mode. ReviewScreenshot reads a file and sends
// it over the network without changing anything on disk — the same reasoning
// that puts WebFetch in read-only mode. The four task tools carry the same
// "the plan is the session's own bookkeeping" reasoning TodoWrite had, so
// read-only-mode sessions keep full plan tracking.
//
// Screenshot is the one member that does write to disk, and it is here
// because of where it is allowed to write: internal/tools/screenshot.go
// confines its output to the workspace's scratch/ directory, which by the
// system prompt's own definition holds nothing that is part of a run's
// deliverable. A read-only session can therefore capture and review a page —
// the whole point of a review run — without being able to leave a mark on a
// cloned repository. It spawns a browser, which Bash-in-readonly is denied
// for, but it spawns exactly one command it composed itself against a URL:
// there is no argument that turns it into arbitrary execution.
var alwaysAllowed = map[string]bool{
	"Read":             true,
	"Glob":             true,
	"Grep":             true,
	"List":             true,
	"WebFetch":         true,
	"ReviewScreenshot": true,
	"Screenshot":       true,
	"TaskCreate":       true,
	"TaskGet":          true,
	"TaskList":         true,
	"TaskUpdate":       true,
	"Complete":         true,
}

// Check evaluates one tool call. descriptor is what a deny pattern matches
// against and what a denial shows the model: the shell command for Bash,
// "ToolName argument" for path-taking tools, and the bare tool name
// otherwise.
func (p *Policy) Check(toolName, descriptor string) Decision {
	d := p.evaluate(toolName, descriptor)
	if d.Allow {
		return d
	}
	if p.Resolver != nil && p.Resolver(toolName, descriptor) {
		return Decision{Allow: true, Rule: "approved interactively"}
	}
	return d
}

func (p *Policy) evaluate(toolName, descriptor string) Decision {
	if rule, denied := p.matchDeny(descriptor); denied {
		return Decision{Allow: false, Rule: "denied by configured pattern: " + rule}
	}

	if alwaysAllowed[toolName] {
		return Decision{Allow: true, Rule: "always allowed"}
	}

	switch p.Mode {
	case ModeReadOnly:
		return Decision{Allow: false, Rule: toolName + " is not permitted in read-only mode"}

	case ModeFull:
		return Decision{Allow: true, Rule: "full access mode"}

	default:
		return Decision{Allow: false, Rule: "unknown permission mode " + string(p.Mode)}
	}
}

func (p *Policy) matchDeny(descriptor string) (string, bool) {
	for _, pattern := range p.Deny {
		if pattern == "" {
			continue
		}
		if strings.Contains(descriptor, pattern) {
			return pattern, true
		}
	}
	return "", false
}
