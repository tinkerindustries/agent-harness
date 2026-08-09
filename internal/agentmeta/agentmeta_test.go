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

func TestValidateParentAgent(t *testing.T) {
	cases := []struct {
		agentType string
		agentID   string
		want      bool
	}{
		{"", "", true},
		{"orchestrator", "orchestrator-1", true},
		{"user", "", true},
		{"", "id-with-no-type", false},
		{"user", "someone", false},
		{"user", "user", false},
	}
	for _, c := range cases {
		err := ValidateParentAgent(c.agentType, c.agentID)
		if (err == nil) != c.want {
			t.Errorf("ValidateParentAgent(%q, %q) error = %v, want error = %v", c.agentType, c.agentID, err, c.want)
		}
	}
}
