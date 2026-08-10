package deepseek

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client talks to a single DeepSeek base URL with one API key.
type Client struct {
	baseURL     string
	apiKey      string
	httpClient  *http.Client
	idleTimeout time.Duration
	maxRetries  int
	retryBase   time.Duration
	retryMax    time.Duration
}

// ClientOption customises a Client built by NewClient.
type ClientOption func(*Client)

// WithIdleTimeout overrides the default idle watchdog duration used by
// StreamChatCompletion.
func WithIdleTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.idleTimeout = d }
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

// NewClient builds a Client for baseURL using apiKey. The default HTTP
// client sets no overall request timeout: that would cap an entire stream
// rather than one phase of it. Transport-level timeouts bound connection
// setup, and StreamChatCompletion's idle watchdog bounds gaps between
// frames (docs/DESIGN.md §4.3).
func NewClient(baseURL, apiKey string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
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
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
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

// CreateChatCompletion sends req without streaming and waits for the full
// response.
func (c *Client) CreateChatCompletion(ctx context.Context, req ChatCompletionRequest) (*ChatCompletionResponse, error) {
	req.Stream = false
	req.StreamOptions = nil
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("deepseek: encode request: %w", err)
	}

	resp, err := c.do(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, fmt.Errorf("deepseek: chat completion request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}
	defer resp.Body.Close()

	var out ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("deepseek: decode response: %w", err)
	}
	return &out, nil
}

// ListModels calls GET /models.
func (c *Client) ListModels(ctx context.Context) (*ModelsResponse, error) {
	resp, err := c.do(ctx, http.MethodGet, "/models", nil)
	if err != nil {
		return nil, fmt.Errorf("deepseek: list models: %w", err)
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
		return nil, fmt.Errorf("deepseek: get balance: %w", err)
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
