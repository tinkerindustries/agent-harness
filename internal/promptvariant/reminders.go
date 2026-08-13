package promptvariant

import (
	"fmt"
	"sort"

	"github.com/mrgeoffrich/deepseek-harness/internal/deepseek"
)

// A rule stated once in the system prompt decays as the context grows.
// Measured over 85 production sessions, the share of searches made with the
// Grep and Glob tools fell from 41.7% under 16k tokens to 3.5% above 128k,
// and edits refused for want of a read rose from 0% to 1.95% (docs/EVALS.md).
// A reminder re-states the rule further down the conversation, where the
// system prompt's hold has weakened.
//
// Cadence is measured in context tokens rather than sub-turns, because that
// is the variable the decay is against. Sub-turns vary enormously in what
// they add — one tool call returning a build log can add more than thirty
// small ones — so a per-sub-turn cadence would remind a cheap run constantly
// and an expensive one hardly at all.
//
// A reminder is appended at the tail, through the same path a steer takes,
// and never rewrites anything earlier. The head of the request is untouched,
// so the cached prefix survives and the reminder costs its own tokens and the
// trailing partial block (docs/CACHE.md).

// NoReminders is the policy every production run uses today: none.
const NoReminders = "none"

// A Policy is a reminder cadence. internal/session owns the running state
// that decides when one fires; this is the data.
type Policy struct {
	description string
	// text is appended verbatim as a user message.
	text string
	// afterTokens is the context size at which reminding starts. Below it the
	// rule still largely holds on its own, and a reminder would be noise.
	afterTokens int
	// everyTokens is how far the context must grow past the last reminder
	// before the next one.
	everyTokens int
	// role is the message role the reminder folds to. Empty is "user", which
	// is what an operator steer is. DeepSeek's schema accepts a system
	// message at any position in the array, and whether one carries further
	// than a user message here is unmeasured — which is why it is a field
	// rather than a decision (docs/EVALS.md).
	role string
}

var reminderPolicies = map[string]Policy{
	NoReminders: {description: "no reminders, which is what production runs do"},

	"search-64k": {
		description: "re-states the search rule once past 64k of context, every 32k after that",
		afterTokens: 64_000,
		everyTokens: 32_000,
		text: "Reminder: search with the Grep and Glob tools rather than through Bash. " +
			"Grep takes a regular expression and an optional glob; Glob takes a path pattern.",
	},

	"search-32k": {
		description: "the same reminder, starting earlier and repeating twice as often",
		afterTokens: 32_000,
		everyTokens: 16_000,
		text: "Reminder: search with the Grep and Glob tools rather than through Bash. " +
			"Grep takes a regular expression and an optional glob; Glob takes a path pattern.",
	},

	"search-64k-system": {
		description: "the search reminder as a system message rather than a user one",
		afterTokens: 64_000,
		everyTokens: 32_000,
		role:        deepseek.RoleSystem,
		text: "Reminder: search with the Grep and Glob tools rather than through Bash. " +
			"Grep takes a regular expression and an optional glob; Glob takes a path pattern.",
	},

	"read-before-edit-64k": {
		description: "re-states the read-before-edit rule once past 64k of context, every 32k after that",
		afterTokens: 64_000,
		everyTokens: 32_000,
		text: "Reminder: Read a file in this session before you Edit or Write over it. " +
			"Having read a different file with the same name does not count.",
	},
}

// ValidateReminderPolicy reports whether a name is one this build knows.
func ValidateReminderPolicy(name string) error {
	if name == "" || name == NoReminders {
		return nil
	}
	if _, ok := reminderPolicies[name]; !ok {
		return fmt.Errorf("unknown reminder policy %q; known policies are %s", name, join(ReminderPolicyNames()))
	}
	return nil
}

// ReminderPolicyNames lists every known policy, "none" first and the rest
// sorted.
func ReminderPolicyNames() []string {
	rest := make([]string, 0, len(reminderPolicies))
	for name := range reminderPolicies {
		if name != NoReminders {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append([]string{NoReminders}, rest...)
}

// ReminderPolicyDescription is the one-line summary of what a policy does.
func ReminderPolicyDescription(name string) string {
	if name == "" {
		name = NoReminders
	}
	return reminderPolicies[name].description
}

// PolicyByName is the named cadence, or a zero Policy that never fires.
func PolicyByName(name string) Policy {
	if name == "" {
		name = NoReminders
	}
	return reminderPolicies[name]
}

// Due reports whether a reminder is owed at this context size, given the size
// at which the last one fired. It is the whole cadence rule: nothing below the
// floor, nothing until the context has grown by the interval, and the first
// reminder at the floor itself. lastAt is zero when none has fired yet.
func (p Policy) Due(contextTokens, lastAt int) (text, role string, ok bool) {
	if p.text == "" || p.everyTokens <= 0 {
		return "", "", false
	}
	if contextTokens < p.afterTokens {
		return "", "", false
	}
	if lastAt > 0 && contextTokens-lastAt < p.everyTokens {
		return "", "", false
	}
	return p.text, p.role, true
}

func join(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += ", "
		}
		out += n
	}
	return out
}
