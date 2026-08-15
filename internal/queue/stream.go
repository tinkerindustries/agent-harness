package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream and consumer names, fixed by docs/DESIGN.md §4.10. The names are
// variables, not constants, for one reason only: IsolateForTest renames them
// so a test package never shares a stream or consumer with production or
// with another test package. The production values below are what every
// non-test caller sees.
var (
	StreamWork = "WORK"

	requestSubjectPrefix = "harness.work.request."

	// ConsumerDurable is the WORK stream's durable pull consumer name.
	ConsumerDurable = "harness-workers"
)

// IsolateForTest renames the WORK stream, the durable consumer, and the
// request subject prefix so this process's tests share nothing with
// production or with another test package running against the same broker.
// Each test package calls it from its TestMain with its own prefix ("mcp",
// "worker", "queue"), which is what lets `go test ./internal/mcp/... ./internal/
// worker/...` run in parallel: internal/worker's real pool used to consume
// the requests internal/mcp's tests asserted were sitting unconsumed on the
// one shared WORK stream. It must never be called from production code;
// nothing outside _test.go files does.
func IsolateForTest(prefix string) {
	StreamWork = prefix + "-WORK"
	ConsumerDurable = prefix + "-harness-workers"
	requestSubjectPrefix = prefix + ".harness.work.request."
}

// Ack discipline and retention, fixed by docs/DESIGN.md §4.10.
const (
	// AckWait is fixed at 60s (docs/DESIGN.md §4.10); the pool's InProgress
	// heartbeat interval is sized well under it so a multi-minute run never
	// trips it.
	AckWait = 60 * time.Second

	// DefaultMaxDeliveryAttempts is the delivery ceiling when a caller passes
	// zero. Production resolves worker.max_delivery_attempts from the
	// settings registry and passes it in.
	//
	// A ceiling has to exist. Once a request carries a session id it is
	// single-use and a redelivery fails it rather than re-runs it (§4.10), so
	// the ceiling's job is the requests that die *before* their session
	// exists — the only ones redelivery still claims. One of those that keeps
	// dying during preparation would otherwise be redelivered forever, each
	// attempt burning a pool slot; the ceiling caps that.
	DefaultMaxDeliveryAttempts = 5
)

// RequestSubject is the subject a work request publishes to. The wildcard
// segment in the stream's harness.work.request.* is request_id, which
// gives per-request visibility in monitoring without needing a separate
// index.
func RequestSubject(requestID string) string {
	return requestSubjectPrefix + requestID
}

// Connect dials url and opens a JetStream context on it.
func Connect(url string) (*nats.Conn, jetstream.JetStream, error) {
	nc, err := nats.Connect(url, nats.Name("deepseek-harness"), nats.MaxReconnects(-1))
	if err != nil {
		return nil, nil, fmt.Errorf("queue: connect to %s: %w", url, err)
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, fmt.Errorf("queue: open jetstream: %w", err)
	}
	return nc, js, nil
}

// EnsureStreams declares the WORK stream and its consumer, converging an
// empty server rather than requiring a setup script (docs/DESIGN.md §4.10).
// CreateOrUpdate is idempotent: run again against a server that already has
// matching definitions, it is a no-op; run again with a different poolSize,
// it updates MaxAckPending to match. maxDeliver bounds how many times one
// request may be delivered; zero means DefaultMaxDeliveryAttempts.
func EnsureStreams(ctx context.Context, js jetstream.JetStream, poolSize int, maxDeliver int) (jetstream.Consumer, error) {
	if maxDeliver <= 0 {
		maxDeliver = DefaultMaxDeliveryAttempts
	}
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      StreamWork,
		Subjects:  []string{requestSubjectPrefix + "*"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
	})
	if err != nil {
		return nil, fmt.Errorf("queue: ensure %s stream: %w", StreamWork, err)
	}

	consumer, err := js.CreateOrUpdateConsumer(ctx, StreamWork, jetstream.ConsumerConfig{
		Durable:       ConsumerDurable,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       AckWait,
		MaxAckPending: poolSize,
		MaxDeliver:    maxDeliver,
	})
	if err != nil {
		return nil, fmt.Errorf("queue: ensure %s consumer: %w", ConsumerDurable, err)
	}
	return consumer, nil
}
