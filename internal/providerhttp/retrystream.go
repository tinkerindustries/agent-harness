package providerhttp

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// StreamOpener issues one HTTP request for a stream and returns its body,
// already checked for a 200 status — the same shape DeepSeek's, Kimi's, and
// Gemini's own StreamChatCompletion build today before handing the body to a
// pump. RetryStream calls it again, with a fresh request, when it decides a
// dead stream is worth reopening; every other caller of Do's own retry
// machinery already covers a non-200 first response, so this is only ever
// invoked for a connection that reached a 200 and then died.
type StreamOpener func(ctx context.Context) (io.ReadCloser, error)

// StreamPump reads body into events until the stream ends, exactly as
// Transport.PumpStream and Transport.PumpStreamWith do: it takes ownership
// of body and closes it, and events is always closed when it returns. A
// provider whose frames are not DeepSeek's or Kimi's OpenAI-format chunks —
// Gemini's step-typed stream among them — supplies its own pump here and
// gets the same retry behaviour without PumpStream having any notion of its
// frame vocabulary.
type StreamPump func(ctx context.Context, body io.ReadCloser, events chan<- wire.Event)

// RetryStream runs pump against first, and — if the stream ends in a
// transient error before producing any output at all — reopens via open and
// pumps again, up to MaxRetries times on the same full-jitter backoff
// BackoffDelay gives a pre-stream retry. first is the body a caller already
// obtained from one successful open call; RetryStream never calls open for
// the first attempt, only for a retry.
//
// "Transient" and "no output" are both deliberate, narrow gates
// (docs/DESIGN.md §4.3, "Retrying a stream that dies mid-flight"):
//
//   - A read that fails with a network-level symptom — connection reset,
//     unexpected EOF, any net.Error — or with idleErr, the provider's own
//     idle-watchdog sentinel, is treated as the connection having died
//     rather than the API having refused anything. A decode error from a
//     malformed frame is not: retrying would replay the same bytes into the
//     same bug. context.Canceled and context.DeadlineExceeded are never
//     retried regardless of shape — that is the caller stopping the run on
//     purpose, and retrying it would resurrect a run the operator ended.
//   - The stream must have produced zero output — no reasoning, content,
//     tool-call, or thought-signature delta forwarded to the caller — before
//     the error arrived. Once a delta has been forwarded it may already be
//     live on a hub a browser is watching or committed by the caller
//     alongside the rest of the sub-turn; reopening the request would send
//     a second, independent completion whose text the caller has no way to
//     tell apart from the first's, so a stream that has already spoken
//     fails exactly as it did before this existed.
func (t *Transport) RetryStream(ctx context.Context, first io.ReadCloser, open StreamOpener, pump StreamPump, idleErr error) <-chan wire.Event {
	out := make(chan wire.Event)
	go func() {
		defer close(out)
		body := first
		for attempt := 0; ; attempt++ {
			inner := make(chan wire.Event)
			go pump(ctx, body, inner)

			spoke := false
			var streamErr error
			for ev := range inner {
				if ev.Type == wire.EventError {
					streamErr = ev.Err
					continue
				}
				if isOutputEvent(ev.Type) {
					spoke = true
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					return
				}
			}

			if streamErr == nil {
				return
			}
			if spoke || attempt >= t.MaxRetries || !isRetryableStreamErr(streamErr, idleErr) {
				select {
				case out <- wire.Event{Type: wire.EventError, Err: streamErr}:
				case <-ctx.Done():
				}
				return
			}

			if !t.sleepBackoff(ctx, attempt) {
				select {
				case out <- wire.Event{Type: wire.EventError, Err: ctx.Err()}:
				case <-ctx.Done():
				}
				return
			}
			newBody, err := open(ctx)
			if err != nil {
				select {
				case out <- wire.Event{Type: wire.EventError, Err: err}:
				case <-ctx.Done():
				}
				return
			}
			body = newBody
		}
	}()
	return out
}

// isOutputEvent reports whether an event is model output rather than
// bookkeeping (EventUsage, EventFinish) or the terminal EventError — the
// "has this stream spoken yet" test RetryStream gates a retry on.
func isOutputEvent(t wire.EventType) bool {
	switch t {
	case wire.EventReasoningDelta, wire.EventContentDelta, wire.EventToolCallDelta, wire.EventThoughtSignatureDelta:
		return true
	default:
		return false
	}
}

// isRetryableStreamErr classifies a stream-ending error as a dead connection
// worth reopening. See RetryStream's doc comment for the reasoning behind
// each branch.
func isRetryableStreamErr(err, idleErr error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if idleErr != nil && errors.Is(err, idleErr) {
		return true
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
