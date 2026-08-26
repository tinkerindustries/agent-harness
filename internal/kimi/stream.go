package kimi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// ErrIdleTimeout is sent when no SSE frame, including a keep-alive comment,
// arrives within the idle window. It is not a request timeout: Kimi can hold
// a request before inference starts, and streaming keep-alives are expected
// during that wait — the same watchdog internal/deepseek runs, sized the
// same way (docs/DESIGN.md §4.3).
var ErrIdleTimeout = errors.New("kimi: stream idle timeout")

// StreamChatCompletion sends one streaming completion expressing the
// caller's intent and returns a channel of typed deltas. The request is
// built from the intent here, in the provider, so the caller never spells
// Kimi's reasoning control (docs/KIMI-INTEGRATION.md §4.1). The channel
// closes when the stream ends, normally or by error; a terminal Event with
// Type EventError is always the last event sent before it closes. Reading
// the stream itself — the SSE pump and its idle watchdog — is
// internal/providerhttp.Transport.PumpStream, shared with internal/deepseek.
func (c *Client) StreamChatCompletion(ctx context.Context, intent wire.ChatIntent) (<-chan wire.Event, error) {
	req := requestFromIntent(intent)
	req.Stream = true
	req.StreamOptions = &wire.StreamOptions{IncludeUsage: true}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("kimi: encode request: %w", err)
	}

	resp, err := c.transport.Do(ctx, http.MethodPost, "/chat/completions", body)
	if err != nil {
		return nil, c.transport.WrapError("stream request", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, parseAPIError(resp)
	}

	events := make(chan wire.Event)
	go c.transport.PumpStream(ctx, resp.Body, events, ErrIdleTimeout)
	return events, nil
}

// IsReasoningStarved reports not-starved, always. Reasoning-before-answer
// starvation under a small max_tokens budget is DeepSeek's behaviour,
// recorded in docs/OBSERVED.md; K3 always reasons too (it cannot run
// non-thinking), so the failure mode is conceivable, but this phase makes no
// live API calls and nothing observed about Kimi's API says it happens.
// Reporting false keeps the loop on its ordinary single-request path instead
// of retrying at double budget on a guess; a live Phase 6 run is where this
// gets decided (docs/KIMI-INTEGRATION.md §6).
func (c *Client) IsReasoningStarved(finishReason, content string) bool {
	return false
}
