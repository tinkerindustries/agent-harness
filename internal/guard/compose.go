package guard

import (
	"errors"
	"net/http"
	"strings"
)

// PipelineConfig is what the guard's own parts are assembled from. The caller
// supplies the Anthropic key; this package never reads the environment.
type PipelineConfig struct {
	// Bundle is the validated policy: prompts, models, cut-off and rules.
	Bundle *Bundle
	// Stage1Endpoint is the System One host, without the path. Required when
	// the bundle has a stage 1 and ignored otherwise.
	Stage1Endpoint string
	// AnthropicKey authenticates stage 2.
	AnthropicKey string
	// Stage2BaseURL overrides DefaultStage2BaseURL.
	Stage2BaseURL string
	// HTTPClient is shared by both stages; nil takes a default client.
	HTTPClient *http.Client
}

// NewPipeline assembles the rules, script escalation, the stage clients and
// the guard version into a Pipeline. It fails when the bundle is missing, the
// Anthropic key is empty (ErrNoAnthropicKey), or the bundle has a stage 1 and
// the endpoint is empty.
func NewPipeline(cfg PipelineConfig) (*Pipeline, error) {
	if cfg.Bundle == nil {
		return nil, errors.New("guard: no policy bundle")
	}
	b := cfg.Bundle
	p := &Pipeline{
		Rules:        b.Rules(),
		Escalation:   NewScriptDetector(),
		Stage2Model:  b.Stage2Model(),
		Cutoff:       b.Cutoff(),
		NoStage1:     !b.HasStage1(),
		GuardVersion: b.Version(),
	}
	if b.HasStage1() {
		if strings.TrimSpace(cfg.Stage1Endpoint) == "" {
			return nil, errors.New("guard: stage 1 endpoint is empty")
		}
		stage1, err := NewStage1Client(Stage1Config{Endpoint: cfg.Stage1Endpoint, HTTPClient: cfg.HTTPClient, Bundle: b})
		if err != nil {
			return nil, err
		}
		p.Stage1 = stage1
	}
	stage2, err := NewStage2Client(Stage2Config{
		BaseURL: cfg.Stage2BaseURL, APIKey: cfg.AnthropicKey, Bundle: b, HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	p.Stage2 = stage2
	return p, nil
}
