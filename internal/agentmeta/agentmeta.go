// Package agentmeta holds the session provenance vocabulary: the kind of job
// a session runs and the agent or person that owns it. It imports nothing
// from this repository, so the queue, the store, the session runner, and the
// MCP server validate identical values.
package agentmeta

import (
	"fmt"
	"regexp"
	"unicode"
	"unicode/utf8"
)

// Job types for a session.
const (
	JobTypeImplementation = "implementation"
	JobTypeOrchestration  = "orchestration"
)

// ParentAgentUser is the reserved parent agent type meaning a person started
// the session rather than another agent.
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

// ValidateParentAgent validates the pair as a whole. An id with no type is an
// error, and the reserved type "user" must not carry an id.
func ValidateParentAgent(agentType, agentID string) error {
	if err := ValidateParentAgentType(agentType); err != nil {
		return err
	}
	if err := ValidateParentAgentID(agentID); err != nil {
		return err
	}
	if agentID != "" && agentType == "" {
		return fmt.Errorf("agentmeta: parent agent id %q has no type", agentID)
	}
	if agentType == ParentAgentUser && agentID != "" {
		return fmt.Errorf("agentmeta: user sessions carry no parent agent id")
	}
	return nil
}
