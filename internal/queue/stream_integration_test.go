package queue

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
)

// testNATSURL is the local docker-compose JetStream server
// (docker-compose.yml, .env.example). Tests here are skipped when nothing
// answers on it, so `go test ./...` passes without Docker. The repo's own
// .env is loaded (best effort, real env vars still win) because a machine
// may already run NATS on the default port for an unrelated project
// (docker-compose.yml's own comment); a developer who has overridden the
// port there expects the test suite to honour it too.
func testNATSURL() string {
	config.LoadDotEnv("../../.env")
	if v := os.Getenv("NATS_URL"); v != "" {
		return v
	}
	return "nats://127.0.0.1:4222"
}

func connectOrSkip(t *testing.T) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	nc, js, err := Connect(testNATSURL())
	if err != nil {
		t.Skipf("no local NATS JetStream server reachable at %s (docker compose up -d): %v", testNATSURL(), err)
	}
	t.Cleanup(nc.Close)
	return nc, js
}

// TestEnsureStreamsConverges declares the streams and consumer against a
// real server twice with different pool sizes, proving an empty server
// converges and a second call updates rather than erroring
// (docs/DESIGN.md §4.10, PLAN.md: "treats an existing definition as
// satisfied").
func TestEnsureStreamsConverges(t *testing.T) {
	_, js := connectOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Cleanup(func() {
		js.DeleteStream(context.Background(), StreamWork)
		js.DeleteStream(context.Background(), StreamResults)
	})

	if _, err := EnsureStreams(ctx, js, 4); err != nil {
		t.Fatalf("first EnsureStreams: %v", err)
	}
	consumer, err := EnsureStreams(ctx, js, 8)
	if err != nil {
		t.Fatalf("second EnsureStreams: %v", err)
	}

	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if info.Config.MaxAckPending != 8 {
		t.Fatalf("expected MaxAckPending updated to 8, got %d", info.Config.MaxAckPending)
	}
	if info.Config.AckWait != AckWait {
		t.Fatalf("expected AckWait %s, got %s", AckWait, info.Config.AckWait)
	}

	workInfo, err := js.Stream(ctx, StreamWork)
	if err != nil {
		t.Fatalf("work stream info: %v", err)
	}
	if workInfo.CachedInfo().Config.Retention != jetstream.WorkQueuePolicy {
		t.Fatalf("expected WORK stream to use WorkQueuePolicy retention")
	}

	resultsInfo, err := js.Stream(ctx, StreamResults)
	if err != nil {
		t.Fatalf("results stream info: %v", err)
	}
	if resultsInfo.CachedInfo().Config.MaxAge != resultsMaxAge {
		t.Fatalf("expected RESULTS MaxAge %s, got %s", resultsMaxAge, resultsInfo.CachedInfo().Config.MaxAge)
	}
}

// TestNatsMsgIDDeduplicates is the measurement PLAN.md's report section
// calls out as already verified against the local server: two publishes
// sharing a Nats-Msg-Id store one message, not two.
func TestNatsMsgIDDeduplicates(t *testing.T) {
	_, js := connectOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := EnsureStreams(ctx, js, 4); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	t.Cleanup(func() {
		js.DeleteStream(context.Background(), StreamWork)
		js.DeleteStream(context.Background(), StreamResults)
	})

	requestID := "dedup-test-" + time.Now().Format("150405.000000")
	subject := FinalSubject(requestID)
	msgID := FinalMsgID(requestID)

	for i := 0; i < 2; i++ {
		if _, err := js.Publish(ctx, subject, []byte("payload"), jetstream.WithMsgID(msgID)); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	// A third publish under a different Nats-Msg-Id must still land, so the
	// test proves dedup is keyed on the header rather than the subject.
	if _, err := js.Publish(ctx, subject, []byte("payload"), jetstream.WithMsgID(requestID+".final.other")); err != nil {
		t.Fatalf("publish 3: %v", err)
	}

	info, err := js.Stream(ctx, StreamResults)
	if err != nil {
		t.Fatalf("stream info: %v", err)
	}
	consumer, err := js.OrderedConsumer(ctx, StreamResults, jetstream.OrderedConsumerConfig{FilterSubjects: []string{subject}})
	if err != nil {
		t.Fatalf("ordered consumer: %v", err)
	}
	batch, err := consumer.Fetch(3, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	count := 0
	for range batch.Messages() {
		count++
	}
	if count != 2 {
		t.Fatalf("expected 2 stored messages (one deduplicated pair plus one distinct), got %d", count)
	}
	_ = info
}
