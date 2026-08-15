package queue

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/config"
)

// testNATSURL is the broker in docker-compose.test.yml, started by
// scripts/test.sh. It deliberately ignores NATS_URL: that names the
// deployment's broker, and these tests delete the streams they run
// against. The repo's own .env is loaded (best effort, real env vars still
// win) so a port override there reaches the suite.
func testNATSURL() string {
	config.LoadDotEnv("../../.env")
	if v := os.Getenv("HARNESS_TEST_NATS_URL"); v != "" {
		return v
	}
	return "nats://127.0.0.1:4422"
}

// connectOrSkip connects to the test broker. A missing broker is a failure,
// not a skip, by default: a suite that could not run must not read as a pass
// in `go test ./...` output. The deliberate opt-out is
// HARNESS_TEST_NATS_OPTIONAL=1, for a developer who genuinely has no Docker;
// scripts/test.sh never sets it, so its own broker being unreachable is loud.
func connectOrSkip(t *testing.T) (*nats.Conn, jetstream.JetStream) {
	t.Helper()
	nc, js, err := Connect(testNATSURL())
	if err != nil {
		if os.Getenv("HARNESS_TEST_NATS_OPTIONAL") == "1" {
			t.Skipf("no test NATS JetStream server reachable at %s (scripts/test.sh): %v", testNATSURL(), err)
		}
		t.Fatalf("no test NATS JetStream server reachable at %s: %v — run scripts/test.sh to start one, or set HARNESS_TEST_NATS_OPTIONAL=1 to skip instead of failing", testNATSURL(), err)
	}
	t.Cleanup(nc.Close)
	return nc, js
}

// TestEnsureStreamsConverges declares the WORK stream and consumer against a
// real server twice with different pool sizes, proving an empty server
// converges and a second call updates rather than erroring
// (docs/DESIGN.md §4.10: "treats an existing definition as satisfied").
func TestEnsureStreamsConverges(t *testing.T) {
	_, js := connectOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Cleanup(func() {
		js.DeleteStream(context.Background(), StreamWork)
	})

	if _, err := EnsureStreams(ctx, js, 4, DefaultMaxDeliveryAttempts); err != nil {
		t.Fatalf("first EnsureStreams: %v", err)
	}
	consumer, err := EnsureStreams(ctx, js, 8, DefaultMaxDeliveryAttempts)
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
}

// TestPublishRequestLandsOnWorkStream pins the one publish path every
// producer shares — harness publish, deepseek_agent, and the browser's POST
// /api/runs (docs/RUN-CONTROL.md "Starting is a publish"): the request is
// marshalled and lands on the WORK stream under the request's own subject,
// byte-identical to what the worker parses. A producer that published a
// different shape than ParseRequest accepts would fail here.
func TestPublishRequestLandsOnWorkStream(t *testing.T) {
	_, js := connectOrSkip(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	consumer, err := EnsureStreams(ctx, js, 4, DefaultMaxDeliveryAttempts)
	if err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	t.Cleanup(func() {
		js.DeleteStream(context.Background(), StreamWork)
	})

	req := Request{
		RequestID:      "publish-helper-test",
		Prompt:         "do the thing",
		Repos:          []Repo{{URL: "https://github.com/org/app.git", Branch: "dev"}},
		PermissionMode: "readonly",
		Model:          "deepseek-v4-pro",
		Effort:         "high",
	}
	if err := PublishRequest(ctx, js, req); err != nil {
		t.Fatalf("PublishRequest: %v", err)
	}

	// The WORK stream is a work queue, so an OrderedConsumer will not work on
	// it (JetStream refuses a pull consumer without an ack policy) — read back
	// through the durable consumer EnsureStreams declared, which is the same
	// path the worker pool reads.
	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(3*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	count := 0
	for msg := range batch.Messages() {
		count++
		got, err := ParseRequest(msg.Data())
		if err != nil {
			t.Fatalf("parse published request: %v", err)
		}
		if !reflect.DeepEqual(got, req) {
			t.Fatalf("published request = %+v, want %+v", got, req)
		}
		msg.Ack()
	}
	if count != 1 {
		t.Fatalf("expected 1 message on the WORK stream, got %d", count)
	}
}

// TestEnsureStreamsBoundsRedelivery pins the ceiling on the consumer itself.
// JetStream's default is unlimited redelivery, which means a request whose
// worker dies every time is redelivered forever, each attempt burning a pool
// slot — and killing the stuck run is what triggers the next attempt rather
// than ending it (docs/DESIGN.md §4.10).
func TestEnsureStreamsBoundsRedelivery(t *testing.T) {
	nc, js := connectOrSkip(t)
	defer nc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	t.Cleanup(func() {
		js.DeleteStream(context.Background(), StreamWork)
	})

	consumer, err := EnsureStreams(ctx, js, 4, 3)
	if err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	info, err := consumer.Info(ctx)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if info.Config.MaxDeliver != 3 {
		t.Fatalf("MaxDeliver = %d, want 3 — unlimited redelivery is the bug this bounds", info.Config.MaxDeliver)
	}

	// Zero means the built-in default rather than JetStream's unlimited.
	consumer, err = EnsureStreams(ctx, js, 4, 0)
	if err != nil {
		t.Fatalf("EnsureStreams with zero: %v", err)
	}
	info, err = consumer.Info(ctx)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if info.Config.MaxDeliver != DefaultMaxDeliveryAttempts {
		t.Fatalf("MaxDeliver = %d, want the default %d", info.Config.MaxDeliver, DefaultMaxDeliveryAttempts)
	}
}
