package evals

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/promptvariant"
	"github.com/mrgeoffrich/agent-harness/internal/store"
)

// The orchestrator runs an eval inside `harness serve` rather than in the
// terminal that asked for it. Two things follow: a run survives the operator
// closing the terminal — or rebuilding the container under it, which is how
// a run got stranded at 16 of 24 — and the browser can start one.
//
// The CLI becomes a client of this rather than a second implementation, the
// way cmd/harness/stop.go already posts to the stop endpoint rather than
// reaching around it.

// ErrEvalNotRunning is returned by Cancel when no run in this process owns
// that id.
var ErrEvalNotRunning = errors.New("evals: no such eval run is in flight here")

// ErrEvalInFlight is returned by Start when one is already going. Two evals
// interleaving means each measures a machine the other is loading, which is
// not a comparison either of them can stand behind.
var ErrEvalInFlight = errors.New("evals: an eval run is already in flight")

// Spec is one eval a caller is asking for, in the shape the HTTP body
// carries.
type Spec struct {
	// Suite is a built-in suite's name. SuiteJSON is a suite posted inline,
	// which is how a caller forwards a suite file the server does not have.
	// Exactly one is set.
	Suite     string `json:"suite,omitempty"`
	SuiteJSON *Suite `json:"suite_json,omitempty"`

	Variants    []string `json:"variants"`
	Replicates  int      `json:"replicates"`
	Concurrency int      `json:"concurrency,omitempty"`
	MaxSubTurns int      `json:"max_sub_turns,omitempty"`
	Model       string   `json:"model,omitempty"`
	Effort      string   `json:"effort,omitempty"`
	Judge       bool     `json:"judge,omitempty"`
	JudgeModel  string   `json:"judge_model,omitempty"`
	Note        string   `json:"note,omitempty"`
}

// Resolve turns a Spec into the suite it names and checks everything that can
// be checked before a single request is published. A caller runs this to
// reject a bad request with a 400 rather than half a suite.
func (s Spec) Resolve() (*Suite, error) {
	if (s.Suite == "") == (s.SuiteJSON == nil) {
		return nil, errors.New("evals: name exactly one of suite and suite_json")
	}
	suite := s.SuiteJSON
	if s.Suite != "" {
		found, err := EmbeddedSuite(s.Suite)
		if err != nil {
			return nil, err
		}
		suite = found
	}
	if err := suite.validate(); err != nil {
		return nil, fmt.Errorf("evals: suite: %w", err)
	}
	if err := ValidateVariants(s.Variants); err != nil {
		return nil, err
	}
	if s.Replicates < 1 {
		return nil, errors.New("evals: replicates must be at least 1")
	}
	if s.MaxSubTurns < 0 {
		return nil, errors.New("evals: max_sub_turns must not be negative")
	}
	return suite, nil
}

// TotalRuns is how many sessions the spec will start, which a caller shows
// before asking someone to confirm spending it.
func (s Spec) TotalRuns(suite *Suite) int {
	return s.Replicates * len(suite.Tasks) * len(s.Variants)
}

// Orchestrator runs evals in this process. One at a time: see ErrEvalInFlight.
type Orchestrator struct {
	Publisher Publisher
	Store     *store.Store
	// NewJudge builds the judge for a run. It is a function rather than a
	// Judge so the model resolves per run from settings, the same read-through
	// shape the rest of the process uses; it returns an error when the judge's
	// model cannot be resolved to a provider client, which fails the start
	// loudly rather than scoring with the wrong provider or skipping the judge
	// silently.
	NewJudge func(model string) (*Judge, error)
	// OnChange, when set, is called with the run id after every write, so a
	// browser watching sees the run move.
	OnChange func(evalRunID string)

	mu      sync.Mutex
	running map[string]context.CancelFunc
}

// Start records the run, publishes its first requests, and returns the run's
// id. It does not wait: the run continues in this process after the caller
// has gone.
func (o *Orchestrator) Start(ctx context.Context, spec Spec) (string, error) {
	suite, err := spec.Resolve()
	if err != nil {
		return "", err
	}

	o.mu.Lock()
	if len(o.running) > 0 {
		o.mu.Unlock()
		return "", ErrEvalInFlight
	}
	evalRunID := newID("evr")
	// A background context, not the request's: the run outlives the HTTP call
	// that asked for it. Cancel is what ends it.
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	if o.running == nil {
		o.running = map[string]context.CancelFunc{}
	}
	o.running[evalRunID] = cancel
	o.mu.Unlock()

	opts := Options{
		Suite:       suite,
		Variants:    spec.Variants,
		Replicates:  spec.Replicates,
		Concurrency: spec.Concurrency,
		MaxSubTurns: spec.MaxSubTurns,
		Model:       spec.Model,
		Effort:      spec.Effort,
		Recorder:    o.Store,
		Note:        spec.Note,
		EvalRunID:   evalRunID,
		StartedAt:   time.Now().UTC(),
		OnChange:    o.OnChange,
	}
	if spec.Judge && o.NewJudge != nil {
		judge, err := o.NewJudge(spec.JudgeModel)
		if err != nil {
			// The judge's model could not be resolved to a provider client —
			// an unknown name or a failed settings read. Fail the start loudly
			// instead of running the eval without the judge the spec asked
			// for; the run slot registered above is released here, the same
			// cleanup the goroutine's defer does on a normal finish.
			cancel()
			o.mu.Lock()
			delete(o.running, evalRunID)
			o.mu.Unlock()
			return "", err
		}
		opts.Judge = judge
	}

	go func() {
		defer func() {
			cancel()
			o.mu.Lock()
			delete(o.running, evalRunID)
			o.mu.Unlock()
		}()
		if _, err := Execute(runCtx, o.Publisher, o.Store, opts); err != nil {
			log.Printf("evals: run %s: %v", evalRunID, err)
			// Execute failed before it could close the run out itself, so the
			// row would otherwise say running forever.
			if ferr := o.Store.FinishEvalRun(context.WithoutCancel(runCtx), evalRunID,
				store.EvalStatusFailed, time.Now().UTC()); ferr != nil && !errors.Is(ferr, store.ErrNotFound) {
				log.Printf("evals: close failed run %s: %v", evalRunID, ferr)
			}
			notify(o.OnChange, evalRunID)
		}
	}()
	return evalRunID, nil
}

// Cancel stops a run in flight. Members already finished keep their scores,
// and the run lands cancelled with a partial comparison — a legitimate result
// over what did finish.
func (o *Orchestrator) Cancel(evalRunID string) error {
	o.mu.Lock()
	cancel, ok := o.running[evalRunID]
	o.mu.Unlock()
	if !ok {
		return ErrEvalNotRunning
	}
	cancel()
	return nil
}

// Running reports whether this process is running that eval.
func (o *Orchestrator) Running(evalRunID string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, ok := o.running[evalRunID]
	return ok
}

// InFlight names the run this process is executing, or "" when idle.
func (o *Orchestrator) InFlight() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	for id := range o.running {
		return id
	}
	return ""
}

// VariantNames is the vocabulary a caller may name, re-exported so the HTTP
// layer does not have to import the variant package for one list.
func VariantNames() []string { return promptvariant.Names() }
