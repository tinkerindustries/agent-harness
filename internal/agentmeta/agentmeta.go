// Package agentmeta holds the session provenance vocabulary: the kind of job
// a session runs and the agent or person that owns it. It imports nothing
// from this repository, so the queue, the store, the session runner, and the
// MCP server validate identical values.
package agentmeta

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Job types for a session.
const (
	JobTypeImplementation = "implementation"
	JobTypeOrchestration  = "orchestration"
)

// ParentAgentUser is a legacy stored value only: it was the reserved parent
// agent type meaning a person started the session rather than another agent.
// New requests reject it — the producer sets parent_is_user instead — but it
// is retained so a pre-existing row still reads as started by a person.
const ParentAgentUser = "user"

// parentAgentTypeRE is the parent agent type grammar: a lowercase letter or
// digit, followed by up to 31 letters, digits, or hyphens.
var parentAgentTypeRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// NormalizeJobType returns JobTypeImplementation for "" and s unchanged
// otherwise, so a zero-value Session stores the default job type.
func NormalizeJobType(s string) string {
	if s == "" {
		return JobTypeImplementation
	}
	return s
}

// ValidateJobType returns an error unless s is "" or one of the job type
// constants. Callers normalize first, so "" passes here.
func ValidateJobType(s string) error {
	if s == "" || s == JobTypeImplementation || s == JobTypeOrchestration {
		return nil
	}
	return fmt.Errorf("agentmeta: unknown job type %q", s)
}

// ValidateParentAgentType returns an error unless s is "" or matches
// ^[a-z0-9][a-z0-9-]{0,31}$.
func ValidateParentAgentType(s string) error {
	if s == "" || parentAgentTypeRE.MatchString(s) {
		return nil
	}
	return fmt.Errorf("agentmeta: invalid parent agent type %q", s)
}

// MaxTitleWords is the word cap on a run's title: the name of the run,
// shown bold on the main page. Ten words is a label, not a paragraph.
const MaxTitleWords = 10

// MaxDescriptionWords is the word cap on a run's description: what change
// the agent is making, shown under the title on the main page.
const MaxDescriptionWords = 50

// ValidateTitle returns an error unless s is "" or is at most MaxTitleWords
// whitespace-separated words and contains no newline. Empty is allowed —
// presence is the producer's call (the browser start form leaves it blank;
// the MCP launch path requires it) — so this enforces only the shape of a
// present title.
func ValidateTitle(s string) error {
	if s == "" {
		return nil
	}
	if strings.ContainsAny(s, "\n\r") {
		return fmt.Errorf("agentmeta: title must not contain a newline")
	}
	if n := len(strings.Fields(s)); n > MaxTitleWords {
		return fmt.Errorf("agentmeta: title has %d words, at most %d", n, MaxTitleWords)
	}
	return nil
}

// ValidateDescription returns an error unless s is "" or is at most
// MaxDescriptionWords whitespace-separated words. Newlines are allowed — a
// description is prose. Empty is allowed for the same reason it is for a
// title: presence is the producer's call.
func ValidateDescription(s string) error {
	if s == "" {
		return nil
	}
	if n := len(strings.Fields(s)); n > MaxDescriptionWords {
		return fmt.Errorf("agentmeta: description has %d words, at most %d", n, MaxDescriptionWords)
	}
	return nil
}

// ValidatePhase returns an error unless phase and total are both zero (the
// run is not part of a multi-phase chain) or both positive with
// phase <= total. Negative is always an error, and one set without the
// other is an error: a chain position has no meaning with only half of it.
func ValidatePhase(phase, total int) error {
	if phase == 0 && total == 0 {
		return nil
	}
	if phase <= 0 || total <= 0 {
		return fmt.Errorf("agentmeta: phase (%d) and total_phases (%d) must both be set, or both zero", phase, total)
	}
	if phase > total {
		return fmt.Errorf("agentmeta: phase %d exceeds total_phases %d", phase, total)
	}
	return nil
}

// ValidateParentAgentID returns an error unless s is "" or is at most 128
// characters with no whitespace and no control characters.
func ValidateParentAgentID(s string) error {
	if s == "" {
		return nil
	}
	if utf8.RuneCountInString(s) > 128 {
		return fmt.Errorf("agentmeta: parent agent id longer than 128 characters")
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("agentmeta: parent agent id contains whitespace or control characters")
		}
	}
	return nil
}

// ValidateParent validates the full provenance triple as a whole: whether a
// person started the run directly, the launching agent's kind, and the
// launching agent's own session id (or the operator's name when parentIsUser
// is true). A user-started run carries no parent agent type, the retired type
// "user" is rejected in new requests, and an id with no type remains an error.
func ValidateParent(parentIsUser bool, agentType, agentID string) error {
	if err := ValidateParentAgentType(agentType); err != nil {
		return err
	}
	if err := ValidateParentAgentID(agentID); err != nil {
		return err
	}
	if parentIsUser && agentType != "" {
		return fmt.Errorf("agentmeta: a user-started run carries no parent agent type")
	}
	if !parentIsUser && agentType == ParentAgentUser {
		return fmt.Errorf("agentmeta: parent_agent_type \"user\" is retired: the producer sets parent_is_user instead")
	}
	if !parentIsUser && agentID != "" && agentType == "" {
		return fmt.Errorf("agentmeta: parent agent id %q has no type", agentID)
	}
	return nil
}
