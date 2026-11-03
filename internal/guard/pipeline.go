package guard

import (
	"context"
	"time"
)

// DefaultCutoff is the stage 1 threshold. A command with p(block) at or above
// it goes to stage 2. classifier-research chose it; it changes there first.
const DefaultCutoff = 0.171

// Verdicts and deciding stages.
const (
	VerdictAllow = "allow"
	VerdictBlock = "block"

	DecidedByRules  = "rules"
	DecidedByStage1 = "stage1"
	DecidedByStage2 = "stage2"
)

// RuleChecker names the rules a command hits. An empty result means none.
type RuleChecker interface {
	CheckRules(command, branch string) []string
}

// Escalator names why a command skips stage 1 for stage 2. An empty result
// means it does not escalate.
type Escalator interface {
	EscalationReasons(command string, scripts []ScriptEntry) []string
}

// Classifier is stage 1.
type Classifier interface {
	PBlock(ctx context.Context, state State) (Stage1Result, error)
}

// Reviewer is stage 2.
type Reviewer interface {
	Review(ctx context.Context, state State) (Stage2Result, error)
}

// Decision is the pipeline's verdict and the record of how it got there. It
// holds nothing storage-specific.
type Decision struct {
	Verdict   string
	DecidedBy string
	// Rules are the names of the rules that hit.
	Rules []string
	// Escalation holds the reasons the command skipped stage 1.
	Escalation []string
	// PBlock is stage 1's p(block); nil when stage 1 did not run or failed.
	PBlock *float64
	// Stage1Error is set when stage 1 ran and failed.
	Stage1Error string
	// Stage2Ran is true when stage 2 was called.
	Stage2Ran bool
	// Stage2Model is the model stage 2 used, when it ran.
	Stage2Model  string
	Stage2Reason string
	// Stage2Error is set when stage 2 ran and did not return a verdict.
	Stage2Error string
	Stage2Usage *Usage

	Stage1Latency time.Duration
	Stage2Latency time.Duration
	Latency       time.Duration
	GuardVersion  string
}

// Pipeline combines the rules, escalation, stage 1 and stage 2.
type Pipeline struct {
	Rules      RuleChecker
	Escalation Escalator
	Stage1     Classifier
	Stage2     Reviewer
	// Stage2Model is recorded on a decision that reached stage 2.
	Stage2Model string
	// Cutoff is the stage 1 threshold; zero takes DefaultCutoff.
	Cutoff       float64
	GuardVersion string
}

// Decide runs the order: rules block outright; escalation goes straight to
// stage 2; otherwise stage 1 decides whether stage 2 is needed, and a stage 1
// failure of any kind needs it. A stage 2 failure of any kind blocks. A command
// that reaches none of those is allowed.
func (p *Pipeline) Decide(ctx context.Context, state State) Decision {
	start := time.Now()
	d := Decision{GuardVersion: p.GuardVersion}
	p.decide(ctx, state, &d)
	d.Latency = time.Since(start)
	return d
}

func (p *Pipeline) decide(ctx context.Context, state State, d *Decision) {
	if p.Rules != nil {
		if rules := p.Rules.CheckRules(state.ProposedCommand, state.Branch); len(rules) > 0 {
			d.Verdict, d.DecidedBy, d.Rules = VerdictBlock, DecidedByRules, rules
			return
		}
	}
	if p.Escalation != nil {
		d.Escalation = p.Escalation.EscalationReasons(state.ProposedCommand, state.ScriptsRun)
	}
	if len(d.Escalation) == 0 && !p.runStage1(ctx, state, d) {
		d.Verdict, d.DecidedBy = VerdictAllow, DecidedByStage1
		return
	}
	p.runStage2(ctx, state, d)
}

// runStage1 reports whether the command goes on to stage 2: its p(block) is at
// or above the cut-off, or stage 1 failed.
func (p *Pipeline) runStage1(ctx context.Context, state State, d *Decision) bool {
	began := time.Now()
	var res Stage1Result
	var err error
	if p.Stage1 == nil {
		err = errNotConfigured("stage 1")
	} else {
		res, err = p.Stage1.PBlock(ctx, state)
	}
	d.Stage1Latency = time.Since(began)
	if err != nil {
		d.Stage1Error = err.Error()
		return true
	}
	pb := res.PBlock
	d.PBlock = &pb
	cutoff := p.Cutoff
	if cutoff == 0 {
		cutoff = DefaultCutoff
	}
	return pb >= cutoff
}

func (p *Pipeline) runStage2(ctx context.Context, state State, d *Decision) {
	d.Stage2Ran = true
	d.Stage2Model = p.Stage2Model
	began := time.Now()
	var res Stage2Result
	var err error
	if p.Stage2 == nil {
		err = errNotConfigured("stage 2")
	} else {
		res, err = p.Stage2.Review(ctx, state)
	}
	d.Stage2Latency = time.Since(began)
	d.DecidedBy = DecidedByStage2
	d.Stage2Usage = res.Usage
	if err != nil || (res.Decision != VerdictAllow && res.Decision != VerdictBlock) {
		d.Verdict = VerdictBlock
		if err != nil {
			d.Stage2Error = err.Error()
		} else {
			d.Stage2Error = "stage 2 returned no verdict"
		}
		return
	}
	d.Verdict, d.Stage2Reason = res.Decision, res.Reason
}

type notConfigured string

func (n notConfigured) Error() string { return "guard: " + string(n) + " is not configured" }

func errNotConfigured(stage string) error { return notConfigured(stage) }
