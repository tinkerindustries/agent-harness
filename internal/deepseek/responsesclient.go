package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// ResponsesClient talks to the same DeepSeek base URL as Client, over the
// Responses API (`POST /responses`) instead of Chat Completions. It
// satisfies the same narrow seam internal/session declares, so the agent
// loop above it cannot tell which surface it is on
// (docs/KIMI-INTEGRATION.md §4.1).
//
// It embeds *Client rather than reimplementing it: the transport, the
// per-request key, the retry classification, the usage split, and both
// response-quirk repairs are the provider's, not the surface's, and are
// identical on either. What this type replaces is the two request methods —
// the body they send and the frames they read back.
//
// A session speaks one surface for its whole life. The frozen prefix the
// prompt cache is built on is the serialised head of the request, and the
// two dialects do not serialise alike, so a session that changed surface
// mid-run would re-read its whole conversation at full price
// (docs/DESIGN.md §3.2, docs/CACHE.md).
type ResponsesClient struct {
	*Client
}

// NewResponsesClient builds a ResponsesClient for baseURL using apiKey. The
// options are Client's own and mean the same things.
func NewResponsesClient(baseURL, apiKey string, opts ...ClientOption) *ResponsesClient {
	return &ResponsesClient{Client: NewClient(baseURL, apiKey, opts...)}
}

// ErrResponseFailed is the class of error a `response.failed` terminal event
// carries. The provider's own message is wrapped rather than replaced.
var ErrResponseFailed = errors.New("deepseek: response failed")

// StreamChatCompletion sends one streaming response expressing the caller's
// intent and returns a channel of typed deltas. The name is the seam's, not
// this surface's: it is what internal/session calls to stream a turn, and
// this implementation happens to stream it from /responses.
//
// There is no stream_options here — the surface does not take one, and does
// not need one: usage rides on the terminal event's response object rather
// than on an extra final chunk a caller has to ask for.
func (c *ResponsesClient) StreamChatCompletion(ctx context.Context, intent wire.ChatIntent) (<-chan wire.Event, error) {
	req := responsesRequestFromIntent(intent)
	req.Stream = true
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("deepseek: encode responses request: %w", err)
	}

	resp, err := c.transport.Do(ctx, http.MethodPost, "/responses", body)
	if err != nil {
		return nil, c.transport.WrapError("responses stream request", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}

	events := make(chan wire.Event)
	go c.transport.PumpStreamWith(ctx, resp.Body, events, ErrIdleTimeout, decodeResponsesFrame)
	return events, nil
}

// CreateChatCompletion sends one non-streaming response and waits for the
// whole thing, shaped as the chat completion body the loop's non-streaming
// callers already read (compaction, the eval judge, WebFetch). The
// translation is messageFromResponse's: output items flattened back into
// one assistant message.
func (c *ResponsesClient) CreateChatCompletion(ctx context.Context, intent wire.ChatIntent) (*wire.ChatCompletionResponse, error) {
	body, err := json.Marshal(responsesRequestFromIntent(intent))
	if err != nil {
		return nil, fmt.Errorf("deepseek: encode responses request: %w", err)
	}

	resp, err := c.transport.Do(ctx, http.MethodPost, "/responses", body)
	if err != nil {
		return nil, c.transport.WrapError("responses request", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}
	defer resp.Body.Close()

	var out responseObject
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("deepseek: decode response: %w", err)
	}
	if out.Status == statusFailed && out.Error != nil {
		return nil, fmt.Errorf("%w: %s", ErrResponseFailed, out.Error.Message)
	}
	return &wire.ChatCompletionResponse{
		ID:      out.ID,
		Object:  out.Object,
		Created: out.CreatedAt,
		Model:   out.Model,
		Choices: []wire.Choice{{
			Index:        0,
			Message:      messageFromResponse(&out),
			FinishReason: finishReasonFor(&out),
		}},
		Usage: usageFromResponses(out.Usage),
	}, nil
}
