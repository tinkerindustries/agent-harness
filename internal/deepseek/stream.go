package deepseek

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// ErrIdleTimeout is sent when no SSE frame, including a keep-alive comment,
// arrives within the idle window. It is not a request timeout: DeepSeek can
// hold a request up to ten minutes before inference starts, and streaming
// keep-alives are expected during that wait.
var ErrIdleTimeout = errors.New("deepseek: stream idle timeout")

// StreamChatCompletion sends one streaming completion expressing the
// caller's intent and returns a channel of typed deltas. The request is
// built from the intent here, in the provider, so the caller never spells
// DeepSeek's reasoning control (docs/KIMI-INTEGRATION.md §4.1). The channel
// closes when the stream ends, normally or by error; a terminal Event with
// Type EventError is always the last event sent before it closes. Reading
// the stream itself — the SSE pump and its idle watchdog — is
// internal/providerhttp.Transport.PumpStream, shared with internal/kimi.
//
// A connection that dies before producing any output is retried on
// providerhttp.Transport.RetryStream's own backoff schedule rather than
// ending the run (docs/DESIGN.md §4.3, "Retrying a stream that dies
// mid-flight"); open is what RetryStream re-runs for a retry, so it holds
// exactly the request-building and status-checking this method used to do
// inline.
func (c *Client) StreamChatCompletion(ctx context.Context, intent wire.ChatIntent) (<-chan wire.Event, error) {
	req := requestFromIntent(intent)
	req.Stream = true
	req.StreamOptions = &wire.StreamOptions{IncludeUsage: true}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("deepseek: encode request: %w", err)
	}

	open := func(ctx context.Context) (io.ReadCloser, error) {
		resp, err := c.transport.Do(ctx, http.MethodPost, "/chat/completions", body)
		if err != nil {
			return nil, c.transport.WrapError("stream request", err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, parseAPIError(resp)
		}
		return resp.Body, nil
	}

	respBody, err := open(ctx)
	if err != nil {
		return nil, err
	}

	pump := func(ctx context.Context, body io.ReadCloser, events chan<- wire.Event) {
		c.transport.PumpStream(ctx, body, events, ErrIdleTimeout)
	}
	return c.transport.RetryStream(ctx, respBody, open, pump, ErrIdleTimeout), nil
}

// IsReasoningStarved reports whether a completion hit its max_tokens
// ceiling before producing any answer text. Reasoning is generated before
// content, so an undersized budget is spent entirely on reasoning and bills
// in full while returning nothing usable; this is a distinct condition from
// an answer that was truncated mid-sentence (docs/OBSERVED.md). It is
// DeepSeek's quirk — how the loop reacts to it lives behind the session
// seam, and Kimi's implementation is free to do nothing
// (docs/KIMI-INTEGRATION.md §4.1).
func (c *Client) IsReasoningStarved(finishReason, content string) bool {
	return finishReason == wire.FinishLength && content == ""
}
