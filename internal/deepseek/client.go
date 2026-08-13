package deepseek

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

// Client talks to a single DeepSeek base URL. The API key is supplied per
// request by a provider, so a key stored in the database can change while
// the client lives without rebuilding it.
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
// supplies an empty key — the operator's fix is named rather than DeepSeek's
// 401 being what they see.
var ErrNoAPIKey = errors.New("no DeepSeek API key configured; set one with: harness config set deepseek.api_key <key>")

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
		return nil, fmt.Errorf("deepseek: resolve api key: %w", err)
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
// that one surfaces verbatim, so the operator sees the fix — "no DeepSeek
// API key configured; set one with: harness config set deepseek.api_key
// <key>" — without a transport prefix in front of it.
func wrapClientError(op string, err error) error {
	if errors.Is(err, ErrNoAPIKey) {
		return err
	}
	return fmt.Errorf("deepseek: %s: %w", op, err)
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
		return nil, fmt.Errorf("deepseek: decode models response: %w", err)
	}
	return &out, nil
}

// GetBalance calls GET /user/balance.
func (c *Client) GetBalance(ctx context.Context) (*BalanceResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/user/balance", nil)
	if err != nil {
		return nil, wrapClientError("get balance", err)
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
