package queue

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// natsSink is the phase-2 implementation of ResultSink: it publishes to the
// RESULTS stream exactly as the worker pool did before the seam existed.
// The plan (docs/QUEUE-MIGRATION-PLAN.md §1.6) deletes the RESULTS stream
// outright in phase 3, and this sink with it.
type natsSink struct {
	js jetstream.JetStream
}

// NewNATSSink returns a ResultSink that publishes to the RESULTS stream
// through js, under the subjects and message ids the pool used directly
// before this seam existed.
func NewNATSSink(js jetstream.JetStream) ResultSink {
	return natsSink{js: js}
}

// Final publishes data to harness.work.result.<request_id>.final under the
// deduplicating Nats-Msg-Id (FinalMsgID), bounded by a 10-second timeout so
// a stalled broker cannot wedge the worker's finish path — the pool's
// Nak-and-retry on a publish error depends on the call returning.
func (s natsSink) Final(ctx context.Context, requestID string, data []byte) error {
	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	_, err := s.js.Publish(pubCtx, FinalSubject(requestID), data, jetstream.WithMsgID(FinalMsgID(requestID)))
	return err
}

// Accepted marshals a and publishes it to
// harness.work.result.<request_id>.accepted, bounded by a 5-second timeout.
func (s natsSink) Accepted(ctx context.Context, a Accepted) error {
	data, err := json.Marshal(a)
	if err != nil {
		return err
	}
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = s.js.Publish(pubCtx, AcceptedSubject(a.RequestID), data)
	return err
}

// Progress marshals p and publishes it to
// harness.work.result.<request_id>.progress, bounded by a 5-second timeout.
func (s natsSink) Progress(ctx context.Context, p Progress) error {
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, err = s.js.Publish(pubCtx, ProgressSubject(p.RequestID), data)
	return err
}
