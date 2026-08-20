package tools

import (
	"fmt"
	"strings"
)

// Mode is the permission mode a session holds for its whole life
// (docs/TOOLS.md, "Permissions"). Modes gate execution, never the tool
// array sent to the model: all twenty tools ship in every mode, and a
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
	// MCPReadOnlyServers is the per-server read-only allowance
	// (docs/MCP.md, "Permissions"): a server named true here may be called
	// by a readonly-mode session even though it reaches outside the
	// workspace by definition. It is frozen on the policy at run start the
	// same way Mode and Deny are — Runner.Run and Runner.Resume take it from
	// tools.MCPProvider.Definitions once, never a live read of the
	// mcp_servers table mid-run — so a server an operator flips mid-run
	// cannot change what a running session is allowed to call. A server
	// absent from the map (no MCP configured, or one this policy's snapshot
	// never saw) behaves as false: readonly denies its tools.
	MCPReadOnlyServers map[string]bool
}

// alwaysAllowed tools have no side effects outside the session's own
// bookkeeping and run in every mode. Glance, Ground, Detect, and Transcribe
// read a file and send it over the network without changing anything on disk
// — the same reasoning that puts WebFetch in read-only mode. Transcribe
// makes several of those requests instead of one, which costs more but
// changes nothing about what it touches: it writes no chunk files, and the
// boundaries it reports are y positions in the source image rather than
// anything on disk. The four task tools carry
// the same "the plan is the session's own bookkeeping" reasoning TodoWrite
// had, so read-only-mode sessions keep full plan tracking.
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
//
// Crop writes to disk too, and gets the allowance for the same reason
// Screenshot does — the entry below says why, beside the entry itself.
var alwaysAllowed = map[string]bool{
	"Read":       true,
	"Glob":       true,
	"Grep":       true,
	"List":       true,
	"WebFetch":   true,
	"Glance":     true,
	"Ground":     true,
	"Detect":     true,
	"Transcribe": true,
	"Screenshot": true,
	// Crop writes a file, which is why it is worth saying why it sits
	// here beside the readers rather than with Write and Edit: its output
	// is confined to scratch/ by the same rule Screenshot's is
	// (resolveScratchImageOutput), so it cannot touch the deliverable or a
	// cloned repository. Gating it by mode instead put the whole
	// Ground-Crop-Glance pipeline behind full permissions, which is the
	// mode that also hands the session the host docker socket — a large
	// grant to buy a closer look at a screenshot.
	"Crop":       true,
	"TaskCreate": true,
	"TaskGet":    true,
	"TaskList":   true,
	"TaskUpdate": true,
	"Complete":   true,
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

	// An MCP call is gated by its own table (docs/MCP.md, "Permissions")
	// rather than by the mode switch below or by alwaysAllowed: full always
	// allows it, readonly allows it only when the server it belongs to was
	// probed with allow_readonly set, and it is denied otherwise with a rule
	// that names the server so the model knows what to route around.
	if server, ok := MCPServerOf(toolName); ok {
		switch p.Mode {
		case ModeFull:
			return Decision{Allow: true, Rule: "full access mode"}
		case ModeReadOnly:
			if p.MCPReadOnlyServers[server] {
				return Decision{Allow: true, Rule: fmt.Sprintf("MCP server %q allows read-only calls", server)}
			}
			return Decision{Allow: false, Rule: fmt.Sprintf("MCP server %q is not marked read-only; readonly mode denies its tools", server)}
		default:
			return Decision{Allow: false, Rule: "unknown permission mode " + string(p.Mode)}
		}
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
