// Package providerhttp is the HTTP transport internal/deepseek and
// internal/kimi share: building a request with a per-request API key,
// retrying transient statuses with backoff, and pumping a streaming
// response's SSE frames into wire.Events behind an idle watchdog. Measured
// with comments stripped, this was ~380 lines carrying seven lines of real
// difference between the two providers' client.go, stream.go, errors.go and
// retry.go — a near-verbatim fork of one provider's plumbing into the other.
//
// It carries no provider dialect. Base URL, the retryable-status predicate
// (DeepSeek and Kimi K3 disagree by one code), the "no API key configured"
// error, the error-message prefix, and how the credential rides on the
// request are all fields a provider supplies when it builds a Transport;
// everything dialect-shaped — the request body, usage mapping, error-body
// parsing, and the DeepSeek-specific quirk repairs (docs/OBSERVED.md) —
// stays in internal/deepseek and internal/kimi, each still its own Client
// type satisfying the narrow session.Client seam (internal/session/client.go)
// independently. This package is plumbing, not a provider abstraction:
// nothing here is provider-neutral request *shape*, only the bytes-on-the-
// wire mechanics of sending one and reading a stream back
// (docs/KIMI-INTEGRATION.md §4.1).
//
// internal/gemini's agentic path (StreamChatCompletion, CreateChatCompletion)
// is a third caller of Do, added once Transport.SetAuth existed to carry its
// "x-goog-api-key" header instead of the Authorization: Bearer scheme
// DeepSeek and Kimi both use (docs/GEMINI-INTEGRATION.md §8). It never calls
// PumpStream: that method decodes wire.ChatCompletionChunk, the OpenAI
// Chat Completions shape, and Gemini's SSE frames carry a different
// vocabulary entirely (step-typed, arguments as a delta string, thought
// signatures) — internal/gemini/stream.go's pumpChatEvents reads them
// instead, over the same *http.Response.Body Do already retried into
// existence.
package providerhttp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"time"

	"github.com/mrgeoffrich/deepseek-harness/internal/wire"
)

// Transport is the retrying HTTP client both providers' public Client types
// build around. The seven fields a provider must set at construction —
// BaseURL, APIKeyProvider, HTTPClient, IdleTimeout, MaxRetries, RetryBase,
// RetryMax — are exactly the state internal/deepseek's and internal/kimi's
// Client structs used to carry directly; Retryable, NoAPIKey, ErrPrefix and
// SetAuth are the four points where a provider's own status classification,
// its own "set one with: harness config set <provider>.api_key <key>" error,
// its own name in wrapped error text, and its own credential header plug
// into otherwise-identical logic. internal/gemini is a fourth consumer of
// Do (never PumpStream — that method decodes wire.ChatCompletionChunk,
// DeepSeek's and Kimi's OpenAI-format shape, and Gemini's frames are
// nothing like it, so it keeps its own pumpChatEvents and only wants the
// retry-with-backoff Do gives it before handing the response body over).
type Transport struct {
	BaseURL        string
	APIKeyProvider func() (string, error)
	HTTPClient     *http.Client
	IdleTimeout    time.Duration
	MaxRetries     int
	RetryBase      time.Duration
	RetryMax       time.Duration

	// Retryable classifies a response status code as transient. DeepSeek
	// retries 429/500/503; Kimi K3 also retries 504 (a documented gateway
	// timeout its docs say to retry). That one-code difference is why this
	// is a field rather than logic living here.
	Retryable func(statusCode int) bool

	// NoAPIKey is returned by NewRequest, before anything is sent, when
	// APIKeyProvider supplies an empty key. WrapError lets it pass through
	// unwrapped so the operator sees the fix rather than a transport prefix
	// in front of it.
	NoAPIKey error

	// ErrPrefix names the provider ("deepseek", "kimi") in every error this
	// Transport constructs or wraps.
	ErrPrefix string

	// SetAuth sets the outgoing request's credentials, replacing the default
	// "Authorization: Bearer <key>" DeepSeek and Kimi both send. A nil value
	// keeps that default, so neither provider's ClientOption needs to touch
	// this field or change its request bytes. It exists because Gemini
	// authenticates with "x-goog-api-key: <key>" instead — one header
	// naming scheme, not a retry or backoff difference, so it earns a field
	// the same way Retryable's one-code difference does rather than a
	// second code path through newRequest.
	//
	// It sets credentials and nothing else. A provider that also needs a
	// different Accept has the Accept field below for it, so a reader can
	// see every header this Transport varies by looking at the struct
	// rather than at a callback's body.
	SetAuth func(req *http.Request, apiKey string)

	// Accept overrides the Accept header, which defaults to
	// "application/json" when this is empty — the value DeepSeek and Kimi
	// both send, so neither has to set it. Gemini's client sends
	// "text/event-stream" on every request including the non-streamed one
	// (internal/gemini/client.go, CreateChatCompletion): measured against
	// the live API, the body's own "stream" field decides the response
	// shape and the header is ignored, so one value serves both call
	// shapes.
	Accept string
}

func (t *Transport) errorf(op string, err error) error {
	return fmt.Errorf("%s: %s: %w", t.ErrPrefix, op, err)
}

// WrapError adds op to err for context, except when err is t.NoAPIKey: that
// one surfaces verbatim, so "no DeepSeek API key configured; set one with:
// harness config set deepseek.api_key <key>" reaches the operator without a
// transport prefix in front of it.
func (t *Transport) WrapError(op string, err error) error {
	if errors.Is(err, t.NoAPIKey) {
		return err
	}
	return t.errorf(op, err)
}

func (t *Transport) newRequest(ctx context.Context, method, path string, body []byte) (*http.Request, error) {
	apiKey, err := t.APIKeyProvider()
	if err != nil {
		return nil, t.errorf("resolve api key", err)
	}
	if apiKey == "" {
		return nil, t.NoAPIKey
	}
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, t.BaseURL+path, bodyReader)
	if err != nil {
		return nil, err
	}
	accept := t.Accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if t.SetAuth != nil {
		t.SetAuth(req, apiKey)
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	return req, nil
}

// Do sends one request, retrying on transient status codes (per Retryable)
// and network errors with backoff. It returns the response as-is on a
// non-retryable status so callers can decode the error body; the caller owns
// closing resp.Body in every non-error return.
func (t *Transport) Do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := t.newRequest(ctx, method, path, body)
		if err != nil {
			return nil, err
		}
		resp, err := t.HTTPClient.Do(req)
		if err != nil {
			if attempt >= t.MaxRetries || ctx.Err() != nil {
				return nil, err
			}
			if !t.sleepBackoff(ctx, attempt) {
				return nil, ctx.Err()
			}
			continue
		}
		if resp.StatusCode == http.StatusOK || !t.Retryable(resp.StatusCode) || attempt >= t.MaxRetries {
			return resp, nil
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if !t.sleepBackoff(ctx, attempt) {
			return nil, ctx.Err()
		}
	}
}

func (t *Transport) sleepBackoff(ctx context.Context, attempt int) bool {
	timer := time.NewTimer(BackoffDelay(attempt, t.RetryBase, t.RetryMax))
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// BackoffDelay returns a full-jitter exponential delay for the given retry
// attempt (0-indexed), capped at max. Both providers back off identically —
// this was duplicated byte for byte before the extraction.
func BackoffDelay(attempt int, base, max time.Duration) time.Duration {
	ceiling := base << uint(attempt)
	if ceiling <= 0 || ceiling > max {
		ceiling = max
	}
	return time.Duration(rand.Int63n(int64(ceiling) + 1))
}

// PumpStream reads body as a sequence of SSE frames, translating each data
// frame into zero or more wire.Events on events, and closes events when the
// stream ends — normally (a "[DONE]" frame or EOF), on read error, on the
// idle watchdog firing, or when ctx is cancelled. It takes ownership of body
// and closes it. idleErr is sent as the terminal event's Err when no frame,
// including a keep-alive comment, arrives within t.IdleTimeout — the
// provider's own ErrIdleTimeout, so identity checks against it
// (errors.Is(err, deepseek.ErrIdleTimeout) or kimi's) keep working across
// this extraction.
func (t *Transport) PumpStream(ctx context.Context, body io.ReadCloser, events chan<- wire.Event, idleErr error) {
	defer close(events)
	defer body.Close()

	// Every send is guarded by ctx so a caller that cancels and stops
	// reading cannot strand this goroutine and the response body on an
	// unbuffered send. send reports whether the value was delivered.
	send := func(e wire.Event) bool {
		select {
		case events <- e:
			return true
		case <-ctx.Done():
			return false
		}
	}

	lines := make(chan string)
	lineErrs := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := wire.NewSSEScanner(body)
		for {
			line, err := scanner.Scan()
			if err != nil {
				if err != io.EOF {
					lineErrs <- err
				}
				return
			}
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()

	idle := time.NewTimer(t.IdleTimeout)
	defer idle.Stop()

	for {
		select {
		case <-ctx.Done():
			send(wire.Event{Type: wire.EventError, Err: ctx.Err()})
			return

		case err := <-lineErrs:
			send(wire.Event{Type: wire.EventError, Err: t.errorf("stream read", err)})
			return

		case <-idle.C:
			send(wire.Event{Type: wire.EventError, Err: idleErr})
			return

		case line, ok := <-lines:
			if !ok {
				return
			}
			if !idle.Stop() {
				<-idle.C
			}
			idle.Reset(t.IdleTimeout)

			f := wire.ClassifySSELine(line)
			switch f.Kind {
			case wire.FrameComment, wire.FrameBlank, wire.FrameOther:
				continue
			case wire.FrameData:
				if f.Data == "[DONE]" {
					return
				}
				var chunk wire.ChatCompletionChunk
				if err := json.Unmarshal([]byte(f.Data), &chunk); err != nil {
					send(wire.Event{Type: wire.EventError, Err: t.errorf("decode chunk", err)})
					return
				}
				for _, e := range ChunkToEvents(chunk) {
					if !send(e) {
						return
					}
				}
			}
		}
	}
}

// ChunkToEvents translates one decoded SSE frame into zero or more typed
// events. Reasoning-only frames carry a nil or empty Content, per
// docs/OBSERVED.md, and never produce an EventContentDelta. Identical for
// both providers: K3 emits reasoning_content before content on its delta
// frames exactly as DeepSeek does (third_party/kimi-docs/api/chat.md
// "Streaming Response"), and the final frame carries the usage object for
// both. Exported so each provider's own stream_test.go — which still needs
// its own usage-shape cases, DeepSeek's hit/miss pair versus Kimi's single
// cached_tokens — can exercise it directly.
func ChunkToEvents(chunk wire.ChatCompletionChunk) []wire.Event {
	var events []wire.Event
	if len(chunk.Choices) > 0 {
		choice := chunk.Choices[0]
		d := choice.Delta
		if d.ReasoningContent != nil && *d.ReasoningContent != "" {
			events = append(events, wire.Event{Type: wire.EventReasoningDelta, Reasoning: *d.ReasoningContent})
		}
		if d.Content != nil && *d.Content != "" {
			events = append(events, wire.Event{Type: wire.EventContentDelta, Content: *d.Content})
		}
		for _, tc := range d.ToolCalls {
			events = append(events, wire.Event{Type: wire.EventToolCallDelta, ToolCall: tc})
		}
		if choice.FinishReason != nil {
			events = append(events, wire.Event{Type: wire.EventFinish, FinishReason: *choice.FinishReason})
		}
	}
	if chunk.Usage != nil {
		events = append(events, wire.Event{Type: wire.EventUsage, Usage: chunk.Usage})
	}
	return events
}
