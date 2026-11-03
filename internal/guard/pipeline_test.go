package guard

import (
	"context"
	"errors"
	"testing"
)

type fakeRules []string

func (f fakeRules) CheckRules(command, branch string) []string { return f }

type fakeEscalator []string

func (f fakeEscalator) EscalationReasons(command string, scripts []ScriptEntry) []string { return f }

type fakeStage1 struct {
	res   Stage1Result
	err   error
	calls int
}

func (f *fakeStage1) PBlock(ctx context.Context, s State) (Stage1Result, error) {
	f.calls++
	return f.res, f.err
}

type fakeStage2 struct {
	res   Stage2Result
	err   error
	calls int
}

func (f *fakeStage2) Review(ctx context.Context, s State) (Stage2Result, error) {
	f.calls++
	return f.res, f.err
}

func p1(block float64) *fakeStage1 { return &fakeStage1{res: Stage1Result{PBlock: block}} }
func v2(decision string) *fakeStage2 {
	return &fakeStage2{res: Stage2Result{Decision: decision, Reason: "because", Usage: &Usage{InputTokens: 5}}}
}

func TestPipelineRuleHitBlocksWithoutModels(t *testing.T) {
	s1, s2 := p1(0), v2("allow")
	p := &Pipeline{Rules: fakeRules{"no-sudo"}, Escalation: fakeEscalator{"x"}, Stage1: s1, Stage2: s2, GuardVersion: "v1"}
	d := p.Decide(context.Background(), State{ProposedCommand: "sudo ls"})
	if d.Verdict != VerdictBlock || d.DecidedBy != DecidedByRules || len(d.Rules) != 1 || d.Rules[0] != "no-sudo" {
		t.Fatalf("d = %+v", d)
	}
	if s1.calls+s2.calls != 0 || d.Stage2Ran || d.GuardVersion != "v1" {
		t.Fatalf("models called or version lost: %+v", d)
	}
}

func TestPipelineEscalationSkipsStage1(t *testing.T) {
	s1, s2 := p1(0), v2("block")
	p := &Pipeline{Escalation: fakeEscalator{"two scripts"}, Stage1: s1, Stage2: s2, Stage2Model: "m"}
	d := p.Decide(context.Background(), State{})
	if s1.calls != 0 || s2.calls != 1 {
		t.Fatalf("calls s1=%d s2=%d", s1.calls, s2.calls)
	}
	if d.Verdict != VerdictBlock || d.DecidedBy != DecidedByStage2 || d.Escalation[0] != "two scripts" || d.PBlock != nil || d.Stage2Model != "m" {
		t.Fatalf("d = %+v", d)
	}
	if d.Stage2Reason != "because" || d.Stage2Usage == nil || d.Stage2Usage.InputTokens != 5 {
		t.Fatalf("d = %+v", d)
	}
}

func TestPipelineStage1BelowCutoffAllows(t *testing.T) {
	s2 := v2("block")
	p := &Pipeline{Rules: fakeRules(nil), Escalation: fakeEscalator(nil), Stage1: p1(0.05), Stage2: s2}
	d := p.Decide(context.Background(), State{})
	if d.Verdict != VerdictAllow || d.DecidedBy != DecidedByStage1 || s2.calls != 0 || d.Stage2Ran {
		t.Fatalf("d = %+v", d)
	}
	if d.PBlock == nil || *d.PBlock != 0.05 {
		t.Fatalf("pblock = %v", d.PBlock)
	}
}

func TestPipelineCutoffIsInclusive(t *testing.T) {
	for _, tc := range []struct {
		pb      float64
		stage2  bool
		verdict string
	}{{0.170999, false, VerdictAllow}, {0.171, true, VerdictAllow}, {0.9, true, VerdictAllow}} {
		s2 := v2("allow")
		p := &Pipeline{Stage1: p1(tc.pb), Stage2: s2}
		d := p.Decide(context.Background(), State{})
		if (s2.calls == 1) != tc.stage2 || d.Verdict != tc.verdict {
			t.Fatalf("pblock %v: calls %d verdict %s", tc.pb, s2.calls, d.Verdict)
		}
	}
}

func TestPipelineStage2VerdictIsTheDecisionOnceFlagged(t *testing.T) {
	for _, v := range []string{VerdictAllow, VerdictBlock} {
		p := &Pipeline{Stage1: p1(0.5), Stage2: v2(v)}
		d := p.Decide(context.Background(), State{})
		if d.Verdict != v || d.DecidedBy != DecidedByStage2 {
			t.Fatalf("stage 2 said %s: %+v", v, d)
		}
	}
}

func TestPipelineStage1FailureEscalates(t *testing.T) {
	s2 := v2("allow")
	p := &Pipeline{Stage1: &fakeStage1{err: errors.New("timeout")}, Stage2: s2}
	d := p.Decide(context.Background(), State{})
	if s2.calls != 1 || d.Verdict != VerdictAllow || d.DecidedBy != DecidedByStage2 {
		t.Fatalf("d = %+v", d)
	}
	if d.Stage1Error != "timeout" || d.PBlock != nil {
		t.Fatalf("d = %+v", d)
	}
}

func TestPipelineStage2FailureBlocks(t *testing.T) {
	cases := map[string]*fakeStage2{
		"error":            {err: errors.New("deadline exceeded")},
		"error with usage": {res: Stage2Result{Decision: VerdictAllow, Usage: &Usage{OutputTokens: 9}}, err: errors.New("bad verdict")},
		"empty decision":   {res: Stage2Result{}},
		"odd decision":     {res: Stage2Result{Decision: "maybe"}},
	}
	for name, s2 := range cases {
		t.Run(name, func(t *testing.T) {
			p := &Pipeline{Stage1: p1(0.9), Stage2: s2}
			d := p.Decide(context.Background(), State{})
			if d.Verdict != VerdictBlock || d.DecidedBy != DecidedByStage2 || d.Stage2Error == "" || d.Stage2Reason != "" {
				t.Fatalf("d = %+v", d)
			}
		})
	}
}

func TestPipelineBothStagesDownBlocks(t *testing.T) {
	p := &Pipeline{Stage1: &fakeStage1{err: errors.New("down")}, Stage2: &fakeStage2{err: errors.New("down")}}
	d := p.Decide(context.Background(), State{})
	if d.Verdict != VerdictBlock || d.Stage1Error == "" || d.Stage2Error == "" {
		t.Fatalf("d = %+v", d)
	}
}

func TestPipelineMissingStagesNeverAllowAFlaggedCommand(t *testing.T) {
	d := (&Pipeline{}).Decide(context.Background(), State{})
	if d.Verdict != VerdictBlock {
		t.Fatalf("d = %+v", d)
	}
}

func TestPipelineRecordsLatency(t *testing.T) {
	p := &Pipeline{Stage1: p1(0.5), Stage2: v2("allow")}
	d := p.Decide(context.Background(), State{})
	if d.Latency <= 0 || d.Latency < d.Stage1Latency || d.Latency < d.Stage2Latency {
		t.Fatalf("latency = %+v", d)
	}
}
