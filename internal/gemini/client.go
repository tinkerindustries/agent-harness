// Package gemini is a client for Google's Gemini API. Two callers use it:
// the vision tools — Glance, Ground, and Detect — which send images DeepSeek
// cannot see to a vision model and return its answer or located boxes
// (docs/TOOLS.md, "Glance" and "Ground and Detect") through Interact, and
// the agent loop, which drives a full coding session on gemini-3.7-flash
// through StreamChatCompletion and CreateChatCompletion — the
// internal/session.Client seam DeepSeek and Kimi already satisfy
// (docs/GEMINI-INTEGRATION.md). Request and response bodies are Go structs,
// never map[string]any, so identical values always serialise to identical
// bytes — the byte-stability contract the prompt cache depends on
// (docs/DESIGN.md §3.2), which matters here exactly as much as it does for
// the other two providers.
//
// The request is sent to POST {base}/v1beta/interactions for both callers;
// docs/gemini-3.5-flash-ui-review-prompting.md's sources document the shapes
// Interact uses (thinking_level inside generation_config, resolution as a
// per-image key on the image part), and third_party/gemini-docs/openapi.json
// is the authoritative reference for the agentic shapes chat_types.go,
// intent.go and stream.go add — prefer it over prose wherever they disagree
// about a field name or type. Both surfaces use snake_case JSON, so this
// package does too.
//
// Interact, InteractionRequest, InteractionResponse, Step and decodeStream
// are the vision path and are untouched by the agentic addition: they are
// in production (internal/tools/vision.go depends on Interact directly) and
// must keep producing identical request bytes. Everything the agentic path
// needs — ChatInteractionRequest and its step types (chat_types.go),
// requestFromIntent (intent.go), pumpChatEvents and IsReasoningStarved
// (stream.go), RepairArguments (toolcall.go), and the SSE-and-plain error
// parsing errors.go adds — is additive, not a refactor of what Interact
// already does. The one dependency this adds package-wide is
// internal/wire, for wire.ChatIntent, wire.Event and the message and tool
// vocabulary the session.Client seam is built from.
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
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

// DefaultBaseURL is the host the client talks to when NewClient is given an
// empty string.
const DefaultBaseURL = "https://generativelanguage.googleapis.com"

// DefaultModel is the vision model Glance, Ground, and Detect use when no
// google.vision_model setting is stored; it is also internal/settings's
// default for that key. The two are pinned equal by
// TestGeminiDefaultModelMatchesTheRegistry — this constant is the fallback
// on a path that could not reach the store, so a disagreement between them
// is a silent change of model on exactly the runs least able to report it.
const DefaultModel = "gemini-3.7-flash"

// Thinking levels for generation_config.thinking_level (docs/gemini-3.5
// -flash-ui-review-prompting.md, "Control reasoning depth").
const (
	ThinkingLevelMinimal = "minimal"
	ThinkingLevelLow     = "low"
	ThinkingLevelMedium  = "medium"
	ThinkingLevelHigh    = "high"
)

// Resolution values for a per-image part resolution (the same doc, "Image
// resolution"). "unspecified" is the API's default and is never sent.
const (
	ResolutionLow       = "low"
	ResolutionMedium    = "medium"
	ResolutionHigh      = "high"
	ResolutionUltraHigh = "ultra_high"
)

// ErrNoAPIKey is returned before a request is sent when the key provider
// supplies an empty key — the operator's fix is named rather than Gemini's
// 400 being what they see.
var ErrNoAPIKey = errors.New("no Google API key configured; set one with: harness config set google.api_key <key>")

// Client talks to a single Gemini base URL. The API key is supplied per
// request by a provider, so a key stored in the database can change while
// the client lives without rebuilding it.
type Client struct {
	baseURL        string
	apiKeyProvider func() (string, error)
	httpClient     *http.Client
	// chatIdleTimeout overrides pumpChatEvents' watchdog window
	// (stream.go's idleTimeout); zero means defaultIdleTimeout. Interact has
	// no equivalent — see NewClient's own comment on why it sets no
	// response-header timeout — but the agentic streaming path keeps a
	// watchdog, matching DeepSeek's and Kimi's streams.
	chatIdleTimeout time.Duration
}

// ClientOption customises a Client built by NewClient.
type ClientOption func(*Client)

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

// WithChatIdleTimeout overrides the agentic streaming path's idle watchdog
// window (stream.go's pumpChatEvents), e.g. in tests. It has no effect on
// Interact, which sets no response-header timeout at all (NewClient's own
// comment explains why).
func WithChatIdleTimeout(d time.Duration) ClientOption {
	return func(c *Client) { c.chatIdleTimeout = d }
}

// WithTransportWrapper wraps the client's existing transport, e.g. to
// capture traffic. The default transport's dial, TLS handshake, and
// response header timeouts survive because the wrapper replaces the
// Transport field, not the http.Client.
func WithTransportWrapper(wrap func(http.RoundTripper) http.RoundTripper) ClientOption {
	return func(c *Client) { c.httpClient.Transport = wrap(c.httpClient.Transport) }
}

// NewClient builds a Client for baseURL. With no WithAPIKeyProvider option,
// every request fails locally with ErrNoAPIKey: cmd/harness always supplies
// a provider reading the key from the settings table, so the empty default
// is the safe one. The default HTTP client sets no overall request timeout
// and no response header timeout: the tool-level context is the call's only
// deadline, so raising tools.reviewscreenshot_timeout actually raises it.
// Dial and TLS handshake timeouts still bound connection setup.
//
// A response header timeout cannot be set here. Interactions are not
// streamed, so nothing arrives until the model has finished thinking, and a
// multi-image review at medium thinking reaches 29s of it
// (docs/reviews/sess-8df2a5f78847c4737b0e86e6e5d069b6.md). internal/deepseek
// and internal/kimi keep theirs because they stream.
func NewClient(baseURL string, opts ...ClientOption) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	c := &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKeyProvider: func() (string, error) {
			return "", nil
		},
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
				TLSHandshakeTimeout: 15 * time.Second,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Image is one screenshot to send to the model. Data is the raw file bytes,
// MIMEType one of image/png, image/jpeg, or image/webp, and Resolution one
// of the Resolution constants. Only the image that needs the closest
// scrutiny should be ResolutionHigh; the rest should be ResolutionMedium to
// save tokens (docs/gemini-3.5-flash-ui-review-prompting.md).
//
// Label names the image to the model: Interact emits a text part carrying it
// ("Image 1: <label>") immediately before the image part, so a finding can
// say which screenshot it is about instead of referring to a position in the
// array. Glance, Ground, and Detect all set it to the file's base name.
type Image struct {
	Data       []byte
	MIMEType   string
	Resolution string
	Label      string
}

// interactConfig holds the per-call generation settings an InteractOption
// can move. The defaults are what every call got before the options existed.
type interactConfig struct {
	thinkingLevel  string
	responseFormat string
}

// InteractOption customises one Interact call.
type InteractOption func(*interactConfig)

// WithThinkingLevel sets generation_config.thinking_level for the call.
//
// Thinking is where a vision call's cost goes: measured over two production
// sessions, an empty findings list cost 1,947 thinking tokens against one
// token of answer (docs/reviews/vision-path-2026-08-14.md). Glance defaults
// to medium, for answering a question about a page; Ground and Detect default
// to low, on the grounds that locating something is perception rather than
// reasoning (internal/tools/vision.go).
func WithThinkingLevel(level string) InteractOption {
	return func(c *interactConfig) {
		if level != "" {
			c.thinkingLevel = level
		}
	}
}

// WithResponseFormat sets the top-level response_format type — "array",
// "object", and the rest of the set ResponseFormat documents. The empty
// string omits the field, which is a deliberate choice rather than a no-op:
// the type constrains the container and says nothing about its contents, so
// asking for "object" returns a literally empty object. Leaving the model to
// choose its own container is what actually produces the shape the system
// instruction asked for (docs/gemini-3.5-flash-ui-review-prompting.md,
// "response_format is a type, not a schema — measured").
func WithResponseFormat(format string) InteractOption {
	return func(c *interactConfig) { c.responseFormat = format }
}

// Interact sends one interaction to model with the screenshots first
// and question last ("data first, question last", per the doc), plus
// systemInstruction as the system instruction. It returns the model's text
// and, when the API reported one, the call's usage for cost accounting
// (internal/session commits it as its own usage event, the same way a
// DeepSeek turn's usage is). The key is resolved per request; an empty key
// fails before anything is sent.
//
// With no options the call is what it has always been: medium thinking, and
// an array response format so the answer arrives as bare JSON rather than
// inside a ```json fence (measured against the live API, item
// "response_format" in the follow-up brief).
func (c *Client) Interact(ctx context.Context, model, systemInstruction, question string, images []Image, opts ...InteractOption) (string, *Usage, error) {
	if model == "" {
		model = DefaultModel
	}
	cfg := interactConfig{thinkingLevel: ThinkingLevelMedium, responseFormat: "array"}
	for _, opt := range opts {
		opt(&cfg)
	}
	req := InteractionRequest{
		Model:             model,
		SystemInstruction: systemInstruction,
		GenerationConfig:  &GenerationConfig{ThinkingLevel: cfg.thinkingLevel},
		Input:             make([]Content, 0, len(images)+1),
		Stream:            true,
	}
	if cfg.responseFormat != "" {
		req.ResponseFormat = &ResponseFormat{Type: cfg.responseFormat}
	}
	for i, img := range images {
		// The label rides as its own text part immediately before the image,
		// so the model can name the screenshot a finding is about rather than
		// referring to a position in the array. The numbering is 1-based, the
		// order a human sees the images in.
		if img.Label != "" {
			req.Input = append(req.Input, Content{
				Type: ContentTypeText,
				Text: fmt.Sprintf("Image %d: %s", i+1, img.Label),
			})
		}
		req.Input = append(req.Input, Content{
			Type:       ContentTypeImage,
			MIMEType:   img.MIMEType,
			Data:       base64.StdEncoding.EncodeToString(img.Data),
			Resolution: img.Resolution,
		})
	}
	req.Input = append(req.Input, Content{Type: ContentTypeText, Text: question})

	resp, err := c.interact(ctx, req)
	if err != nil {
		return "", nil, err
	}
	text := resp.Text()
	if text == "" {
		return "", nil, fmt.Errorf("gemini: no text output in response (status %q, id %q)", resp.Status, resp.ID)
	}
	return text, resp.Usage, nil
}

func (c *Client) interact(ctx context.Context, req InteractionRequest) (*InteractionResponse, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: encode request: %w", err)
	}

	httpReq, err := c.newRequest(ctx, "/v1beta/interactions", body)
	if err != nil {
		return nil, wrapClientError("interaction request", err)
	}

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: interaction request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// A refused request answers in JSON even when it asked for a stream,
		// so the error path is the same as it ever was.
		return nil, parseAPIError(resp)
	}
	defer resp.Body.Close()

	return decodeStream(resp.Body)
}

// newRequest resolves the API key and builds the request. The key rides in
// the x-goog-api-key header, never in the URL: a URL carrying a secret ends
// up in logs and error messages.
func (c *Client) newRequest(ctx context.Context, path string, body []byte) (*http.Request, error) {
	apiKey, err := c.apiKeyProvider()
	if err != nil {
		return nil, fmt.Errorf("gemini: resolve api key: %w", err)
	}
	if apiKey == "" {
		return nil, ErrNoAPIKey
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("x-goog-api-key", apiKey)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// wrapClientError adds operation context to err, except for ErrNoAPIKey:
// that one surfaces verbatim, so the operator sees the fix — "no Google API
// key configured; set one with: harness config set google.api_key <key>" —
// without a transport prefix in front of it.
func wrapClientError(op string, err error) error {
	if errors.Is(err, ErrNoAPIKey) {
		return err
	}
	return fmt.Errorf("gemini: %s: %w", op, err)
}

// parseAPIError reads and closes resp.Body, building an error from Gemini's
// {"error": {"code": ..., "message": ..., "status": ...}} envelope, falling
// back to the raw body if that does not parse.
func parseAPIError(resp *http.Response) error {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))

	var wrapped struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Status  string `json:"status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wrapped); err == nil && wrapped.Error.Message != "" {
		return fmt.Errorf("gemini: %d %s: %s", wrapped.Error.Code, wrapped.Error.Status, wrapped.Error.Message)
	}
	return fmt.Errorf("gemini: unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
}

// StreamChatCompletion sends one streaming interaction expressing the
// agent loop's intent and returns a channel of typed deltas — the
// session.Client half of this package (docs/GEMINI-INTEGRATION.md §5.1).
// The request is built from the intent here, in the provider, so the loop
// never spells Gemini's step-typed history or its flattened tool schema
// (intent.go). The channel closes when the stream ends, normally or by
// error; a terminal wire.Event with Type EventError or EventFinish is
// always the last event sent before it closes (stream.go's
// pumpChatEvents).
func (c *Client) StreamChatCompletion(ctx context.Context, intent wire.ChatIntent) (<-chan wire.Event, error) {
	req := requestFromIntent(intent)
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: encode request: %w", err)
	}

	httpReq, err := c.newRequest(ctx, "/v1beta/interactions", body)
	if err != nil {
		return nil, wrapClientError("stream request", err)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: stream request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// docs/OBSERVED.md's central finding for this phase: a bad or
		// missing thought signature answers 400 with an SSE-framed body,
		// not the plain JSON parseAPIError (the vision path's own error
		// reader) expects.
		return nil, parseAgenticAPIError(resp)
	}

	events := make(chan wire.Event)
	go c.pumpChatEvents(ctx, resp.Body, events)
	return events, nil
}

// CreateChatCompletion sends one non-streaming interaction expressing the
// loop's intent and waits for the full response — the compaction summary's
// path (internal/session.Client's doc comment). stream:false is a
// genuinely different response shape here, not merely Interact's own
// always-streamed request read to completion: docs/OBSERVED.md's "Unary
// (stream: false)" finding is that a thought step's signature sits directly
// on the step object, not nested in a delta, which is exactly the shape
// chatStep.Signature and chatCompletionResponseFromRaw below read.
//
// c.newRequest below sets Accept: text/event-stream unconditionally, the
// same header Interact and StreamChatCompletion send; a live measurement
// confirmed this is harmless for a stream:false body — both
// Accept: text/event-stream and Accept: application/json against
// stream:false return Content-Type: application/json — so the request's
// own stream field controls the response shape, not the Accept header.
func (c *Client) CreateChatCompletion(ctx context.Context, intent wire.ChatIntent) (*wire.ChatCompletionResponse, error) {
	req := requestFromIntent(intent)
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("gemini: encode request: %w", err)
	}

	httpReq, err := c.newRequest(ctx, "/v1beta/interactions", body)
	if err != nil {
		return nil, wrapClientError("chat completion request", err)
	}
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: chat completion request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAgenticAPIError(resp)
	}
	defer resp.Body.Close()

	var raw chatInteractionResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("gemini: decode response: %w", err)
	}
	return chatCompletionResponseFromRaw(&raw), nil
}

// chatCompletionResponseFromRaw turns the unary agentic response into the
// shared wire.ChatCompletionResponse shape: one Choice, since the
// Interactions surface returns exactly one candidate the way DeepSeek and
// Kimi do, built by walking the steps in order — a thought step's signature
// onto Message.ThoughtSignature, function_call steps onto Message.ToolCalls,
// a model_output step's text onto Message.Content. Both together (a
// function_call after a thought) is the shape docs/OBSERVED.md's captured
// round trip shows; a model_output alongside tool calls has never been
// observed and is not expected, so the two are treated as alternatives, the
// same as requestFromIntent's own reading of an assistant message.
func chatCompletionResponseFromRaw(raw *chatInteractionResponse) *wire.ChatCompletionResponse {
	msg := wire.Message{Role: wire.RoleAssistant}
	var content strings.Builder
	for _, s := range raw.Steps {
		switch s.Type {
		case StepTypeThought:
			if s.Signature != "" {
				sig := s.Signature
				msg.ThoughtSignature = &sig
			}
		case StepTypeFunctionCall:
			msg.ToolCalls = append(msg.ToolCalls, wire.ToolCall{
				ID:   s.ID,
				Type: "function",
				Function: wire.ToolCallFunc{
					Name:      s.Name,
					Arguments: argumentsFromObject(s.Arguments),
				},
			})
		case StepTypeModelOutput:
			for _, c := range s.Content {
				if c.Type == ContentTypeText {
					content.WriteString(c.Text)
				}
			}
		}
	}
	msg.Content = wire.TextContent(content.String())

	// finishReasonFor (stream.go) is the same synthesis the streaming path
	// uses, so a capped unary call and a capped streamed call report the
	// same FinishLength — the raw response's own Status is the unary
	// equivalent of the interaction.completed frame's Status the streaming
	// path reads.
	finishReason := finishReasonFor(chatStreamState{sawFunctionCall: len(msg.ToolCalls) > 0, status: raw.Status})
	return &wire.ChatCompletionResponse{
		ID:      raw.ID,
		Choices: []wire.Choice{{Index: 0, Message: msg, FinishReason: finishReason}},
		Usage:   usageToWire(raw.Usage),
	}
}

// usageToWire maps Gemini's usage shape onto the wire.Usage every provider's
// UsageSplit reads. It reuses Usage.TokenSplit's own reasoning — thought
// tokens bill at the output rate, cached tokens are a subset of input
// (types.go's TokenSplit doc comment) — so this and TokenSplit must not
// drift apart; TestUsageToWireMatchesTokenSplit pins that they cannot. The
// cache split rides CachedTokens the same single-field way Kimi K3's own
// usage does (wire.Usage's own doc comment), which is what lets
// Client.UsageSplit below read it with the identical formula Kimi's does.
func usageToWire(u *Usage) *wire.Usage {
	if u == nil {
		return nil
	}
	_, _, completion, reasoning := u.TokenSplit()
	return &wire.Usage{
		PromptTokens:            u.TotalInputTokens,
		CompletionTokens:        completion,
		TotalTokens:             u.TotalTokens,
		CachedTokens:            u.TotalCachedTokens,
		CompletionTokensDetails: &wire.CompletionTokensDetails{ReasoningTokens: reasoning},
	}
}

// UsageSplit maps one request's usage onto the cache-hit and cache-miss
// counts the cost model and the stored usage payload use. usageToWire above
// carries Gemini's cache figure through CachedTokens, the same single-field
// shape Kimi K3 reports (wire.Usage's own doc comment), so the split is the
// identical formula internal/kimi's own UsageSplit uses: the hit is
// CachedTokens itself, the miss is everything else in the prompt. The
// clamp against a negative miss guards a malformed report the same way
// Usage.TokenSplit's own cacheMiss already does; a nil usage — the event
// never arrived — maps to zeroes.
func (c *Client) UsageSplit(usage *wire.Usage) (cacheHit, cacheMiss int) {
	if usage == nil {
		return 0, 0
	}
	cacheHit = usage.CachedTokens
	cacheMiss = usage.PromptTokens - usage.CachedTokens
	if cacheMiss < 0 {
		cacheMiss = 0
	}
	return cacheHit, cacheMiss
}

// CacheSlack is the churn detector's tolerance for Gemini: PROVISIONAL.
// Phase 2 could not measure this — the two-request cache warm-up
// (docs/OBSERVED.md, "Implicit caching works under store: false, after a
// warm-up") means the churn detector's own prediction-vs-reality comparison
// needs a live multi-sub-turn session to produce a real bound, which no
// phase before Phase 8 runs. 8192 is deliberately loose — a wide multiple of
// Kimi K3's own empirical bound of 512 (docs/OBSERVED.md) — so the churn
// diagnostic stays quiet rather than false-alarming on Gemini sessions
// before Phase 8 measures the true figure and replaces this.
func (c *Client) CacheSlack() int { return 8192 }
