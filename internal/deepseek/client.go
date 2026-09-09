package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/providerhttp"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// Client talks to a single DeepSeek base URL. The API key is supplied per
// request by a provider, so a key stored in the database can change while
// the client lives without rebuilding it. The HTTP shape — per-request key,
// retries with backoff on transient statuses, the streaming idle watchdog —
// lives in internal/providerhttp, shared with internal/kimi; what stays here
// is DeepSeek's own dialect: the request body, the error body, the retry
// classification, the usage split, and the quirk repairs
// (docs/KIMI-INTEGRATION.md §4.1).
type Client struct {
	transport *providerhttp.Transport
}

// DefaultBaseURL is the host a caller with no configured base URL talks to.
// `harness serve` reads its own from the environment (internal/config, which
// depends on nothing internal and carries the same literal); this is for
// `harness stdio-session`, which loads no configuration at all because the
// parent owns its working directory and no .env of that directory may reach
// this process (docs/STDIO-PROTOCOL.md).
const DefaultBaseURL = "https://api.deepseek.com"

// ErrNoAPIKey is returned before a request is sent when the key provider
// supplies an empty key — the operator's fix is named rather than DeepSeek's
// 401 being what they see.
var ErrNoAPIKey = errors.New("no DeepSeek API key configured; set one from the settings screen or PUT /api/settings/deepseek.api_key")

// ClientOption customises a Client built by NewClient.
type ClientOption func(*Client)

// WithIdleTimeout overrides the default idle watchdog duration used by
// StreamChatCompletion.
func WithIdleTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.transport.IdleTimeout = d }
}

// WithAPIKeyProvider replaces the key supplied at construction time with one
// resolved per request. The provider is called before every request is sent;
// an empty key returned from it fails the request locally with ErrNoAPIKey.
func WithAPIKeyProvider(fn func() (string, error)) ClientOption {
	return func(c *Client) { c.transport.APIKeyProvider = fn }
}

// WithHTTPClient overrides the default HTTP client, e.g. in tests.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) { c.transport.HTTPClient = h }
}

// WithTransportWrapper wraps the client's existing transport, e.g. to
// capture traffic. The default transport's dial, TLS handshake, and
// response header timeouts survive because the wrapper replaces the
// Transport field, not the http.Client.
func WithTransportWrapper(wrap func(http.RoundTripper) http.RoundTripper) ClientOption {
	return func(c *Client) { c.transport.HTTPClient.Transport = wrap(c.transport.HTTPClient.Transport) }
}

// NewClient builds a Client for baseURL using apiKey. The string is wrapped
// in a provider internally, so NewClient keeps its fixed-key behaviour while
// the client itself reads the key per request; WithAPIKeyProvider replaces
// the fixed key with one resolved at request time. The default HTTP client
// sets no overall request timeout: that would cap an entire stream rather
// than one phase of it. Transport-level timeouts bound connection setup, and
// StreamChatCompletion's idle watchdog bounds gaps between frames
// (docs/DESIGN.md §4.3).
func NewClient(baseURL, apiKey string, opts ...ClientOption) *Client {
	c := &Client{
		transport: &providerhttp.Transport{
			BaseURL: strings.TrimRight(baseURL, "/"),
			APIKeyProvider: func() (string, error) {
				return apiKey, nil
			},
			HTTPClient: &http.Client{
				Transport: &http.Transport{
					DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
					TLSHandshakeTimeout:   15 * time.Second,
					ResponseHeaderTimeout: 30 * time.Second,
					IdleConnTimeout:       90 * time.Second,
				},
			},
			IdleTimeout: 120 * time.Second,
			MaxRetries:  4,
			RetryBase:   500 * time.Millisecond,
			RetryMax:    20 * time.Second,
			Retryable:   isRetryableStatus,
			NoAPIKey:    ErrNoAPIKey,
			ErrPrefix:   "deepseek",
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// CreateChatCompletion sends one non-streaming completion expressing the
// caller's intent and waits for the full response. The request is built from
// the intent here, in the provider, so the caller never spells DeepSeek's
// reasoning control (docs/KIMI-INTEGRATION.md §4.1).
func (c *Client) CreateChatCompletion(ctx context.Context, intent wire.ChatIntent) (*wire.ChatCompletionResponse, error) {
	body, err := json.Marshal(requestFromIntent(intent))
	if err != nil {
		return nil, fmt.Errorf("deepseek: encode request: %w", err)
	}

	resp, err := c.transport.Do(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, c.transport.WrapError("chat completion request", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}
	defer resp.Body.Close()

	var out wire.ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("deepseek: decode response: %w", err)
	}
	return &out, nil
}

// UsageSplit maps one request's usage figures onto the cache-hit and
// cache-miss counts the cost model and the stored usage payload use. How a
// provider reports prefix caching is a provider decision: DeepSeek reports
// prompt_cache_hit_tokens and prompt_cache_miss_tokens separately, so the
// split is the response's own figures; Kimi K3 reports a single
// cached_tokens instead and will derive the split itself
// (docs/KIMI-INTEGRATION.md §2). A nil usage — the API did not return one —
// maps to zeroes.
func (c *Client) UsageSplit(usage *wire.Usage) (cacheHit, cacheMiss int) {
	if usage == nil {
		return 0, 0
	}
	return usage.PromptCacheHitTokens, usage.PromptCacheMissTokens
}

// CacheSlack is the churn detector's tolerance for DeepSeek: the trailing
// partial 128-token block, always under one block (internal/cache/churn.go,
// docs/OBSERVED.md).
func (c *Client) CacheSlack() int { return 127 }

// ListModels calls GET /models.
func (c *Client) ListModels(ctx context.Context) (*ModelsResponse, error) {
	resp, err := c.transport.Do(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, c.transport.WrapError("list models", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}
	defer resp.Body.Close()

	var out ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("deepseek: decode models response: %w", err)
	}
	return &out, nil
}

// GetBalance calls GET /user/balance.
func (c *Client) GetBalance(ctx context.Context) (*BalanceResponse, error) {
	resp, err := c.transport.Do(ctx, http.MethodGet, "/user/balance", nil)
	if err != nil {
		return nil, c.transport.WrapError("get balance", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}
	defer resp.Body.Close()

	var out BalanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("deepseek: decode balance response: %w", err)
	}
	return &out, nil
}
