// Package gemini is a client for Google's Gemini API, used by the
// ReviewScreenshot tool to send screenshots DeepSeek cannot see to a vision
// model and return its findings (docs/TOOLS.md, "ReviewScreenshot"). Request
// and response bodies are Go structs, never map[string]any, so identical
// values always serialise to identical bytes.
//
// The request is sent to POST {base}/v1beta/interactions, the surface
// docs/gemini-3.5-flash-ui-review-prompting.md's sources document: the
// whats-new page shows thinking_level inside generation_config, and the
// media-resolution page shows resolution as a per-image key on the image
// part. Both pages use snake_case JSON, so this package does too. The
// package has no dependency on any other internal package.
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
)

// DefaultBaseURL is the host the client talks to when NewClient is given an
// empty string.
const DefaultBaseURL = "https://generativelanguage.googleapis.com"

// DefaultModel is the vision model ReviewScreenshot uses when no
// google.vision_model setting is stored; it is also internal/settings's
// default for that key.
const DefaultModel = "gemini-3.5-flash"

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
// array. ReviewScreenshot sets it to the file's base name.
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
// Thinking is where a review's cost goes: measured over two production
// sessions, an empty findings list cost 1,947 thinking tokens against one
// token of answer (docs/reviews/vision-path-2026-08-14.md). Medium is right
// for judging a page against a spec; a call that only has to say what is on
// the screen does not need it, which is why ReviewScreenshot's describe mode
// drops to low.
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
		return nil, parseAPIError(resp)
	}
	defer resp.Body.Close()

	var out InteractionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("gemini: decode response: %w", err)
	}
	return &out, nil
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
	req.Header.Set("Accept", "application/json")
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
