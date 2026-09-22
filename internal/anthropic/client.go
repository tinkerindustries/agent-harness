// Package anthropic is a client for Anthropic's Messages API
// (POST https://api.anthropic.com/v1/messages), hand-rolled rather than
// anthropic-sdk-go for the same reason internal/gemini and internal/deepseek
// are hand-rolled: request and response bodies are Go structs, never
// map[string]any, so identical values always serialise to identical bytes —
// the byte-stability contract the prompt cache depends on
// (docs/DESIGN.md §3.2) — and internal/httplog needs the transport, which an
// SDK's own client does not accept an injected one for. It implements
// internal/session.Client, the same narrow seam internal/deepseek,
// internal/kimi and internal/gemini implement, so the agent loop never
// learns Anthropic's request shape. docs/ANTHROPIC-INTEGRATION.md is the
// provider reference this package is built from; prefer Anthropic's live
// docs over it wherever they disagree, and correct it when they do.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/providerhttp"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// DefaultBaseURL is the host the client talks to when NewClient is given an
// empty string.
const DefaultBaseURL = "https://api.anthropic.com"

// anthropicVersion is the required `anthropic-version` header value.
const anthropicVersion = "2023-06-01"

// thinkingBindingBeta is the beta header this client always sends, gating
// thinking.block_binding (docs/ANTHROPIC-INTEGRATION.md, "Thinking and
// effort").
const thinkingBindingBeta = "thinking-binding-controls-2026-08-01"

// maxPauseTurnResumes bounds the internal resume loop StreamChatCompletion
// and CreateChatCompletion run when a response ends with stop_reason
// "pause_turn" — Claude wanting another round of server-tool use within
// what the loop sees as a single request (docs/ANTHROPIC-INTEGRATION.md,
// "pause_turn"). Bounded so a server tool stuck bouncing pause_turn cannot
// wedge a sub-turn forever; five rounds is generous headroom over what a
// single web_search-then-answer turn needs.
const maxPauseTurnResumes = 5

// ErrNoAPIKey is returned before a request is sent when the key provider
// supplies an empty key.
var ErrNoAPIKey = fmt.Errorf("no Anthropic API key configured; set ANTHROPIC_API_KEY in the environment this process was spawned with")

// Client talks to a single Anthropic base URL. The API key is supplied per
// request by a provider, so a key stored in the database can change while
// the client lives without rebuilding it.
type Client struct {
	transport           *providerhttp.Transport
	httpClient          *http.Client
	idleTimeoutOverride time.Duration
	cacheSlackOverride  int
}

// ClientOption customises a Client built by NewClient.
type ClientOption func(*Client)

// WithAPIKeyProvider replaces the key supplied at construction time with one
// resolved per request.
func WithAPIKeyProvider(fn func() (string, error)) ClientOption {
	return func(c *Client) { c.transport.APIKeyProvider = fn }
}

// WithHTTPClient overrides the default HTTP client, e.g. in tests.
func WithHTTPClient(h *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = h
		c.transport.HTTPClient = h
	}
}

// WithIdleTimeout overrides the stream's idle watchdog window, e.g. in
// tests.
func WithIdleTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.idleTimeoutOverride = d }
}

// WithTransportWrapper wraps the client's existing transport, e.g. to
// capture traffic through internal/httplog.
func WithTransportWrapper(wrap func(http.RoundTripper) http.RoundTripper) ClientOption {
	return func(c *Client) { c.httpClient.Transport = wrap(c.httpClient.Transport) }
}

// NewClient builds a Client for baseURL. With no WithAPIKeyProvider option,
// every request fails locally with ErrNoAPIKey.
func NewClient(baseURL string, opts ...ClientOption) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
			TLSHandshakeTimeout: 15 * time.Second,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	c := &Client{
		httpClient: httpClient,
		transport: &providerhttp.Transport{
			BaseURL: baseURL,
			APIKeyProvider: func() (string, error) {
				return "", nil
			},
			HTTPClient: httpClient,
			MaxRetries: 4,
			RetryBase:  500 * time.Millisecond,
			RetryMax:   20 * time.Second,
			Retryable:  isRetryableStatus,
			NoAPIKey:   ErrNoAPIKey,
			ErrPrefix:  "anthropic",
			// x-api-key, not Authorization: Bearer — the credential header
			// this surface wants, the same reason internal/gemini's own
			// SetAuth exists. anthropic-version and anthropic-beta ride
			// along here too: every request sends both, unconditionally,
			// and Transport offers no header hook narrower than this one
			// (its own Accept field covers one more header, not two), so
			// this is the seam the plan names ("hand-rolled on
			// internal/providerhttp.Transport with SetAuth") for all three.
			SetAuth: func(req *http.Request, apiKey string) {
				req.Header.Set("x-api-key", apiKey)
				req.Header.Set("anthropic-version", anthropicVersion)
				req.Header.Set("anthropic-beta", thinkingBindingBeta)
			},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// StreamChatCompletion sends one streaming request expressing the agent
// loop's intent and returns a channel of typed deltas — the
// session.Client half of this package (docs/ANTHROPIC-INTEGRATION.md).
// The channel closes when the turn ends, normally or by error. A response
// carrying stop_reason "pause_turn" is resumed internally, bounded by
// maxPauseTurnResumes, so the loop always sees a single logical response:
// one EventProviderBlocks carrying every block from every resumed request
// concatenated in order, one EventUsage summing their usage, then
// EventFinish or EventError.
//
// Each round's own connection gets the same mid-stream reconnection every
// other provider's client does: a connection that dies after a 200 but
// before producing any output is reopened with the same request body and
// re-pumped, on providerhttp.Transport.RetryStream's own backoff schedule
// (docs/DESIGN.md §4.3, "Retrying a stream that dies mid-flight"), rather
// than ending the sub-turn (streamOneRound's own doc comment).
func (c *Client) StreamChatCompletion(ctx context.Context, intent wire.ChatIntent) (<-chan wire.Event, error) {
	req := requestFromIntent(intent)
	req.Stream = true

	out := make(chan wire.Event)
	go func() {
		defer close(out)
		send := func(e wire.Event) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}

		var allBlocks []json.RawMessage
		var totalUsage wire.Usage
		haveUsage := false

		for attempt := 0; attempt < maxPauseTurnResumes; attempt++ {
			body, err := json.Marshal(req)
			if err != nil {
				send(wire.Event{Type: wire.EventError, Err: fmt.Errorf("anthropic: encode request: %w", err)})
				return
			}

			result, err := c.streamOneRound(ctx, body, send)
			if err != nil {
				// streamOneRound already forwarded the terminal EventError.
				return
			}
			allBlocks = append(allBlocks, result.blocks...)
			if result.usage != nil {
				totalUsage = sumUsage(totalUsage, *result.usage)
				haveUsage = true
			}

			if result.stopReason == "refusal" {
				send(wire.Event{Type: wire.EventError, Err: &RefusalError{StopDetails: result.stopDetails}})
				return
			}
			if result.stopReason != "pause_turn" {
				raw, _ := json.Marshal(allBlocks)
				send(wire.Event{Type: wire.EventProviderBlocks, ProviderBlocks: raw})
				if haveUsage {
					u := totalUsage
					send(wire.Event{Type: wire.EventUsage, Usage: &u})
				}
				send(wire.Event{Type: wire.EventFinish, FinishReason: finishReasonFor(result.stopReason)})
				return
			}

			// pause_turn: resend the same history with this response's
			// content appended as the assistant's continuing turn, no new
			// user turn (docs/ANTHROPIC-INTEGRATION.md, "pause_turn").
			raw, _ := json.Marshal(result.blocks)
			req.Messages = append(req.Messages, Message{Role: wire.RoleAssistant, Content: raw})
		}
		send(wire.Event{Type: wire.EventError, Err: fmt.Errorf("anthropic: exceeded %d pause_turn resumes in one sub-turn", maxPauseTurnResumes)})
	}()
	return out, nil
}

// streamOneRound issues one Messages API request for reqBody and pumps its
// SSE stream, forwarding every delta to send as it arrives, exactly as the
// inline call this replaced did. The difference is what happens when the
// connection dies after a 200 but before the stream reaches message_stop: to
// providerhttp.Transport.RetryStream, "one HTTP response's whole SSE body"
// is the retry unit, so it is applied here, per round, rather than around
// the pause_turn loop that calls this — a round that has already streamed
// output before dying still ends the sub-turn on that output (RetryStream's
// own "has this stream spoken yet" gate), the same as every other provider.
//
// readSSE is unmodified: this wraps it in a providerhttp.StreamOpener/
// StreamPump pair rather than splitting it, since RetryStream only needs
// its existing (body, send) shape called again on a fresh body — result and
// its error are captured by the closure for streamOneRound to read once the
// retried channel drains, which is safe because a channel close
// happens-after every send (and every closure write ahead of it) the
// closing goroutine performed.
//
// On success, the terminal EventProviderBlocks/EventUsage/EventFinish or a
// pause_turn continuation is still StreamChatCompletion's job — this
// returns only the one round's streamResult. On failure, the terminal
// EventError has already reached send by the time this returns a non-nil
// error, so the only thing left for the caller to do is stop.
func (c *Client) streamOneRound(ctx context.Context, reqBody []byte, send func(wire.Event) bool) (streamResult, error) {
	open := func(ctx context.Context) (io.ReadCloser, error) {
		resp, err := c.transport.Do(ctx, http.MethodPost, "/v1/messages", reqBody)
		if err != nil {
			return nil, c.transport.WrapError("stream request", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, parseAPIError(resp)
		}
		return resp.Body, nil
	}

	firstBody, err := open(ctx)
	if err != nil {
		send(wire.Event{Type: wire.EventError, Err: err})
		return streamResult{}, err
	}

	var result streamResult
	pump := func(ctx context.Context, body io.ReadCloser, events chan<- wire.Event) {
		defer close(events)
		forward := func(e wire.Event) bool {
			select {
			case events <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		res, err := c.readSSE(ctx, body, forward)
		result = res
		if err != nil {
			forward(wire.Event{Type: wire.EventError, Err: err})
		}
	}

	for ev := range c.transport.RetryStream(ctx, firstBody, open, pump, ErrIdleTimeout) {
		if ev.Type == wire.EventError {
			send(ev)
			return streamResult{}, ev.Err
		}
		send(ev)
	}
	return result, nil
}

// CreateChatCompletion sends one non-streaming request expressing the
// loop's intent and waits for the full response — the compaction summary's
// path. A compacted session never replays a synthetic assistant turn into
// its new history (docs/GEMINI-INTEGRATION.md §5.2's finding, which holds
// for every provider: internal/session/compact.go forks a fresh session
// with the summary in its system prompt), so this path never needs the
// raw-block replay CreateChatCompletion's caller would otherwise require.
func (c *Client) CreateChatCompletion(ctx context.Context, intent wire.ChatIntent) (*wire.ChatCompletionResponse, error) {
	req := requestFromIntent(intent)
	req.Stream = false

	var totalUsage wire.Usage
	haveUsage := false
	var finalMsg wire.Message
	var finalStop string

	for attempt := 0; attempt < maxPauseTurnResumes; attempt++ {
		body, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("anthropic: encode request: %w", err)
		}
		resp, err := c.transport.Do(ctx, http.MethodPost, "/v1/messages", body)
		if err != nil {
			return nil, c.transport.WrapError("chat completion request", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, parseAPIError(resp)
		}

		var raw messagesResponse
		err = json.NewDecoder(resp.Body).Decode(&raw)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("anthropic: decode response: %w", err)
		}
		if raw.Usage != nil {
			totalUsage = sumUsage(totalUsage, *usageFromMessages(raw.Usage))
			haveUsage = true
		}
		if raw.StopReason == "refusal" {
			return nil, &RefusalError{StopDetails: raw.StopDetails}
		}

		msg, blocksJSON := messageFromResponse(raw)
		if len(finalMsg.Content.Text) == 0 {
			finalMsg = msg
		} else {
			finalMsg.Content = wire.TextContent(finalMsg.Content.String() + msg.Content.String())
		}
		finalStop = raw.StopReason

		if raw.StopReason != "pause_turn" {
			return &wire.ChatCompletionResponse{
				ID:      raw.ID,
				Choices: []wire.Choice{{Index: 0, Message: finalMsg, FinishReason: finishReasonFor(finalStop)}},
				Usage:   usageFrom(totalUsage, haveUsage),
			}, nil
		}
		req.Messages = append(req.Messages, Message{Role: wire.RoleAssistant, Content: blocksJSON})
	}
	return nil, fmt.Errorf("anthropic: exceeded %d pause_turn resumes in one request", maxPauseTurnResumes)
}

func usageFrom(total wire.Usage, have bool) *wire.Usage {
	if !have {
		return nil
	}
	u := total
	return &u
}

// sumUsage adds b's counts onto a — used to fold several pause_turn-resumed
// responses' usage into one figure for the sub-turn the loop sees as a
// single request.
func sumUsage(a, b wire.Usage) wire.Usage {
	return wire.Usage{
		PromptTokens:     a.PromptTokens + b.PromptTokens,
		CompletionTokens: a.CompletionTokens + b.CompletionTokens,
		TotalTokens:      a.TotalTokens + b.TotalTokens,
		CachedTokens:     a.CachedTokens + b.CachedTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
	}
}

// finishReasonFor maps a Messages API stop_reason onto the finish-reason
// vocabulary wire.ChatCompletionRequest's OpenAI-format siblings use, so
// the loop's IsReasoningStarved check (a no-op here, but sharing the
// vocabulary costs nothing) and its logging read the same values regardless
// of provider.
func finishReasonFor(stopReason string) string {
	switch stopReason {
	case "tool_use":
		return wire.FinishToolCalls
	case "max_tokens":
		return wire.FinishLength
	default:
		// end_turn, stop_sequence, and anything this table does not yet
		// name.
		return wire.FinishStop
	}
}

// messageFromResponse builds the unary wire.Message and returns the
// verbatim content-blocks JSON alongside it, for CreateChatCompletion's
// pause_turn continuation.
func messageFromResponse(raw messagesResponse) (wire.Message, json.RawMessage) {
	msg := wire.Message{Role: wire.RoleAssistant}
	var text string
	for _, b := range raw.Content {
		if b.Type == "text" {
			text += b.Text
		}
	}
	msg.Content = wire.TextContent(text)
	blocksJSON, _ := json.Marshal(raw.Content)
	return msg, blocksJSON
}

func usageFromMessages(u *messagesUsage) *wire.Usage {
	if u == nil {
		return nil
	}
	return &wire.Usage{
		PromptTokens:     u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens + u.OutputTokens,
		CachedTokens:     u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
}

// UsageSplit maps one request's usage onto the cache-hit and cache-miss
// counts the cost model and the stored usage payload use. CachedTokens
// carries the cache-read figure the same single-field way Kimi K3's and
// Gemini's own usage does; the cache-write figure is not a miss — it bills
// separately at pricing.ModelPrices.InputCacheWritePerMillionUSD, read
// directly off usage.CacheWriteTokens by internal/session/turn.go rather
// than through this seam — so it is subtracted out of cacheMiss here, not
// added to it.
func (c *Client) UsageSplit(usage *wire.Usage) (cacheHit, cacheMiss int) {
	if usage == nil {
		return 0, 0
	}
	cacheHit = usage.CachedTokens
	cacheMiss = usage.PromptTokens - usage.CachedTokens - usage.CacheWriteTokens
	if cacheMiss < 0 {
		cacheMiss = 0
	}
	return cacheHit, cacheMiss
}

// CacheSlack is the churn detector's tolerance for Anthropic, measured
// against the live API rather than guessed (docs/OBSERVED.md, "Claude
// Messages API — Phase 2 live check"): five two-request pairs across all
// three models, at both "low" and "high" effort, one with a real thinking
// block replayed, showed a genuine cache miss (prompt tokens minus cache
// read minus cache write) of exactly 2 tokens every time. That is a single
// short session, not the multi-sub-turn incrementally-growing measurement
// internal/gemini.CacheSlack's own doc comment describes for its surface —
// a later phase driving a real multi-sub-turn session should tighten or
// widen this the way Gemini's Phase 8 did. 1024 is deliberately generous
// over the measured 2-token miss, headroom for the ordinary per-turn
// wrapping (a new user or tool_result message, ordinary reminder text) a
// longer conversation adds that this short probe never exercised.
func (c *Client) CacheSlack() int {
	if c.cacheSlackOverride > 0 {
		return c.cacheSlackOverride
	}
	return 1024
}

// IsReasoningStarved is a no-op. Adaptive thinking has no max_tokens-style
// pathology comparable to DeepSeek's: max_tokens caps the whole response
// (thinking and text together), and a capped response that produced no
// text at all is a request that needs a larger budget or a lower effort
// level, not a retry at double the same shape — the loop's own retry-at-
// double-budget mechanism would just as likely exhaust the same fraction of
// the new budget on thinking again. internal/gemini's own no-op reasons the
// same way for its surface's finish semantics.
func (c *Client) IsReasoningStarved(finishReason, content string) bool {
	return false
}

// RepairArguments is a no-op. Tool input arrives as a genuine JSON object
// built by this client from an accumulated, already-valid partial_json
// stream (docs/ANTHROPIC-INTEGRATION.md, "Streaming") rather than a string
// the model could mis-punctuate the way DeepSeek's does; there is no
// misplaced-brace quirk on this surface to repair, matching
// internal/gemini's own no-op for the same reason.
func (c *Client) RepairArguments(finishReason, args string) (string, bool) {
	return args, false
}
