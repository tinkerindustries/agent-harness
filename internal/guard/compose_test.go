package guard

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrgeoffrich/agent-harness/internal/guard/guardtest"
)

func newTestPipeline(t *testing.T, fake *guardtest.Fake, bundle *Bundle) *Pipeline {
	t.Helper()
	p, err := NewPipeline(PipelineConfig{Bundle: bundle, Stage1Endpoint: fake.Stage1URL, AnthropicKey: "k", Stage2BaseURL: fake.Stage2URL})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewPipelineTakesEverythingFromTheBundle(t *testing.T) {
	fake := guardtest.New(t)
	bundle := mustBundle(t)
	p := newTestPipeline(t, fake, bundle)
	if p.GuardVersion != bundle.Version() || p.Cutoff != guardtest.Cutoff || p.Stage2Model != guardtest.Stage2Model || p.NoStage1 {
		t.Fatalf("pipeline = %+v", p)
	}

	fake.Set(func(b *guardtest.Behaviour) { b.PBlock = 0.1 })
	d := p.Decide(context.Background(), State{ProposedCommand: "ls"})
	if d.Verdict != VerdictAllow || d.DecidedBy != DecidedByStage1 || d.GuardVersion != bundle.Version() {
		t.Fatalf("below the cut-off: %+v", d)
	}
	fake.Set(func(b *guardtest.Behaviour) { b.PBlock = guardtest.Cutoff })
	d = p.Decide(context.Background(), State{ProposedCommand: "ls"})
	if !d.Stage2Ran || d.Stage2Model != guardtest.Stage2Model {
		t.Fatalf("at the cut-off: %+v", d)
	}
	d = p.Decide(context.Background(), State{ProposedCommand: "deploy prod"})
	if d.Verdict != VerdictBlock || d.DecidedBy != DecidedByRules || len(d.Rules) != 1 || d.Rules[0] != "deploy_prod" {
		t.Fatalf("rule hit: %+v", d)
	}
}

func TestNewPipelineWithoutStage1SendsACleanCommandToStage2(t *testing.T) {
	fake := guardtest.New(t)
	bundle := mustBundle(t, func(b map[string]any) {
		delete(b, "stage1")
		delete(b, "cutoff")
	})
	p, err := NewPipeline(PipelineConfig{Bundle: bundle, AnthropicKey: "k", Stage2BaseURL: fake.Stage2URL})
	if err != nil {
		t.Fatal(err)
	}
	d := p.Decide(context.Background(), State{ProposedCommand: "ls"})
	if d.Verdict != VerdictAllow || d.DecidedBy != DecidedByStage2 || !d.Stage2Ran {
		t.Fatalf("d = %+v", d)
	}
	if len(d.Escalation) != 1 || d.Escalation[0] != EscalationNoStage1 || d.Stage1Error != "" {
		t.Fatalf("d = %+v", d)
	}
	if len(fake.Stage1Requests()) != 0 || len(fake.Stage2Requests()) != 1 {
		t.Fatalf("requests: stage 1 %d, stage 2 %d", len(fake.Stage1Requests()), len(fake.Stage2Requests()))
	}
	if d := p.Decide(context.Background(), State{ProposedCommand: "elevate x"}); d.DecidedBy != DecidedByRules {
		t.Fatalf("rules did not run: %+v", d)
	}
	fake.Set(func(b *guardtest.Behaviour) { b.Stage2Status = 500 })
	if d := p.Decide(context.Background(), State{ProposedCommand: "ls"}); d.Verdict != VerdictBlock || d.Stage2Error == "" {
		t.Fatalf("stage 2 failure did not block: %+v", d)
	}
}

func TestNewPipelineStage1ReceivesTheBundlesQuestionsAndFields(t *testing.T) {
	fake := guardtest.New(t)
	p := newTestPipeline(t, fake, mustBundle(t))
	p.Decide(context.Background(), State{ProposedCommand: "ls"})
	reqs := fake.Stage1Requests()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d", len(reqs))
	}
	var req struct {
		Model     string          `json:"model"`
		State     json.RawMessage `json:"state"`
		Questions json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal([]byte(reqs[0]), &req); err != nil {
		t.Fatal(err)
	}
	if req.Model != guardtest.Stage1Model || string(req.Questions) != string(mustBundle(t).stage1.questions) {
		t.Fatalf("request = %s", reqs[0])
	}
	if !strings.HasPrefix(string(req.State), `{"working_directory":`) || strings.Contains(string(req.State), "package_scripts") {
		t.Fatalf("state = %s", req.State)
	}
}

func TestNewPipelineRefusals(t *testing.T) {
	bundle := mustBundle(t)
	if _, err := NewPipeline(PipelineConfig{AnthropicKey: "k"}); err == nil {
		t.Error("no bundle accepted")
	}
	if _, err := NewPipeline(PipelineConfig{Bundle: bundle, AnthropicKey: "k"}); err == nil {
		t.Error("stage 1 without an endpoint accepted")
	}
	if _, err := NewPipeline(PipelineConfig{Bundle: bundle, Stage1Endpoint: "http://x"}); err != ErrNoAnthropicKey {
		t.Errorf("no key: %v", err)
	}
}
