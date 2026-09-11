package providerhttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// fakeNetErr satisfies net.Error without opening a real connection, so a
// test can assert RetryStream's classifier treats a network-level symptom
// as transient without depending on OS-level socket behaviour.
type fakeNetErr struct{ msg string }

func (e *fakeNetErr) Error() string   { return e.msg }
func (e *fakeNetErr) Timeout() bool   { return false }
func (e *fakeNetErr) Temporary() bool { return false }

// newRetryTestTransport builds a Transport with backoff shrunk to
// millisecond scale, so a test exercising a real retry sleep does not take
// real time to run.
func newRetryTestTransport(maxRetries int) *Transport {
	return &Transport{
		MaxRetries: maxRetries,
		RetryBase:  time.Millisecond,
		RetryMax:   5 * time.Millisecond,
	}
}

func drainStream(t *testing.T, ch <-chan wire.Event) []wire.Event {
	t.Helper()
	var out []wire.Event
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatal("timed out waiting for the retried stream to close")
		}
	}
}

func nopBody() io.ReadCloser { return io.NopCloser(strings.NewReader("")) }

// TestRetryStreamRetriesBeforeAnyOutput is the production incident directly:
// a stream that dies on a network-level error before producing a single
// delta is retried, and the caller sees the retry's output as if nothing
// had gone wrong.
func TestRetryStreamRetriesBeforeAnyOutput(t *testing.T) {
	tr := newRetryTestTransport(4)
	calls := 0
	opens := 0
	pump := func(ctx context.Context, body io.ReadCloser, events chan<- wire.Event) {
		defer close(events)
		defer body.Close()
		calls++
		if calls == 1 {
			events <- wire.Event{Type: wire.EventError, Err: &fakeNetErr{"read: connection reset by peer"}}
			return
		}
		events <- wire.Event{Type: wire.EventContentDelta, Content: "hello"}
		events <- wire.Event{Type: wire.EventFinish, FinishReason: "stop"}
	}
	open := func(ctx context.Context) (io.ReadCloser, error) {
		opens++
		return nopBody(), nil
	}

	events := drainStream(t, tr.RetryStream(context.Background(), nopBody(), open, pump, nil))

	if calls != 2 {
		t.Errorf("pump ran %d time(s), want 2 (the failed attempt plus the retry)", calls)
	}
	if opens != 1 {
		t.Errorf("open ran %d time(s), want 1: only the retry re-opens, the first attempt's body is the caller's own", opens)
	}
	for _, ev := range events {
		if ev.Type == wire.EventError {
			t.Fatalf("caller saw an error event %v; the retry should have been invisible to it", ev.Err)
		}
	}
	if len(events) != 2 || events[0].Type != wire.EventContentDelta || events[0].Content != "hello" {
		t.Fatalf("events = %+v, want the retry's content delta then finish", events)
	}
}

// TestRetryStreamDoesNotRetryAfterOutput is the partial-output decision:
// once a delta has reached the caller, a later transient error on the same
// stream is forwarded exactly as it would have been before RetryStream
// existed, because retrying would send a second, independent completion
// duplicating what the caller already published or committed.
func TestRetryStreamDoesNotRetryAfterOutput(t *testing.T) {
	tr := newRetryTestTransport(4)
	opens := 0
	failure := &fakeNetErr{"read: connection reset by peer"}
	pump := func(ctx context.Context, body io.ReadCloser, events chan<- wire.Event) {
		defer close(events)
		defer body.Close()
		events <- wire.Event{Type: wire.EventContentDelta, Content: "partial"}
		events <- wire.Event{Type: wire.EventError, Err: failure}
	}
	open := func(ctx context.Context) (io.ReadCloser, error) {
		opens++
		return nopBody(), nil
	}

	events := drainStream(t, tr.RetryStream(context.Background(), nopBody(), open, pump, nil))

	if opens != 0 {
		t.Errorf("open ran %d time(s), want 0: a stream that already spoke must never be reopened", opens)
	}
	if len(events) != 2 || events[0].Type != wire.EventContentDelta || events[1].Type != wire.EventError || !errors.Is(events[1].Err, failure) {
		t.Fatalf("events = %+v, want the content delta then the original error unmodified", events)
	}
}

// TestRetryStreamNeverRetriesCancelledContext pins that context.Canceled is
// excluded from the retryable classification regardless of how it reached
// RetryStream — including, per the production incident, wrapped inside a
// "stream read" error rather than arriving through ctx.Done() directly. A
// cancelled context is the operator stopping the run on purpose; retrying it
// would resurrect a run they ended.
func TestRetryStreamNeverRetriesCancelledContext(t *testing.T) {
	tr := newRetryTestTransport(4)
	opens := 0
	wrapped := fmt.Errorf("deepseek: stream read: %w", context.Canceled)
	pump := func(ctx context.Context, body io.ReadCloser, events chan<- wire.Event) {
		defer close(events)
		defer body.Close()
		events <- wire.Event{Type: wire.EventError, Err: wrapped}
	}
	open := func(ctx context.Context) (io.ReadCloser, error) {
		opens++
		return nopBody(), nil
	}

	events := drainStream(t, tr.RetryStream(context.Background(), nopBody(), open, pump, nil))

	if opens != 0 {
		t.Errorf("open ran %d time(s), want 0: a cancelled context must never be retried", opens)
	}
	if len(events) != 1 || !errors.Is(events[0].Err, context.Canceled) {
		t.Fatalf("events = %+v, want the cancellation forwarded once", events)
	}
}

// TestRetryStreamDoesNotRetryDecodeError pins that a non-network,
// non-idle-timeout error — a malformed frame, say — is never retried: the
// bytes on the wire would be identical on a second attempt, so retrying
// would only replay the same bug at the cost of another turn.
func TestRetryStreamDoesNotRetryDecodeError(t *testing.T) {
	tr := newRetryTestTransport(4)
	opens := 0
	decodeErr := errors.New("deepseek: decode chunk: unexpected end of JSON input")
	pump := func(ctx context.Context, body io.ReadCloser, events chan<- wire.Event) {
		defer close(events)
		defer body.Close()
		events <- wire.Event{Type: wire.EventError, Err: decodeErr}
	}
	open := func(ctx context.Context) (io.ReadCloser, error) {
		opens++
		return nopBody(), nil
	}

	events := drainStream(t, tr.RetryStream(context.Background(), nopBody(), open, pump, nil))

	if opens != 0 {
		t.Errorf("open ran %d time(s), want 0: a decode error is not a dead connection", opens)
	}
	if len(events) != 1 || !errors.Is(events[0].Err, decodeErr) {
		t.Fatalf("events = %+v, want the decode error forwarded unmodified", events)
	}
}

// TestRetryStreamRetriesIdleTimeoutBeforeOutput pins the other judgement
// call: the idle watchdog firing before any output is the same "connection
// is presumably dead" signal a network-level error is, so it gets the same
// one retry rather than ending the run — but only pre-output; a stall after
// output has already started is the partial-output case, covered by
// TestRetryStreamDoesNotRetryAfterOutput regardless of which error caused it.
func TestRetryStreamRetriesIdleTimeoutBeforeOutput(t *testing.T) {
	tr := newRetryTestTransport(4)
	idleErr := errors.New("test: stream idle timeout")
	calls := 0
	pump := func(ctx context.Context, body io.ReadCloser, events chan<- wire.Event) {
		defer close(events)
		defer body.Close()
		calls++
		if calls == 1 {
			events <- wire.Event{Type: wire.EventError, Err: idleErr}
			return
		}
		events <- wire.Event{Type: wire.EventFinish, FinishReason: "stop"}
	}
	open := func(ctx context.Context) (io.ReadCloser, error) { return nopBody(), nil }

	events := drainStream(t, tr.RetryStream(context.Background(), nopBody(), open, pump, idleErr))

	if calls != 2 {
		t.Errorf("pump ran %d time(s), want 2", calls)
	}
	for _, ev := range events {
		if ev.Type == wire.EventError {
			t.Fatalf("caller saw an error event %v; the idle-timeout retry should have been invisible to it", ev.Err)
		}
	}
}

// TestRetryStreamStopsAtMaxRetries pins that the retry budget is bounded: a
// connection that never recovers is retried MaxRetries times and then fails
// with the underlying error, not an unbounded loop or a ctx.Err() standing
// in for it.
func TestRetryStreamStopsAtMaxRetries(t *testing.T) {
	tr := newRetryTestTransport(2)
	failure := &fakeNetErr{"read: connection reset by peer"}
	pumpCalls, opens := 0, 0
	pump := func(ctx context.Context, body io.ReadCloser, events chan<- wire.Event) {
		defer close(events)
		defer body.Close()
		pumpCalls++
		events <- wire.Event{Type: wire.EventError, Err: failure}
	}
	open := func(ctx context.Context) (io.ReadCloser, error) {
		opens++
		return nopBody(), nil
	}

	events := drainStream(t, tr.RetryStream(context.Background(), nopBody(), open, pump, nil))

	if pumpCalls != 3 {
		t.Errorf("pump ran %d time(s), want 3 (the initial attempt plus MaxRetries=2 retries)", pumpCalls)
	}
	if opens != 2 {
		t.Errorf("open ran %d time(s), want 2", opens)
	}
	if len(events) != 1 || !errors.Is(events[0].Err, failure) {
		t.Fatalf("events = %+v, want the underlying failure forwarded once retries are exhausted", events)
	}
}
