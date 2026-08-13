package kimi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// DefaultBaseURL is Moonshot AI's OpenAI-compatible endpoint
// (third_party/kimi-docs/api/overview.md: the service address is
// https://api.moonshot.ai and the /v1 prefix is part of every path, so the
// client's paths below are /chat/completions, /models, and
// /users/me/balance against this base).
const DefaultBaseURL = "https://api.moonshot.ai/v1"

// Client talks to a single Kimi base URL. The API key is supplied per
// request by a provider, so a key stored in the database can change while
// the client lives without rebuilding it. The HTTP shape — per-request key,
// retries with backoff on transient statuses, the streaming idle watchdog —
// follows internal/deepseek's client, which is the worked example for all of
// it; what differs is the base URL, the request body (reasoning_effort, never
// thinking), the usage split, and the auxiliary endpoints, all documented
// against third_party/kimi-docs/.
type Client struct {
	baseURL        string
	apiKeyProvider func() (string, error)
	httpClient     *http.Client
	idleTimeout    time.Duration
	maxRetries     int
	retryBase      time.Duration
	retryMax       time.Duration
}

// ErrNoAPIKey is returned before a request is sent when the key provider
// supplies an empty key — the operator's fix is named rather than Kimi's
// 401 being what they see.
var ErrNoAPIKey = errors.New("no Kimi API key configured; set one with: harness config set kimi.api_key <key>")

// ClientOption customises a Client built by NewClient.
type ClientOption func(*Client)

// WithIdleTimeout overrides the default idle watchdog duration used by
// StreamChatCompletion.
func WithIdleTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.idleTimeout = d }
}

// WithAPIKeyProvider replaces the key supplied at construction time with one
// resolved per request. The provider is called before every request is sent;
// an empty key returned from it fails the request locally with ErrNoAPIKey.
func WithAPIKeyProvider(fn func() (string, error)) ClientOption {
	return func(c *Client) { c.apiKeyProvider = fn }
}

// WithHTTPClient overrides the default HTTP client, e.g. in tests.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) { c.httpClient = h }
}

// WithTransportWrapper wraps the client's existing transport, e.g. to
// capture traffic. The default transport's dial, TLS handshake, and
// response header timeouts survive because the wrapper replaces the
// Transport field, not the http.Client.
func WithTransportWrapper(wrap func(http.RoundTripper) http.RoundTripper) ClientOption {
	return func(c *Client) { c.httpClient.Transport = wrap(c.httpClient.Transport) }
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
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKeyProvider: func() (string, error) {
			return apiKey, nil
		},
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
				TLSHandshakeTimeout:   15 * time.Second,
				ResponseHeaderTimeout: 30 * time.Second,
				IdleConnTimeout:       90 * time.Second,
			},
		},
		idleTimeout: 120 * time.Second,
		maxRetries:  4,
		retryBase:   500 * time.Millisecond,
		retryMax:    20 * time.Second,
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func (c *Client) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	apiKey, err := c.apiKeyProvider()
	if err != nil {
		return nil, fmt.Errorf("kimi: resolve api key: %w", err)
	}
	if apiKey == "" {
		return nil, ErrNoAPIKey
	}
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// do sends one request, retrying on transient status codes and network
// errors with backoff. It returns the response as-is on a non-retryable
// status so callers can decode the error body; the caller owns closing
// resp.Body in every non-error return.
func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := c.newRequest(ctx, method, path, body)
		if err != nil {
			return nil, err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt >= c.maxRetries || ctx.Err() != nil {
				return nil, err
			}
			if !sleepBackoff(ctx, c.retryBase, c.retryMax, attempt) {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode == http.StatusOK || !isRetryableStatus(resp.StatusCode) || attempt >= c.maxRetries {
			return resp, nil
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if !sleepBackoff(ctx, c.retryBase, c.retryMax, attempt) {
			return nil, ctx.Err()
		}
	}
}

func sleepBackoff(ctx context.Context, base, max time.Duration, attempt int) bool {
	t := time.NewTimer(backoffDelay(attempt, base, max))
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// wrapClientError adds operation context to err, except for ErrNoAPIKey:
// that one surfaces verbatim, so the operator sees the fix — "no Kimi API
// key configured; set one with: harness config set kimi.api_key <key>" —
// without a transport prefix in front of it.
func wrapClientError(op string, err error) error {
	if errors.Is(err, ErrNoAPIKey) {
		return err
	}
	return fmt.Errorf("kimi: %s: %w", op, err)
}

// CreateChatCompletion sends one non-streaming completion expressing the
// caller's intent and waits for the full response. The request is built from
// the intent here, in the provider, so the caller never spells Kimi's
// reasoning control (docs/KIMI-INTEGRATION.md §4.1).
func (c *Client) CreateChatCompletion(ctx context.Context, intent wire.ChatIntent) (*wire.ChatCompletionResponse, error) {
	body, err := json.Marshal(requestFromIntent(intent))
	if err != nil {
		return nil, fmt.Errorf("kimi: encode request: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, wrapClientError("chat completion request", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}
	defer resp.Body.Close()

	var out wire.ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("kimi: decode response: %w", err)
	}
	return &out, nil
}

// UsageSplit maps one request's usage figures onto the cache-hit and
// cache-miss counts the cost model and the stored usage payload use. How a
// provider reports prefix caching is a provider decision: DeepSeek reports
// prompt_cache_hit_tokens and prompt_cache_miss_tokens separately, while
// Kimi K3 reports a single cached_tokens
// (third_party/kimi-docs/api/chat.md, openapi.json Usage). The hit is
// cached_tokens itself and the miss is everything else in the prompt —
// prompt_tokens minus cached_tokens — so the existing cost accounting keeps
// working (docs/KIMI-INTEGRATION.md §2). A nil usage — the API did not
// return one — maps to zeroes, as does an absent cached_tokens, which then
// bills the whole prompt as a miss.
func (c *Client) UsageSplit(usage *wire.Usage) (cacheHit, cacheMiss int) {
	if usage == nil {
		return 0, 0
	}
	return usage.CachedTokens, usage.PromptTokens - usage.CachedTokens
}

// ListModels calls GET /models.
func (c *Client) ListModels(ctx context.Context) (*ModelsResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, wrapClientError("list models", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}
	defer resp.Body.Close()

	var out ModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("kimi: decode models response: %w", err)
	}
	return &out, nil
}

// GetBalance calls GET /users/me/balance.
func (c *Client) GetBalance(ctx context.Context) (*BalanceResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/users/me/balance", nil)
	if err != nil {
		return nil, wrapClientError("get balance", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}
	defer resp.Body.Close()

	var out BalanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("kimi: decode balance response: %w", err)
	}
	return &out, nil
}
