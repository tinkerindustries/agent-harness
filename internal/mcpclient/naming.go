package mcpclient

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/mrgeoffrich/agent-harness/internal/tools"
)

// maxToolNameLen is the function-name limit both DeepSeek and Kimi enforce
// (docs/MCP.md, "Naming").
const maxToolNameLen = 64

// hashSuffixLen is how much of the SHA-256 hex digest a disambiguating
// suffix carries — six hex characters, 24 bits, plenty to keep a handful of
// long or colliding names apart within one server without eating much of
// the 64-character budget.
const hashSuffixLen = 6

// QualifyToolName turns one MCP tool's own name, as server reported it,
// into the "mcp__<server>__<tool>" name the model is offered (docs/MCP.md,
// "Naming"): the tool name sanitised to [A-Za-z0-9_-] — every other rune
// becoming a single "_" — capped at maxToolNameLen.
//
// A name that would exceed the cap keeps the prefix and server intact,
// truncates the sanitised tool part, and appends "_" plus the first six hex
// characters of the SHA-256 of the *original*, unsanitised tool name. That
// keeps truncation deterministic — probing the same server twice produces
// the same qualified names — and keeps two long names that happen to share
// a sanitised prefix distinct, since the hash is of the whole original name
// rather than of the part that got cut off.
func QualifyToolName(server, tool string) string {
	prefix := tools.MCPToolPrefix + server + "__"
	sanitised := sanitiseToolName(tool)
	if len(prefix)+len(sanitised) <= maxToolNameLen {
		return prefix + sanitised
	}
	suffix := "_" + hashSuffix(tool)
	room := maxToolNameLen - len(prefix) - len(suffix)
	if room < 0 {
		room = 0
	}
	if room > len(sanitised) {
		room = len(sanitised)
	}
	return prefix + sanitised[:room] + suffix
}

// QualifyToolNames applies QualifyToolName across every tool name one probe
// of server returned, in order, and resolves a collision after sanitising
// the same way QualifyToolName resolves an over-long name: the second and
// subsequent tool to land on an already-used qualified name gets "_" plus
// the first six hex characters of the SHA-256 of its own original name
// appended (truncating first if the result would exceed maxToolNameLen).
// The disambiguation depends only on the colliding tool's own name, not on
// how many collisions came before it in the list, so it stays deterministic
// across re-probes even if the server reorders its own tool list.
func QualifyToolNames(server string, toolNames []string) []string {
	out := make([]string, len(toolNames))
	seen := make(map[string]bool, len(toolNames))
	for i, name := range toolNames {
		q := QualifyToolName(server, name)
		if seen[q] {
			q = disambiguate(q, name)
		}
		seen[q] = true
		out[i] = q
	}
	return out
}

// disambiguate appends a hash-of-original-name suffix to a qualified name
// that has already been claimed by an earlier tool in the same server's
// list, truncating first if the result would not otherwise fit.
func disambiguate(qualified, original string) string {
	suffix := "_" + hashSuffix(original)
	room := maxToolNameLen - len(suffix)
	if room < 0 {
		room = 0
	}
	if room < len(qualified) {
		qualified = qualified[:room]
	}
	return qualified + suffix
}

// sanitiseToolName replaces every rune outside [A-Za-z0-9_-] with a single
// "_", operating rune-by-rune (rather than byte-by-byte) so one non-ASCII
// character in a tool's name becomes one "_" rather than several.
func sanitiseToolName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if isLegalToolNameRune(r) {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	return string(out)
}

func isLegalToolNameRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '_' || r == '-':
		return true
	default:
		return false
	}
}

// hashSuffix is the first hashSuffixLen hex characters of the SHA-256 of
// name.
func hashSuffix(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:hashSuffixLen]
}
