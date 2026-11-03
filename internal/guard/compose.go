package guard

import (
	"net/http"
)

// PipelineConfig is what the guard's own parts are assembled from. The caller
// supplies the Anthropic key; this package never reads the environment.
type PipelineConfig struct {
	// Stage1Endpoint is the System One host, without the path.
	Stage1Endpoint string
	// Stage2Model is the Anthropic model for stage 2; empty takes
	// DefaultStage2Model.
	Stage2Model string
	// AnthropicKey authenticates stage 2.
	AnthropicKey string
	// Stage2BaseURL overrides DefaultStage2BaseURL.
	Stage2BaseURL string
	// HTTPClient is shared by both stages; nil takes a default client.
	HTTPClient *http.Client
}

// NewPipeline assembles the rules, script escalation, both stage clients and
// the guard version into a Pipeline. It fails when the Anthropic key is empty
// (ErrNoAnthropicKey) or the stage 1 endpoint is.
func NewPipeline(cfg PipelineConfig) (*Pipeline, error) {
	stage1, err := NewStage1Client(Stage1Config{Endpoint: cfg.Stage1Endpoint, HTTPClient: cfg.HTTPClient})
	if err != nil {
		return nil, err
	}
	stage2, err := NewStage2Client(Stage2Config{
		BaseURL: cfg.Stage2BaseURL, APIKey: cfg.AnthropicKey, Model: cfg.Stage2Model, HTTPClient: cfg.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	version, err := Version(VersionInputs{
		Rules:       RulesIdentity(),
		Cutoff:      DefaultCutoff,
		Stage1Model: DefaultStage1Model,
		Stage2Model: stage2.Model(),
	})
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		Rules:        NewRules(),
		Escalation:   NewScriptDetector(),
		Stage1:       stage1,
		Stage2:       stage2,
		Stage2Model:  stage2.Model(),
		Cutoff:       DefaultCutoff,
		GuardVersion: version,
	}, nil
}
