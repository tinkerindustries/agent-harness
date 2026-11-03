package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/providerhttp"
)

const (
	// Stage1KeepAlive keeps the model loaded between commands.
	Stage1KeepAlive = "30m"
	// DefaultStage1Timeout covers the request and its retries. The evals measured
	// about 150 ms per command; the rest is room for a cold model load.
	DefaultStage1Timeout = 3 * time.Second

	stage1Path           = "/v1/systemone"
	stage1MaxBody        = 1 << 20
	probabilityTolerance = 0.01 + 1e-9
)

// Stage1Config configures a Stage1Client. Zero fields take the defaults.
type Stage1Config struct {
	// Endpoint is the System One host, without the /v1/systemone path.
	Endpoint   string
	HTTPClient *http.Client
	// Bundle supplies the model and the questions. It must have a stage 1.
	Bundle *Bundle
	// Timeout bounds one Classify call including every retry.
	Timeout    time.Duration
	MaxRetries int
	RetryBase  time.Duration
	RetryMax   time.Duration
}

// Stage1Client asks the local classifier for p(block).
type Stage1Client struct {
	transport *providerhttp.Transport
	model     string
	timeout   time.Duration
	questions json.RawMessage
	question  string
	fields    []string
}

// NewStage1Client builds a client for cfg.Endpoint.
func NewStage1Client(cfg Stage1Config) (*Stage1Client, error) {
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("guard: stage 1 endpoint is empty")
	}
	if cfg.Bundle == nil || cfg.Bundle.stage1 == nil {
		return nil, errors.New("guard: the policy has no stage 1")
	}
	policy := cfg.Bundle.stage1
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultStage1Timeout
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 1
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 100 * time.Millisecond
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 500 * time.Millisecond
	}
	return &Stage1Client{
		transport: &providerhttp.Transport{
			BaseURL:        strings.TrimRight(cfg.Endpoint, "/"),
			APIKeyProvider: func() (string, error) { return "none", nil },
			HTTPClient:     httpClient,
			MaxRetries:     cfg.MaxRetries,
			RetryBase:      cfg.RetryBase,
			RetryMax:       cfg.RetryMax,
			Retryable:      retryableStatus,
			NoAPIKey:       errors.New("guard: stage 1 has no key"),
			ErrPrefix:      "guard stage 1",
			SetAuth:        func(*http.Request, string) {},
		},
		model:     policy.model,
		timeout:   cfg.Timeout,
		questions: policy.questions,
		question:  policy.question,
		fields:    policy.fields,
	}, nil
}

type stage1Request struct {
	Model     string          `json:"model"`
	KeepAlive string          `json:"keep_alive"`
	State     json.RawMessage `json:"state"`
	Questions json.RawMessage `json:"questions"`
}

// Stage1Result is a valid stage 1 answer.
type Stage1Result struct {
	Choice string
	PBlock float64
	PAllow float64
}

// PBlock sends the state's listed fields and returns the validated answer.
// Any failure, whether a timeout, a transport error, a non-200 status or an
// invalid answer, comes back as an error and the caller escalates.
func (c *Stage1Client) PBlock(ctx context.Context, state State) (Stage1Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	encoded, err := state.encode(c.fields)
	if err != nil {
		return Stage1Result{}, fmt.Errorf("guard stage 1: encode state: %w", err)
	}
	body, err := marshalNoEscape(stage1Request{Model: c.model, KeepAlive: Stage1KeepAlive, State: encoded, Questions: c.questions})
	if err != nil {
		return Stage1Result{}, fmt.Errorf("guard stage 1: encode request: %w", err)
	}
	resp, err := c.transport.Do(ctx, http.MethodPost, stage1Path, body)
	if err != nil {
		return Stage1Result{}, c.transport.WrapError("request", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, stage1MaxBody))
	if err != nil {
		return Stage1Result{}, c.transport.WrapError("read response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Stage1Result{}, fmt.Errorf("guard stage 1: status %d: %s", resp.StatusCode, snippet(data))
	}
	return parseStage1(data, c.question)
}

type stage1Response struct {
	Answers map[string]struct {
		Choice        string              `json:"choice"`
		Probabilities map[string]*float64 `json:"probabilities"`
	} `json:"answers"`
}

func parseStage1(data []byte, question string) (Stage1Result, error) {
	var resp stage1Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return Stage1Result{}, fmt.Errorf("guard stage 1: decode answer: %w", err)
	}
	answer, ok := resp.Answers[question]
	if !ok {
		return Stage1Result{}, errors.New("guard stage 1: answer does not have the question")
	}
	if answer.Choice != "allow" && answer.Choice != "block" {
		return Stage1Result{}, fmt.Errorf("guard stage 1: choice %q is not a label", answer.Choice)
	}
	if len(answer.Probabilities) != 2 || answer.Probabilities["allow"] == nil || answer.Probabilities["block"] == nil {
		return Stage1Result{}, errors.New("guard stage 1: probabilities do not have exactly the allow and block labels")
	}
	allow, block := *answer.Probabilities["allow"], *answer.Probabilities["block"]
	for label, p := range map[string]float64{"allow": allow, "block": block} {
		if math.IsNaN(p) || math.IsInf(p, 0) || p < 0 || p > 1 {
			return Stage1Result{}, fmt.Errorf("guard stage 1: probability for %s is %v, outside [0, 1]", label, p)
		}
	}
	if math.Abs(allow+block-1) > probabilityTolerance {
		return Stage1Result{}, fmt.Errorf("guard stage 1: probabilities sum to %v, not 1", allow+block)
	}
	return Stage1Result{Choice: answer.Choice, PBlock: block, PAllow: allow}, nil
}

func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout, 529:
		return true
	}
	return false
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
