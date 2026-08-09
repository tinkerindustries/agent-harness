package tools

import "strings"

// Mode is the permission mode a session holds for its whole life
// (docs/TOOLS.md, "Permissions"). Modes gate execution, never the tool
// array sent to the model: all eleven tools ship in every mode, and a
// disallowed call is refused at execution time.
type Mode string

const (
	ModeReadOnly Mode = "readonly"
	ModeDefault  Mode = "default"
	ModeFull     Mode = "full"
)

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
	// implementation chose — see the phase 2 report). Deny only ever
	// subtracts from what Mode allows; it can never widen it.
	Deny []string
	// BashAllowlist gates Bash in ModeDefault: the command's leading
	// executable name (and any executable after a shell connector such as
	// && or |) must appear here.
	BashAllowlist []string
	Resolver      Resolver
}

// alwaysAllowed tools have no side effects outside the session's own
// bookkeeping and run in every mode.
var alwaysAllowed = map[string]bool{
	"Read":      true,
	"Glob":      true,
	"Grep":      true,
	"List":      true,
	"WebFetch":  true,
	"TodoWrite": true,
	"Complete":  true,
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

	case ModeDefault:
		switch toolName {
		case "Write", "Edit", "Task":
			return Decision{Allow: true, Rule: "permitted in default mode"}
		case "Bash":
			if p.bashAllowed(descriptor) {
				return Decision{Allow: true, Rule: "command matches the configured allowlist"}
			}
			return Decision{Allow: false, Rule: "command is not on the configured Bash allowlist"}
		default:
			return Decision{Allow: false, Rule: toolName + " is not permitted in default mode"}
		}

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

// bashAllowed reports whether every executable named in command — the
// leading word of the command itself and of each segment after a shell
// connector (&&, ||, ;, |) — appears in the allowlist. It is a heuristic,
// not a shell parser: good enough to keep obviously-out-of-policy commands
// out, not a sandbox.
func (p *Policy) bashAllowed(command string) bool {
	allowed := make(map[string]bool, len(p.BashAllowlist))
	for _, a := range p.BashAllowlist {
		allowed[a] = true
	}
	for _, segment := range splitShellConnectors(command) {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		fields := strings.Fields(segment)
		if len(fields) == 0 {
			continue
		}
		exe := fields[0]
		if slash := strings.LastIndexByte(exe, '/'); slash >= 0 {
			exe = exe[slash+1:]
		}
		if !allowed[exe] {
			return false
		}
	}
	return true
}

func splitShellConnectors(command string) []string {
	replacer := strings.NewReplacer("&&", "\x00", "||", "\x00", ";", "\x00", "|", "\x00")
	return strings.Split(replacer.Replace(command), "\x00")
}

// DefaultBashAllowlist is a conservative set of read-and-build commands for
// ModeDefault. Config can override it.
var DefaultBashAllowlist = []string{
	// cd leads most commands a coding model writes. Matching checks every
	// connector-separated segment, so allowing it does not let the rest of a
	// chained command through.
	"cd",
	"git", "go", "npm", "npx", "yarn", "pnpm", "make", "python", "python3", "node",
	"ls", "cat", "echo", "mkdir", "cp", "mv", "grep", "find", "sed", "awk",
	"diff", "wc", "head", "tail", "sort", "uniq", "tree", "pwd", "which", "env",
	"true", "false", "test", "rg", "jq", "gofmt",
}
