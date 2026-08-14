package evals

import (
	"context"
	"errors"
	"testing"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// TestStartFailsLoudlyWhenJudgeCannotBeBuilt pins the orchestrator's failure
// path for an unresolvable judge model: Start returns the error instead of
// running the eval without the judge the spec asked for, and releases the
// run slot so a second eval can start.
func TestStartFailsLoudlyWhenJudgeCannotBeBuilt(t *testing.T) {
	wantErr := errors.New(`evals: judge model: provider: unknown model "no-such-model" (known: deepseek-v4-flash, deepseek-v4-pro, kimi-k3)`)
	o := &Orchestrator{
		NewJudge: func(model string) (*Judge, error) {
			if model != "no-such-model" {
				t.Errorf("NewJudge model = %q, want no-such-model", model)
			}
			return nil, wantErr
		},
	}
	spec := Spec{
		SuiteJSON: &Suite{
			Name:   "t",
			Rubric: "r",
			Tasks: []Task{{
				ID:     "t1",
				Prompt: "p",
				Repos:  []queue.Repo{{URL: "https://example.com/repo"}},
			}},
		},
		Variants:   []string{"base", "search-first"},
		Replicates: 1,
		Judge:      true,
		JudgeModel: "no-such-model",
	}
	if _, err := o.Start(context.Background(), spec); !errors.Is(err, wantErr) {
		t.Fatalf("Start error = %v, want %v", err, wantErr)
	}
	if inFlight := o.InFlight(); inFlight != "" {
		t.Errorf("InFlight = %q after a failed start, want the run slot released", inFlight)
	}
}
