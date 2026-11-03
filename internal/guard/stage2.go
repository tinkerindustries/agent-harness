package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/providerhttp"
)

const (
	// DefaultStage2BaseURL is the Anthropic API host.
	DefaultStage2BaseURL = "https://api.anthropic.com"
	// DefaultStage2Timeout covers the request and its retries. The evals measured
	// a 1.3 s median at medium effort, with a longer tail when the model thinks.
	DefaultStage2Timeout = 20 * time.Second

	anthropicVersion = "2023-06-01"
	stage2Path       = "/v1/messages"
	stage2MaxBody    = 4 << 20
)

// ErrNoAnthropicKey is returned by NewStage2Client when no key is supplied.
var ErrNoAnthropicKey = errors.New("guard: stage 2 needs an Anthropic API key; set ANTHROPIC_API_KEY")

// Stage2Config configures a Stage2Client. Zero fields take the defaults.
type Stage2Config struct {
	// BaseURL is the Anthropic host, without a path.
	BaseURL string
	// APIKey is supplied by the caller. The package never reads the environment.
	APIKey     string
	HTTPClient *http.Client
	// Bundle supplies the model, effort, token limit, prompt, schema and
	// state fields.
	Bundle *Bundle
	// Timeout bounds one Review call including every retry.
	Timeout    time.Duration
	MaxRetries int
	RetryBase  time.Duration
	RetryMax   time.Duration
}

// Stage2Client asks the reviewer model for an allow or block verdict.
type Stage2Client struct {
	transport *providerhttp.Transport
	model     string
	timeout   time.Duration
	system    string
	schema    json.RawMessage
	effort    string
	maxTokens int
	fields    []string
}

// NewStage2Client builds a client. It fails when APIKey is empty.
func NewStage2Client(cfg Stage2Config) (*Stage2Client, error) {
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, ErrNoAnthropicKey
	}
	if cfg.Bundle == nil {
		return nil, errors.New("guard: stage 2 needs a policy")
	}
	policy := cfg.Bundle.stage2
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultStage2BaseURL
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultStage2Timeout
	}
	if cfg.MaxRetries < 0 {
		cfg.MaxRetries = 0
	} else if cfg.MaxRetries == 0 {
		cfg.MaxRetries = 2
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 500 * time.Millisecond
	}
	if cfg.RetryMax <= 0 {
		cfg.RetryMax = 4 * time.Second
	}
	key := cfg.APIKey
	return &Stage2Client{
		transport: &providerhttp.Transport{
			BaseURL:        strings.TrimRight(cfg.BaseURL, "/"),
			APIKeyProvider: func() (string, error) { return key, nil },
			HTTPClient:     httpClient,
			MaxRetries:     cfg.MaxRetries,
			RetryBase:      cfg.RetryBase,
			RetryMax:       cfg.RetryMax,
			Retryable:      retryableStatus,
			NoAPIKey:       ErrNoAnthropicKey,
			ErrPrefix:      "guard stage 2",
			SetAuth: func(req *http.Request, apiKey string) {
				req.Header.Set("x-api-key", apiKey)
				req.Header.Set("anthropic-version", anthropicVersion)
			},
		},
		model:     policy.model,
		timeout:   cfg.Timeout,
		system:    policy.system,
		schema:    policy.schema,
		effort:    policy.effort,
		maxTokens: policy.maxTokens,
		fields:    policy.fields,
	}, nil
}

// Model is the model id this client sends.
func (c *Stage2Client) Model() string { return c.model }

// Usage is the token count of one stage 2 call.
type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// Stage2Result is a stage 2 call's outcome. Usage is filled whenever the API
// answered, including when the answer was not a valid verdict.
type Stage2Result struct {
	Decision string
	Reason   string
	Usage    *Usage
}

type stage2Request struct {
	Model        string           `json:"model"`
	MaxTokens    int              `json:"max_tokens"`
	System       []stage2System   `json:"system"`
	Messages     []stage2Message  `json:"messages"`
	OutputConfig stage2OutputConf `json:"output_config"`
}

type stage2System struct {
	Type         string       `json:"type"`
	Text         string       `json:"text"`
	CacheControl cacheControl `json:"cache_control"`
}

type cacheControl struct {
	Type string `json:"type"`
}

type stage2Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type stage2OutputConf struct {
	Effort string       `json:"effort"`
	Format stage2Format `json:"format"`
}

type stage2Format struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema"`
}

func (c *Stage2Client) request(state State) ([]byte, error) {
	text, err := state.indented(c.fields)
	if err != nil {
		return nil, err
	}
	return marshalNoEscape(stage2Request{
		Model:     c.model,
		MaxTokens: c.maxTokens,
		System:    []stage2System{{Type: "text", Text: c.system, CacheControl: cacheControl{Type: "ephemeral"}}},
		Messages:  []stage2Message{{Role: "user", Content: text}},
		OutputConfig: stage2OutputConf{
			Effort: c.effort,
			Format: stage2Format{Type: "json_schema", Schema: c.schema},
		},
	})
}

// Review sends the state and returns the verdict. Anything but a complete,
// well-formed verdict is an error, and the caller blocks.
func (c *Stage2Client) Review(ctx context.Context, state State) (Stage2Result, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	body, err := c.request(state)
	if err != nil {
		return Stage2Result{}, fmt.Errorf("guard stage 2: encode request: %w", err)
	}
	resp, err := c.transport.Do(ctx, http.MethodPost, stage2Path, body)
	if err != nil {
		return Stage2Result{}, c.transport.WrapError("request", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, stage2MaxBody))
	if err != nil {
		return Stage2Result{}, c.transport.WrapError("read response", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Stage2Result{}, fmt.Errorf("guard stage 2: status %d: %s", resp.StatusCode, snippet(data))
	}
	return parseStage2(data)
}

type stage2Response struct {
	StopReason string `json:"stop_reason"`
	Content    []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage *Usage `json:"usage"`
}

func parseStage2(data []byte) (Stage2Result, error) {
	var resp stage2Response
	if err := json.Unmarshal(data, &resp); err != nil {
		return Stage2Result{}, fmt.Errorf("guard stage 2: decode response: %w", err)
	}
	result := Stage2Result{Usage: resp.Usage}
	if resp.StopReason != "end_turn" {
		return result, fmt.Errorf("guard stage 2: stop_reason %q, not end_turn", resp.StopReason)
	}
	var text strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	decision, reason, err := parseVerdict(text.String())
	if err != nil {
		return result, err
	}
	result.Decision, result.Reason = decision, reason
	return result, nil
}

// parseVerdict accepts a JSON object with exactly the keys reason (a string)
// and decision (allow or block).
func parseVerdict(text string) (decision, reason string, err error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil {
		return "", "", fmt.Errorf("guard stage 2: verdict is not a JSON object: %w", err)
	}
	if _, extra := dec.Token(); extra != io.EOF {
		return "", "", errors.New("guard stage 2: verdict has text after the JSON object")
	}
	if len(fields) != 2 || fields["reason"] == nil || fields["decision"] == nil {
		return "", "", errors.New("guard stage 2: verdict does not have exactly the keys reason and decision")
	}
	if bytes.Equal(fields["reason"], []byte("null")) || bytes.Equal(fields["decision"], []byte("null")) {
		return "", "", errors.New("guard stage 2: verdict has a null value")
	}
	if err := json.Unmarshal(fields["reason"], &reason); err != nil {
		return "", "", fmt.Errorf("guard stage 2: reason is not a string: %w", err)
	}
	if err := json.Unmarshal(fields["decision"], &decision); err != nil {
		return "", "", fmt.Errorf("guard stage 2: decision is not a string: %w", err)
	}
	if decision != "allow" && decision != "block" {
		return "", "", fmt.Errorf("guard stage 2: decision %q is not allow or block", decision)
	}
	return decision, reason, nil
}
