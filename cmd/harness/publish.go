package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/mrgeoffrich/deepseek-harness/internal/queue"
)

// runPublish sends one work request to the WORK stream "harness serve"
// reads from. It is an operator and demonstration tool, not the harness's
// only ingress path — a NATS client in any language can publish the same
// JSON body directly (docs/DESIGN.md §4.10).
func runPublish(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	workspace := fs.String("workspace", "", "workspace directory the request names (required)")
	prompt := fs.String("prompt", "", "task for the request; if omitted, the task is the trailing positional argument")
	requestID := fs.String("request-id", "", "idempotency key; a random one is generated if omitted. Pass the same value twice to demonstrate deduplication")
	model := fs.String("model", "", "override model (config default otherwise)")
	effort := fs.String("effort", "", "override reasoning effort")
	permissionMode := fs.String("permission-mode", "", "readonly or full (required)")
	var deny stringList
	fs.Var(&deny, "deny", "deny pattern, matched as a substring; repeatable")
	resultSchemaPath := fs.String("result-schema", "", "path to a JSON Schema file Complete's result must satisfy")
	maxSubTurns := fs.Int("max-sub-turns", 0, "override max sub-turns")
	deadlineMS := fs.Int64("deadline-ms", 0, "override the request's deadline in milliseconds")
	wait := fs.Bool("wait", false, "block until the final result is published, then print it")
	waitTimeout := fs.Duration("wait-timeout", 0, "how long -wait blocks for (default: the request's own deadline, or the config default deadline)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *workspace == "" {
		return errors.New("usage: harness publish -workspace P [flags] \"task\"")
	}
	task := *prompt
	if task == "" {
		if fs.NArg() < 1 {
			return errors.New("usage: harness publish -workspace P [flags] \"task\"")
		}
		task = strings.Join(fs.Args(), " ")
	}

	if *permissionMode == "" {
		return errors.New("-permission-mode is required: readonly or full")
	}

	id := *requestID
	if id == "" {
		id = randomRequestID()
	}

	var resultSchema json.RawMessage
	if *resultSchemaPath != "" {
		b, err := os.ReadFile(*resultSchemaPath)
		if err != nil {
			return fmt.Errorf("read result schema: %w", err)
		}
		resultSchema = b
	}

	req := queue.Request{
		RequestID:      id,
		Prompt:         task,
		Workspace:      *workspace,
		Model:          *model,
		Effort:         *effort,
		PermissionMode: *permissionMode,
		Deny:           deny,
		ResultSchema:   resultSchema,
		MaxSubTurns:    *maxSubTurns,
		DeadlineMS:     *deadlineMS,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	nc, js, err := queue.Connect(cfg.NATSURL)
	if err != nil {
		return err
	}
	defer nc.Close()

	var waitConsumer jetstream.Consumer
	if *wait {
		// Set up the result subscription before publishing, so a fast
		// response cannot land before this is listening.
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		waitConsumer, err = js.OrderedConsumer(waitCtx, queue.StreamResults, jetstream.OrderedConsumerConfig{
			FilterSubjects: []string{queue.FinalSubject(id)},
		})
		cancel()
		if err != nil {
			return fmt.Errorf("subscribe for result: %w", err)
		}
	}

	pubCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = js.Publish(pubCtx, queue.RequestSubject(id), data)
	cancel()
	if err != nil {
		return fmt.Errorf("publish request: %w", err)
	}
	fmt.Printf("published request_id %s\n", id)

	if !*wait {
		return nil
	}

	timeout := *waitTimeout
	if timeout <= 0 {
		timeout = time.Duration(cfg.DefaultDeadlineMS) * time.Millisecond
		if *deadlineMS > 0 {
			timeout = time.Duration(*deadlineMS) * time.Millisecond
		}
	}
	fmt.Printf("waiting up to %s for the final result...\n", timeout)
	batch, err := waitConsumer.Fetch(1, jetstream.FetchMaxWait(timeout))
	if err != nil {
		return fmt.Errorf("fetch result: %w", err)
	}
	for msg := range batch.Messages() {
		var res queue.Result
		if err := json.Unmarshal(msg.Data(), &res); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	if err := batch.Error(); err != nil {
		return fmt.Errorf("no result arrived: %w", err)
	}
	return errors.New("no result arrived before the wait timed out")
}

func randomRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("publish: crypto/rand unavailable: " + err.Error())
	}
	return "req-" + hex.EncodeToString(b[:])
}
