package evals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/mrgeoffrich/deepseek-harness/internal/promptvariant"
	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// A Suite is a set of tasks run under every variant being compared. Tasks are
// data, not code, so adding one is a file edit rather than a build
// (evals/*.json).
type Suite struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	// Rubric is what the judge scores every task in this suite against. It is
	// per-suite rather than per-task because a rubric that varies by task
	// cannot be compared across them.
	Rubric string `json:"rubric"`
	Tasks  []Task `json:"tasks"`
}

// A Task is one unit of work, published once per variant per replicate.
type Task struct {
	ID     string       `json:"id"`
	Prompt string       `json:"prompt"`
	Repos  []queue.Repo `json:"repos"`
	// PermissionMode defaults to readonly: a suite that only measures how the
	// model looks for things has no reason to let it write.
	PermissionMode string `json:"permission_mode,omitempty"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
	MaxSubTurns    int    `json:"max_sub_turns,omitempty"`
	DeadlineMS     int64  `json:"deadline_ms,omitempty"`
}

// LoadSuite reads a suite from a JSON file and checks it names work that can
// actually be published.
func LoadSuite(path string) (*Suite, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("evals: read suite: %w", err)
	}
	return parseSuite(b, path)
}

// parseSuite decodes a suite, rejecting unknown fields so a misspelled key
// fails rather than being dropped in silence.
func parseSuite(b []byte, name string) (*Suite, error) {
	var s Suite
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("evals: parse suite %s: %w", name, err)
	}
	if err := s.validate(); err != nil {
		return nil, fmt.Errorf("evals: suite %s: %w", name, err)
	}
	return &s, nil
}

func (s *Suite) validate() error {
	if s.Name == "" {
		return errors.New("name is required")
	}
	if len(s.Tasks) == 0 {
		return errors.New("at least one task is required")
	}
	seen := map[string]bool{}
	for i, t := range s.Tasks {
		if t.ID == "" {
			return fmt.Errorf("tasks[%d]: id is required", i)
		}
		if seen[t.ID] {
			return fmt.Errorf("tasks[%d]: duplicate id %q", i, t.ID)
		}
		seen[t.ID] = true
		if t.Prompt == "" {
			return fmt.Errorf("task %q: prompt is required", t.ID)
		}
		if len(t.Repos) == 0 {
			return fmt.Errorf("task %q: at least one repo is required", t.ID)
		}
	}
	return nil
}

// PermissionModeOrDefault is the mode a task runs under.
func (t Task) PermissionModeOrDefault() string {
	if t.PermissionMode == "" {
		return "readonly"
	}
	return t.PermissionMode
}

// Request builds the work request for one run of a task under one variant.
//
// An eval is a producer, not a person: parent_is_user false sends the session
// to the read-only watch page rather than the chat page with a steer
// composer, and steering an eval run would corrupt the measurement it exists
// to produce.
func (t Task) Request(requestID, variant string) queue.Request {
	return queue.Request{
		RequestID:       requestID,
		Prompt:          t.Prompt,
		Repos:           t.Repos,
		Model:           t.Model,
		Effort:          t.Effort,
		PermissionMode:  t.PermissionModeOrDefault(),
		MaxSubTurns:     t.MaxSubTurns,
		DeadlineMS:      t.DeadlineMS,
		JobType:         "implementation",
		PromptVariant:   variant,
		ParentIsUser:    false,
		ParentAgentType: "eval",
		ParentAgentID:   requestID,
	}
}

// ValidateVariants checks every name before anything is published, so a typo
// costs nothing rather than half a suite.
func ValidateVariants(names []string) error {
	if len(names) < 2 {
		return errors.New("evals: name at least two variants to compare")
	}
	seen := map[string]bool{}
	for _, n := range names {
		if seen[n] {
			return fmt.Errorf("evals: variant %q named twice", n)
		}
		seen[n] = true
		if err := promptvariant.Validate(n); err != nil {
			return fmt.Errorf("evals: %w", err)
		}
	}
	return nil
}
