package session

import "github.com/mrgeoffrich/deepseek-harness/internal/promptvariant"

// reminderState is one run's position in its reminder cadence. The policy —
// what the reminder says and how far the context must grow between them —
// lives in internal/promptvariant; what a particular run has already been
// told lives here, because it is per-run state the loop owns.
type reminderState struct {
	policy promptvariant.Policy
	// lastAt is the context size when the last reminder fired. Zero means
	// none has.
	lastAt int
}

func newReminderState(name string) reminderState {
	return reminderState{policy: promptvariant.PolicyByName(name)}
}

// due reports whether a reminder is owed before the next request, and records
// that it fired.
func (s *reminderState) due(contextTokens int) (text, role string, ok bool) {
	text, role, ok = s.policy.Due(contextTokens, s.lastAt)
	if ok {
		s.lastAt = contextTokens
	}
	return text, role, ok
}
