// Package redact masks credential-shaped strings on their way out of the
// process to a human-facing surface.
//
// The problem it solves is that a session's tool output is whatever the
// commands it ran printed. An agent that needs a token in its container
// writes one into a file and may well read that file back, and the tool
// result is committed to the event log verbatim — a live run put a full
// github_pat_ value there with nothing more exotic than `head -2 .env`
// (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md). The event log is
// therefore a credential store, and the HTTP API serves it.
//
// Two things this deliberately is not:
//
// It is not applied on the write path. The model must see its own tool
// output exactly as the command produced it, or it cannot verify the file
// it just wrote; redacting before the log would corrupt the conversation
// the fold rebuilds from that same log (internal/fold). Redaction belongs
// at the boundary where events leave for a reader, not where they are
// recorded.
//
// It is not a guarantee. It matches credential formats with a distinctive
// high-entropy prefix — the shapes that are unambiguous enough to strip
// without eating legitimate output. A secret with no recognisable shape
// (a password, a bare hex key) passes straight through, and the honest
// summary is that this raises the cost of a leak rather than closing it.
// The real fix for a transcript surface exposed beyond loopback is
// authentication, which docs/RUN-CONTROL.md names as the open question.
package redact

import (
	"regexp"
	"strings"
)

// Placeholder replaces every match. It carries no tail of the original
// value: Secret shows the last four characters so an operator can tell two
// configured keys apart, which is a different job from this one — nobody
// needs to identify the token in a transcript, and four characters of a
// live credential is four more than the reader needs.
const Placeholder = "[redacted]"

// patterns are credential shapes whose prefix makes them unmistakable. Each
// one is anchored on a vendor's own literal marker, so a match is a token
// rather than a guess at one. None of them can match a JSON quote or
// backslash, which is what makes it safe to run this over a marshalled
// payload's bytes without reparsing it.
var patterns = regexp.MustCompile(`(?s)` +
	// GitHub: fine-grained PATs, classic PATs, OAuth, app, and refresh tokens.
	`github_pat_[A-Za-z0-9_]{20,}` +
	`|gh[pousr]_[A-Za-z0-9]{20,}` +
	// GitLab personal access tokens.
	`|glpat-[A-Za-z0-9_-]{16,}` +
	// OpenAI-format keys, which is the shape DeepSeek and Anthropic both use.
	`|sk-[A-Za-z0-9_-]{16,}` +
	// Google / Gemini API keys.
	`|AIza[0-9A-Za-z_-]{35}` +
	// Slack bot, user, app, and refresh tokens.
	`|xox[abprs]-[A-Za-z0-9-]{10,}` +
	// AWS access key ids, and the session token prefix.
	`|(?:AKIA|ASIA)[0-9A-Z]{16}` +
	// npm automation tokens.
	`|npm_[A-Za-z0-9]{36}` +
	// PEM private keys, whole block. Non-greedy so two keys in one payload
	// are two matches rather than one span swallowing what sits between them.
	`|-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

// String replaces every credential-shaped run in s with Placeholder.
func String(s string) string {
	return patterns.ReplaceAllLiteralString(s, Placeholder)
}

// Bytes is String over a byte slice. It returns b unchanged, not a copy,
// when nothing matched — the common case by far, and the events endpoint
// runs this over every payload it serves.
func Bytes(b []byte) []byte {
	if !patterns.Match(b) {
		return b
	}
	return patterns.ReplaceAllLiteral(b, []byte(Placeholder))
}

// Secret masks a stored setting value so at most its last 4 characters are
// visible — the display shape for a secret-shaped setting (per
// settings.IsSecretKey), used by the settings HTTP endpoint
// (internal/httpapi/server.go) and the settings screen that renders it, so
// an operator sees the same mask from either surface. A value of 4
// characters or fewer reveals none of itself. The guarantee this exists
// for: no stored secret ever appears in full outside the process — there is
// no way to reveal one back over HTTP.
func Secret(value string) string {
	if len(value) <= 4 {
		return strings.Repeat("*", len(value))
	}
	return strings.Repeat("*", len(value)-4) + value[len(value)-4:]
}
