package agentmeta

import (
	"strings"
	"testing"
)

func TestNormalizeJobType(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", JobTypeImplementation},
		{JobTypeImplementation, JobTypeImplementation},
		{JobTypeOrchestration, JobTypeOrchestration},
		{"unrecognized", "unrecognized"},
	}
	for _, c := range cases {
		if got := NormalizeJobType(c.in); got != c.want {
			t.Errorf("NormalizeJobType(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidateJobType(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{JobTypeImplementation, true},
		{JobTypeOrchestration, true},
		{"orchestrator", false},
		{"Implementation", false},
		{"implementation ", false},
	}
	for _, c := range cases {
		err := ValidateJobType(c.in)
		if (err == nil) != c.want {
			t.Errorf("ValidateJobType(%q) error = %v, want error = %v", c.in, err, c.want)
		}
	}
}

func TestValidateParentAgentType(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"user", true},
		{"orchestrator", true},
		{"a", true},
		{"0", true},
		{"a-b-c", true},
		{strings.Repeat("a", 32), true},
		{strings.Repeat("a", 33), false},
		{"A", false},
		{"-a", false},
		{"a b", false},
		{"a_b", false},
		{"a-", true},
	}
	for _, c := range cases {
		err := ValidateParentAgentType(c.in)
		if (err == nil) != c.want {
			t.Errorf("ValidateParentAgentType(%q) error = %v, want error = %v", c.in, err, c.want)
		}
	}
}

func TestValidateParentAgentID(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"parent-1", true},
		{strings.Repeat("x", 128), true},
		{strings.Repeat("x", 129), false},
		{"has space", false},
		{"tab\there", false},
		{"newline\nhere", false},
		{"ctl\x01here", false},
		{"nb\u00a0space", false},
	}
	for _, c := range cases {
		err := ValidateParentAgentID(c.in)
		if (err == nil) != c.want {
			t.Errorf("ValidateParentAgentID(%q) error = %v, want error = %v", c.in, err, c.want)
		}
	}
}

func TestValidateParent(t *testing.T) {
	cases := []struct {
		parentIsUser bool
		agentType    string
		agentID      string
		want         bool
	}{
		{true, "", "", true},
		{true, "", "someone", true},
		{true, "claude-code", "x", false},
		{false, "claude-code", "sess-1", true},
		{false, "", "sess-1", false},
		{false, "user", "", false},
		{false, "user", "x", false},
	}
	for _, c := range cases {
		err := ValidateParent(c.parentIsUser, c.agentType, c.agentID)
		if (err == nil) != c.want {
			t.Errorf("ValidateParent(%v, %q, %q) error = %v, want error = %v", c.parentIsUser, c.agentType, c.agentID, err, c.want)
		}
	}
}

// words returns n whitespace-separated words, so the word-cap tests below
// can build a string exactly at a cap or one word over without hand-typing
// fifty words.
func words(n int) string {
	if n <= 0 {
		return ""
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "word"
	}
	return strings.Join(parts, " ")
}

func TestValidateTitle(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"a", true},
		{words(MaxTitleWords), true},
		{words(MaxTitleWords + 1), false},
		{"line one\nline two", false},
		{"line one\rline two", false},
	}
	for _, c := range cases {
		err := ValidateTitle(c.in)
		if (err == nil) != c.want {
			t.Errorf("ValidateTitle(%q) error = %v, want error = %v", c.in, err, c.want)
		}
	}
}

func TestValidateDescription(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"a", true},
		{words(MaxDescriptionWords), true},
		{words(MaxDescriptionWords + 1), false},
		// Newlines are allowed in a description: it is prose, not a label.
		{"line one\nline two", true},
	}
	for _, c := range cases {
		err := ValidateDescription(c.in)
		if (err == nil) != c.want {
			t.Errorf("ValidateDescription(%q) error = %v, want error = %v", c.in, err, c.want)
		}
	}
}

func TestValidatePhase(t *testing.T) {
	cases := []struct {
		phase, total int
		want         bool
	}{
		{0, 0, true},
		{1, 1, true},
		{1, 3, true},
		{3, 3, true},
		{4, 3, false},
		{0, 3, false},
		{2, 0, false},
		{-1, 3, false},
		{1, -3, false},
		{-1, -1, false},
	}
	for _, c := range cases {
		err := ValidatePhase(c.phase, c.total)
		if (err == nil) != c.want {
			t.Errorf("ValidatePhase(%d, %d) error = %v, want error = %v", c.phase, c.total, err, c.want)
		}
	}
}
