package queue

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Stream and subject names, and the consumer's ack discipline, fixed by
// docs/DESIGN.md §4.10. The names are variables, not constants, for one
// reason only: IsolateForTest renames them so a test package never shares
// a stream or consumer with production or with another test package. The
// production values below are what every non-test caller sees.
var (
	StreamWork    = "WORK"
	StreamResults = "RESULTS"

	requestSubjectPrefix = "harness.work.request."
	resultSubjectPrefix  = "harness.work.result."

	// ConsumerDurable is the WORK stream's durable pull consumer name.
	ConsumerDurable = "harness-workers"
)

// IsolateForTest renames the streams, the durable consumer, and the
// subject prefixes so this process's tests share nothing with production
// or with another test package running against the same broker. Each test
// package calls it from its TestMain with its own prefix ("mcp", "worker",
// "queue"), which is what lets `go test ./internal/mcp/... ./internal/
// worker/...` run in parallel: internal/worker's real pool used to consume
// the requests internal/mcp's tests asserted were sitting unconsumed on the
// one shared WORK stream. It must never be called from production code;
// nothing outside _test.go files does.
func IsolateForTest(prefix string) {
	StreamWork = prefix + "-WORK"
	StreamResults = prefix + "-RESULTS"
	ConsumerDurable = prefix + "-harness-workers"
	requestSubjectPrefix = prefix + ".harness.work.request."
	resultSubjectPrefix = prefix + ".harness.work.result."
}

// Ack discipline and retention, fixed by docs/DESIGN.md §4.10.
const (
	// AckWait is fixed at 60s (docs/DESIGN.md §4.10); the pool's InProgress
	// heartbeat interval is sized well under it so a multi-minute run never
	// trips it.
	AckWait = 60 * time.Second

	// resultsMaxAge is the RESULTS stream's retention window.
	resultsMaxAge = 7 * 24 * time.Hour

	// resultsDuplicateWindow is longer than the 2-minute
	// JetStream default. A final result republished after a crash between
	// the DB write and the original publish (docs/DESIGN.md §4.10) can
	// arrive well after AckWait plus redelivery latency; a longer window
	// keeps Nats-Msg-Id dedup covering that gap.
	resultsDuplicateWindow = 10 * time.Minute
)

// RequestSubject is the subject a work request publishes to. The wildcard
// segment in the stream's harness.work.request.* is request_id, which
// gives per-request visibility in monitoring without needing a separate
// index.
func RequestSubject(requestID string) string {
	return requestSubjectPrefix + requestID
}

// AcceptedSubject, ProgressSubject, and FinalSubject are the three
// per-request result subjects under harness.work.result.>
// (docs/DESIGN.md §4.10).
func AcceptedSubject(requestID string) string { return resultSubjectPrefix + requestID + ".accepted" }
func ProgressSubject(requestID string) string { return resultSubjectPrefix + requestID + ".progress" }
func FinalSubject(requestID string) string    { return resultSubjectPrefix + requestID + ".final" }

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

// EnsureStreams declares the WORK and RESULTS streams and the WORK
// consumer, converging an empty server rather than requiring a setup
// script (docs/DESIGN.md §4.10). CreateOrUpdate is
// idempotent: run again against a server that already has matching
// definitions, it is a no-op; run again with a different poolSize, it
// updates MaxAckPending to match.
func EnsureStreams(ctx context.Context, js jetstream.JetStream, poolSize int) (jetstream.Consumer, error) {
	_, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:      StreamWork,
		Subjects:  []string{requestSubjectPrefix + "*"},
		Retention: jetstream.WorkQueuePolicy,
		Storage:   jetstream.FileStorage,
	})
	if err != nil {
		return nil, fmt.Errorf("queue: ensure %s stream: %w", StreamWork, err)
	}

	_, err = js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
		Name:       StreamResults,
		Subjects:   []string{resultSubjectPrefix + ">"},
		Retention:  jetstream.LimitsPolicy,
		MaxAge:     resultsMaxAge,
		Storage:    jetstream.FileStorage,
		Duplicates: resultsDuplicateWindow,
	})
	if err != nil {
		return nil, fmt.Errorf("queue: ensure %s stream: %w", StreamResults, err)
	}

	consumer, err := js.CreateOrUpdateConsumer(ctx, StreamWork, jetstream.ConsumerConfig{
		Durable:       ConsumerDurable,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       AckWait,
		MaxAckPending: poolSize,
	})
	if err != nil {
		return nil, fmt.Errorf("queue: ensure %s consumer: %w", ConsumerDurable, err)
	}
	return consumer, nil
}
