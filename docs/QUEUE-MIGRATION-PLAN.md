# Removing NATS JetStream

Plan against HEAD `775e772`. The WORK stream becomes a table in
`internal/store`; the RESULTS stream is deleted entirely.

## As executed

This document is the record of the change, kept the way
`docs/RUN-CONTROL-PLAN.md` records its own; where the code differs from the
plan, the code is the authority. Four divergences are worth recording.

- **The `ResultSink` interface shipped with a third method.** §1.6 sketched
  two methods, `Final` and `Accepted`; what landed had three — `Final(ctx,
  requestID string, data []byte)`, `Accepted`, and `Progress` — because the
  call sites needed the progress path while the sink still existed. It was
  deleted exactly as §1.6 predicted once `finish` wrote the row and `republish`
  became a bare ack.
- **The `UPDATE ... RETURNING` claim never needed the SELECT-then-UPDATE
  fallback.** §1.3 allowed for `modernc.org/sqlite` not supporting
  `RETURNING`; it was verified to work on the first try, so the fallback was
  never written.
- **The stop-during-preparation gap (§4.10's "a stop during a clone marks the
  creating row cancelled") was found by phase 5, not part of the original
  plan.** `TestStopHealthyRunCancels` failed about half the time because a
  fast store-backed claim let a stop land mid-preparation, where the old code
  reported `workspace_setup` regardless of cause. Phase 5 settled the suite
  by waiting for `running`; phase 6 fixed the code — a stopped preparation
  now reports `cancelled` with the operator's reason and marks the `creating`
  row — and restored the coverage with
  `TestStopDuringPreparationSoftStopCancelsRowAndResult`.
- **One more divergence the §4 table predicted optimistically:** "the guarded
  UPDATE and the queue-row DELETE **in one submit transaction**". The two
  writes are separate store submits. The crash window is closed all the same:
  the result is the row, and a crash between the writes leaves a terminal row
  that the redelivered request reads back and acks without running anything.

---

## 0. What the code actually says

Six facts, each verified, that make the change smaller than it looks, and one
that makes it larger.

**The pool's `jetstream.Msg` surface is exactly six methods.** Every `msg.` in
`internal/worker/pool.go` and `control.go` is one of: `Data()`, `Metadata()`
(read solely for `NumDelivered`, in `deliveryCount`), `Ack()`, `Term()`,
`NakWithDelay(d)`, `InProgress()`. It never calls bare `Nak`, `DoubleAck`,
`TermWithReason`, `Headers`, `Subject`, or `Reply` — the test's `fakeMsg`
implements those only to satisfy the interface. So `queue.Msg` can be
*narrower* than first sketched: `Nak(delay)` replaces `NakWithDelay`, and
`DeliveryCount() uint64` replaces `Metadata() (*MsgMetadata, error)`.

**The `progress` subject has zero consumers.** `queue.ProgressSubject` is
referenced only by its own definition and by `Pool.publishProgress`. Nothing
subscribes. The hub already carries strictly more (`PublishEvents`,
`PublishLive`, `PublishSessionState` on every sub-turn). So `progress` is
**deleted, not migrated to the hub** — the hub already has it.

**`accepted` has one consumer and the row already answers it.**
`internal/mcp/launch.go` subscribes to `AcceptedSubject` before publishing and
fetches for `Cfg.AcceptedWaitMS` to learn "did a worker take this, and what
session id". `Pool.run` calls `Store.SetWorkRequestSession` immediately
*before* `publishAccepted`, so a `work_requests` row with a non-empty
`session_id` is the same signal, durably, and is already served by
`GET /api/requests/{request_id}`. **Do not add a request-keyed hub channel** —
a bounded poll of that endpoint is simpler and survives the MCP process not
being connected.

**`internal/evals` already proves the pattern.** `waitForRequest` in
`internal/evals/run.go` polls `store.GetWorkRequest` until `FinishedAt != nil`
and never touches RESULTS.

**`GET /api/control-token` is served unauthenticated to a local caller.**
`handleGetControlToken` in `internal/httpapi/auth.go` gates on
`isLocalCallerAddr` only, and `internal/mcp/control.go` already fetches it that
way. That is what lets `harness publish` become an HTTP client with no new auth
surface.

**The larger-than-it-looks part: `harness publish` runs in a different process
from `harness serve`.** Today a shared broker is what makes a second process a
producer. Afterwards the queue lives in serve's SQLite file and a second
process cannot nudge serve's pool. `harness publish` must become an HTTP
client. This is the concrete form of the §4.10 "any NATS client can publish"
loss.

`internal/httpapi` already imports `internal/queue` (for `Request` and
`Validate`), so a `queue.Stats` type behind the health seam adds no new edge.
`internal/httpapi/boundary_test.go` forbids only `internal/session` and
`internal/worker`; nothing here touches that.

---

## 1. Target design

### 1.1 `queue.Msg`

New file `internal/queue/msg.go`:

```go
// Msg is one claimed unit of work and the four ways of disposing of it.
type Msg interface {
	Data() []byte
	DeliveryCount() uint64          // 1 on the first delivery
	Ack() error                     // done; the row goes away
	Nak(delay time.Duration) error  // try again after delay
	Term() error                    // never again; the row goes away
	InProgress() error              // extend the lease
}
```

`DeliveryCount()` returns a plain `uint64` rather than `(Metadata, error)`
because the SQLite implementation reads it off the row it just claimed and
cannot fail. The NATS adapter (phase 1 only) folds today's `deliveryCount(msg)`
helper — `Metadata()` error ⇒ 1 — into itself, and `worker.deliveryCount` is
deleted.

### 1.2 The `work_queue` table

```sql
CREATE TABLE IF NOT EXISTS work_queue (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    request_id       TEXT    NOT NULL,
    payload          TEXT    NOT NULL,
    enqueued_at      TEXT    NOT NULL,
    visible_at_ms    INTEGER NOT NULL,
    lease_expires_ms INTEGER NOT NULL DEFAULT 0,
    delivery_count   INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_work_queue_claimable
    ON work_queue (visible_at_ms, id);
CREATE INDEX IF NOT EXISTS idx_work_queue_request_id
    ON work_queue (request_id);
```

Two decisions need defending, because getting either wrong breaks §4.10
quietly.

**The primary key is a surrogate `id`, not `request_id`.** JetStream's WORK
stream never deduplicated requests — it deduplicated *results*
(`Nats-Msg-Id`). Two publishes of the same `request_id` are two messages today,
and that is load-bearing: `store.shouldClaim`'s comment says "numDelivered from
a second, independently published message for the same request_id starts back
at 1, so a genuine race between two live attempts never satisfies this and
falls through to 'still owned elsewhere' instead of running twice". A
`request_id` primary key with `INSERT OR IGNORE` would collapse the second
enqueue into the first and destroy the `RefusalOwnedElsewhere` and
`RefusalSpent` paths. One enqueue = one row = one independent delivery counter
is the exact mapping.

**Time columns are integer Unix milliseconds, not RFC3339Nano text.** Every
other table stores `time.RFC3339Nano`, which *trims trailing zeros* on the
fraction: `"…:00Z"` versus `"…:00.5Z"` compares as `'.'(0x2E) < 'Z'(0x5A)`, so
the half-second timestamp sorts *before* the whole-second one. Elsewhere that
is a cosmetic ordering wart; here it is the claim predicate, so it would be a
correctness bug. `enqueued_at` stays RFC3339Nano TEXT because it is only ever
displayed. Put a comment in `internal/store/schema.go` saying exactly this so
nobody "tidies" it back.

`lease_expires_ms = 0` means unleased. A single integer comparison
`lease_expires_ms <= now_ms` covers both "never leased" and "lease expired".

### 1.3 Store methods

New file `internal/store/work_queue.go`. All take an explicit `now time.Time`,
matching `ClaimWorkRequest`, `CloseWorkRequest`, `DeleteWorkspaceLease`. All
writes go through `s.submit`, i.e. the single writer goroutine, so the DESIGN
§4.5 invariant is untouched.

```go
type QueuedWork struct {
	ID            int64
	RequestID     string
	Payload       []byte
	DeliveryCount uint64
}

func (s *Store) EnqueueWork(ctx, requestID string, payload []byte, now time.Time) error
func (s *Store) ClaimWork(ctx, limit int, lease time.Duration, maxDeliveries int, now time.Time) ([]QueuedWork, error)
func (s *Store) HeartbeatWork(ctx, id int64, lease time.Duration, now time.Time) error
func (s *Store) AckWork(ctx, id int64) error                                   // DELETE
func (s *Store) NakWork(ctx, id int64, delay time.Duration, maxDeliveries int, now time.Time) error
func (s *Store) WorkQueueClaimable(ctx, now time.Time) (bool, error)           // read-only gate
func (s *Store) WorkQueueStats(ctx, now time.Time) (WorkQueueStats, error)
```

`ClaimWork`, inside one transaction:

```sql
UPDATE work_queue
   SET lease_expires_ms = :now + :lease,
       delivery_count   = delivery_count + 1
 WHERE id IN (SELECT id FROM work_queue
               WHERE visible_at_ms    <= :now
                 AND lease_expires_ms <= :now
                 AND delivery_count   <  :max
               ORDER BY visible_at_ms ASC, id ASC
               LIMIT :limit)
RETURNING id, request_id, payload, delivery_count;
```

`RETURNING` needs SQLite ≥ 3.35; `modernc.org/sqlite` is well past that, but
**verify with a one-line test before building on it**. The fallback is a
`SELECT ... LIMIT n` followed by an `UPDATE ... WHERE id IN (...)` in the
*same* transaction — atomic for the reason the claim is atomic today: it runs
on the store's one writer goroutine, so there is no second goroutine to race
with (`ClaimWorkRequest`'s doc comment already makes this argument).

`NakWork` carries the delivery ceiling, because this is where the ceiling has
to live:

```sql
-- delivery_count was already incremented at claim time.
DELETE FROM work_queue WHERE id = :id AND delivery_count >= :max;
UPDATE work_queue
   SET visible_at_ms = :now + :delay, lease_expires_ms = 0
 WHERE id = :id;
```

That is JetStream's exact behaviour: at `MaxDeliver` the message stops being
redelivered and, on a WorkQueue-retention stream, is discarded. The
`delivery_count < :max` clause in `ClaimWork` is belt-and-braces.

`AckWork` and `Term` are both `DELETE FROM work_queue WHERE id = ?`. They stay
separate methods on `queue.Msg` for readability and because a future
dead-letter table would differ — but see §11; no dead letter is being built.

### 1.4 `queue.Queue` — the seam the pool and the producers share

New file `internal/queue/sqlite.go`. This is where the wake-up channel lives,
because the store must not grow a notification concern.

```go
type Queue struct {
	Store         *store.Store
	Lease         time.Duration // queue.LeaseDuration, 60s (was AckWait)
	MaxDeliveries int           // worker.max_delivery_attempts
	PollInterval  time.Duration // 1s backstop
	wake          chan struct{} // buffered 1
}

func (q *Queue) Enqueue(ctx context.Context, req Request) error  // marshal + EnqueueWork + nudge
func (q *Queue) Claim(ctx context.Context, limit int) ([]Msg, error)
func (q *Queue) Wake()                                            // non-blocking send
func (q *Queue) Wait(ctx context.Context)                         // select on wake / ticker / ctx
func (q *Queue) Stats(ctx context.Context) (Stats, error)
```

`Enqueue` replaces `queue.PublishRequest(ctx, js, req)` and keeps the same job:
it is *the* one marshal-and-enqueue path for all four producers
(`POST /api/runs` via `publishAdapter`, `deepseek_agent`, the evals
orchestrator, and `harness publish` indirectly through `POST /api/runs`), so
the wire shape stays defined once.

`queue.sqliteMsg` implements `Msg` over `(q, QueuedWork)`;
`Ack`/`Nak`/`Term`/`InProgress` call the store methods with
`time.Now().UTC()`.

### 1.5 Flow control and wake-ups in the pool

`Pool.Run` loses `Consumer.Consume` and becomes an explicit claim loop:

```go
func (p *Pool) Run(ctx context.Context) error {
	sem := make(chan struct{}, p.size())
	for {
		if ctx.Err() != nil || p.halted.Load() { break }
		free := p.size() - len(sem)              // MaxAckPending = pool size
		if free > 0 {
			msgs, err := p.Source.Claim(ctx, free)
			...
			for _, m := range msgs {
				sem <- struct{}{}
				p.wg.Add(1)
				go func(m queue.Msg) {
					defer p.wg.Done()
					release := sync.OnceFunc(func() { <-sem; p.Source.Wake() })
					defer release()
					p.handle(m, release)
				}(m)
			}
		}
		p.Source.Wait(ctx)                        // wake nudge, or the 1s ticker
	}
	p.wg.Wait()
	return nil
}
```

Three details that matter:

- **`release` also calls `Wake()`.** Without it, a saturated pool draining a
  backlog would claim one job per ticker interval. This is the non-obvious half
  of the wake-up design and is easy to omit.
- **`Halt` no longer needs `stopPull`/`haltMu`.** `Pool.Halt` currently stashes
  `consumeCtx.Stop`; with the loop above, the `p.halted.Load()` check at the top
  is the whole mechanism. Delete `haltMu`, `stopPull`, and the
  `p.haltMu.Lock()` block in `Run`. `Halted()` is unchanged (the health
  endpoint reads it).
- **Claiming is gated by a read.** `Queue.Claim` calls
  `store.WorkQueueClaimable` on the read pool first and only takes the writer
  goroutine when something is actually claimable. Otherwise an idle harness runs
  one write transaction per second forever, growing the WAL for nothing.

`p.size()` remains the local backstop and is now also the flow controller — the
comment on `Pool.Size` ("It must equal the consumer's MaxAckPending … Size is
the local backstop, not the flow controller") inverts and must be rewritten.

### 1.6 Results

`Pool.JS` goes away. In its place, a narrow sink declared in `internal/queue`:

```go
type ResultSink interface {
	Final(ctx context.Context, requestID, sessionID string, result Result, raw json.RawMessage) error
	Accepted(ctx context.Context, requestID, sessionID string, at time.Time) error
}
```

Phase 2 implements it over JetStream (behaviour-identical). Phase 3 replaces
the implementation with one that does nothing durable at all — because
`Pool.finish` already writes the result to `work_requests` via
`FinishWorkRequest` before publishing, and `Pool.republish` already reads it
back out. After phase 3 the sink shrinks to nothing and is deleted; `finish`
writes the row and acks, and `republish` becomes a bare `msg.Ack()`.

`publishAccepted`, `publishProgress`, `queue.Accepted`, `queue.Progress`,
`queue.ProgressLimiter`, `queue.FinalMsgID`, `queue.FinalSubject`,
`queue.AcceptedSubject`, `queue.ProgressSubject`, `queue.RequestSubject`,
`queue.StreamWork`, `queue.StreamResults`, `queue.ConsumerDurable`,
`queue.AckWait`, `queue.DefaultResultsMaxAge`, `resultsDuplicateWindow`,
`queue.Connect`, `queue.EnsureStreams`, `queue.IsolateForTest` — all deleted by
the end. `queue.DefaultMaxDeliveryAttempts` survives, now consumed by
`Queue.MaxDeliveries`.

`runOpts.Progress` in `Pool.run` stops being set. **Do not remove
`session.RunOptions.Progress`** — `cmd/harness/run.go` and
`cmd/harness/session.go` both set it for CLI console output.

---

## 2. Phasing

Six phases. Each builds, each passes the full suite at its boundary, each is
independently mergeable and revertable. The ordering deliberately kills RESULTS
*before* swapping WORK, so that even if the work stalls halfway the deployment
is left with one stream instead of two and a strictly simpler MCP package.

### Phase 1 — the `queue.Msg` seam, NATS still behind it

*New:* `internal/queue/msg.go` (the interface), `internal/queue/natsmsg.go`
(`natsMsg` adapter + `WrapNATS`).

*Changed:* `internal/worker/pool.go` — every `jetstream.Msg` parameter becomes
`queue.Msg`; `NakWithDelay(d)` → `Nak(d)`; the `deliveryCount(msg)` helper is
deleted and its call sites use `msg.DeliveryCount()`.
`internal/worker/control.go` — `inflight.msg` becomes `queue.Msg`, drops the
`jetstream` import. `Pool.Run` wraps:
`p.Consumer.Consume(func(m jetstream.Msg) { ... p.handle(queue.WrapNATS(m), release) })`.

*Tests:* `fakeMsg` in `internal/worker/pool_test.go` sheds six methods and
implements `queue.Msg`.

*Exit criteria:* zero behaviour change; `internal/worker` imports `jetstream`
only for `Pool.JS`/`Pool.Consumer`.

### Phase 2 — the `ResultSink` seam, NATS still behind it

*New:* `ResultSink` in `internal/queue/result.go`; a `natsSink` implementation
beside `natsmsg.go`.

*Changed:* `Pool.JS jetstream.JetStream` → `Pool.Results queue.ResultSink`.
`finish`, `republish`, `publishAccepted`, `publishProgress` call the sink.
`cmd/harness/serve.go` constructs the sink from `js`.

*Exit criteria:* `internal/worker` imports `jetstream` only for
`Pool.Consumer`. Behaviour identical (`Nats-Msg-Id` still set, inside the
sink).

### Phase 3 — delete RESULTS

*Changed:*

- `internal/mcp/collect.go` — `handleCollect` becomes
  `GET /api/requests/{request_id}` (see §5).
- `internal/mcp/launch.go` — the accepted subscription/fetch becomes a bounded
  poll of the same endpoint.
- `internal/mcp/service.go` — the `JS` field is replaced by a one-method
  `Publisher` seam (`Publish(ctx, queue.Request) error`), implemented in
  `cmd/harness/serve.go`. The package doc's "Backed by the WORK and RESULTS
  streams" is rewritten.
- `cmd/harness/publish.go` — `-wait` becomes an HTTP poll (the publish itself
  is still NATS at this phase).
- `internal/queue/stream.go` — the RESULTS `CreateOrUpdateStream` call, the
  three result subjects, `FinalMsgID`, `resultsDuplicateWindow`,
  `DefaultResultsMaxAge` deleted; `EnsureStreams` loses its `resultsMaxAge`
  parameter.
- `internal/queue/result.go` — `Accepted`, `Progress` deleted; `Result` kept
  (it is the `work_requests.result` payload shape).
- `internal/queue/progress.go` and `progress_test.go` — deleted.
- `internal/settings/registry.go` — `KeyQueueResultsMaxAge` and its descriptor
  removed; `registry_test.go` updated. `cmd/harness/serve.go` drops the
  `resultsMaxAge` resolution.
- `Pool.Results` collapses: `finish` writes the row and acks; `republish`
  becomes `msg.Ack()`; `publishAccepted`/`publishProgress` deleted; the
  `ResultSink` interface deleted with them.

*Exit criteria:* nothing references `harness.work.result.*`. `internal/mcp`
imports no NATS at all. One JetStream stream remains.

### Phase 4 — the SQLite queue, built and tested, unwired

*New:* the `work_queue` DDL in `internal/store/schema.go`;
`internal/store/work_queue.go`; `internal/store/work_queue_test.go`;
`internal/queue/sqlite.go` (`Queue`, `sqliteMsg`, `Stats`);
`internal/queue/sqlite_test.go`.

*Exit criteria:* the new code compiles, is fully tested, and nothing in
production calls it. A pure-addition, low-risk merge.

### Phase 5 — swap WORK

*Changed:*

- `Pool.Consumer jetstream.Consumer` → `Pool.Source queue.Source` (a
  three-method interface: `Claim(ctx, limit)`, `Wait(ctx)`, `Wake()`), and
  `Pool.Run` becomes the claim loop of §1.5. `Halt`'s `stopPull`/`haltMu`
  deleted.
- `cmd/harness/serve.go` — `queue.Connect`, `queue.EnsureStreams`, `nc`, `js`
  all deleted; `q := &queue.Queue{Store: st, ...}` built instead;
  `publishAdapter` and `evalPublisher` both wrap `q`; `mcpSvc.Publisher = q`;
  `api.Consumer` → `api.Queue`; the startup log line drops `cfg.NATSURL`.
- `cmd/harness/publish.go` — the publish becomes `POST /api/runs` against
  `DEEPSEEK_HARNESS_BASE_URL` (falling back to `http://127.0.0.1:` + the port
  from `DEEPSEEK_HTTP_ADDR`), with the bearer token fetched from
  `GET /api/control-token`. All NATS imports go.
- `internal/httpapi/server.go` — `QueueConsumer` replaced by
  `QueueStats { Stats(ctx) (queue.Stats, error) }`; the `jetstream` import
  deleted. `internal/httpapi/requests.go` — `handleQueueHealth` reads the new
  stats (see §6).
- `internal/queue/request.go` — `PublishRequest` deleted (its job moved to
  `Queue.Enqueue`); the `requestIDDisallowed` comment rewritten from "NATS
  subject token" to URL-path safety (the rule itself stays: `request_id` is a
  path segment in `/api/requests/{request_id}`).

*Exit criteria:* nothing calls NATS. `go.mod` still lists `nats.go` (removed
next phase, so this phase's diff stays reviewable).

### Phase 6 — delete the infrastructure

`go mod tidy` drops `github.com/nats-io/nats.go`. `internal/queue/stream.go`
deleted outright (with `IsolateForTest`). The three `testmain_test.go` files
deleted. `docker-compose.test.yml` deleted. `docker-compose.yml` /
`docker-compose.prod.yml`: the `nats` service, the `depends_on`, the `NATS_URL`
env, the `nats-data` volume. `Dockerfile`: `ARG NATS_SERVER_VERSION` and the
`nats-server` download layer. `scripts/test.sh`: collapses to the
`go list | grep -v /workspaces` filter plus `go test`. `internal/config`,
`internal/worktree`, `.env.example`, `scripts/docker-entrypoint.sh`, and all
documentation (§8, §9).

---

## 3. Schema, migration, and the upgrade window

**Migration approach.** `work_queue` is a brand-new table, so it is added to
the `schema` const in `internal/store/schema.go` and created by the existing
`CREATE TABLE IF NOT EXISTS` block on `Open`. **No `migrateTableColumns` entry
is needed** — that mechanism exists for adding columns to tables an older
binary created, and there is no older `work_queue`. Do not add a
`workQueueMigrationColumns` slice; add one only when a column is later added to
a shipped `work_queue`.

`work_requests` is untouched: no new columns, no changed semantics, no changed
`shouldClaim`.

**Messages in flight at upgrade.** The WORK stream has WorkQueue retention, so
its depth is exactly "requests enqueued but not yet claimed". In steady state
with a pool of 4 that is normally zero. A drain is required, and it is cheap:

1. Stop producing (no browser starts, no MCP launches, no evals) for the deploy
   window.
2. Confirm the queue is empty: `GET /api/queue` reports `consumer_lag: 0` and
   `in_flight: 0`, or check the NATS monitor directly at
   `http://localhost:${NATS_MONITOR_PORT}/jsz?streams=1` for
   `WORK.state.messages == 0`.
3. Deploy.

This needs a one-off note in `RELEASE.md`, in the style of the existing
"One-off, the first deploy of the settings-screen change" section:

> **One-off, the deploy that removes NATS: drain the WORK stream first.**
> Requests sitting in JetStream at the moment of the deploy are not migrated —
> the new binary never connects to the broker and cannot see them. Confirm
> `GET /api/queue` reports zero depth and zero in flight before promoting.
> Anything left in WORK is lost and must be republished under a new
> `request_id`. Results in flight need no care: the durable result has always
> been the `work_requests` row, which the new binary reads.
>
> **After a successful deploy, `docker volume rm deepseek-harness-prod_nats-data`.**
> The volume is orphaned; leaving it costs disk and misleads the next reader.
> This makes the release one-way in the sense §"State does not roll back with
> the image" already describes.

Also update RELEASE.md's "**`deploy` drains rather than cuts**" paragraph: "A
run that does not finish in time leaves its queue message unacked and is
redelivered to the new container" stays true, but the mechanism is now lease
expiry rather than AckWait, and the volume named is `harness-data` alone.

---

## 4. Every §4.10 semantic, and where it lives afterwards

| §4.10 semantic | Today | After |
| --- | --- | --- |
| `AckWait` 60s | consumer config | `queue.LeaseDuration = 60 * time.Second`; `lease_expires_ms` on claim |
| `InProgress` heartbeat every 20s | `Pool.heartbeat` → `msg.InProgress()` | unchanged shape; `HeartbeatWork` bumps `lease_expires_ms`. One tiny UPDATE per 20s per in-flight run |
| Publish result, **then** ack | `FinishWorkRequest` then `JS.Publish` then `msg.Ack()` | The row **is** the result. `Pool.finish` does the guarded UPDATE and the queue-row DELETE **in one `submit` transaction** — the crash window between them closes entirely. A strict improvement, not a preservation |
| `Nats-Msg-Id` dedup on `final` | `WithMsgID(requestID + ".final")` | Gone. `work_requests.request_id` is the primary key; the guarded `UPDATE … WHERE request_id = ? AND session_id = ?` already prevents a stale attempt overwriting |
| `Term` a malformed request | `msg.Term()` after publishing `failed` | `Msg.Term()` → `DELETE FROM work_queue`. `Pool.handle`'s two Term-on-unparseable branches unchanged |
| `Nak` with delay | `msg.NakWithDelay(p.retryLaterDelay())` | `Msg.Nak(delay)` → `visible_at_ms = now+delay, lease_expires_ms = 0` |
| Last delivery publishes `failed` then `Term` | `Pool.retryLater` compares `deliveryCount(msg)` to `p.maxDeliveryAttempts()` | Identical logic on `msg.DeliveryCount()`. **Additionally** `NakWork` deletes a row that has reached the ceiling and `ClaimWork` filters `delivery_count < max` — this replaces server-side `MaxDeliver` for the Nak paths that do *not* go through `retryLater` (`recoverPanic`, and `finish`'s own encode/store failures). Without it those loop forever |
| `MaxAckPending` = pool size | JetStream is the flow controller | `Claim(ctx, p.size() - len(sem))`. The pool is the flow controller; §4.10's "the harness pulls only what it can run and the backlog stays in the stream" becomes "…stays in the table", still visible and still restart-surviving |
| Single-use `session_id` | `store.shouldClaim` on `numDelivered` | **Unchanged.** Preserved only because one enqueue = one row = one independent counter (§1.2) |
| Abandoned-session close | `Pool.handleSpent` / `closeAbandonedSession` | Unchanged |
| False-redelivery guard | `abandonedSessionIdleThreshold` (10 min) passed to `CloseSession` | Unchanged |
| `waitForResolution` heartbeat-while-waiting | heartbeats the JetStream message | heartbeats the lease. A waiting message now writes one row per 20s for up to its whole deadline. Acceptable; call it out in the comment |
| Graceful drain on shutdown | `consumeCtx.Stop()`, then `wg.Wait()` | the claim loop stops on `ctx.Done()`, then `wg.Wait()`. In-flight runs finish and ack normally. Identical |
| Hard kill loses the run | unacked message redelivers; the spent path fails it | leased row's lease expires (60s); the spent path fails it. Identical |
| "An empty server converges rather than needing a setup script" | `EnsureStreams` | `store.Open` applies the schema. Strictly better — one fewer thing to converge |
| Backlog survives a restart | `nats-data` volume | `harness-data` volume. One volume instead of two |
| "A NATS client in any language can publish directly" | true | **Lost.** `POST /api/runs` is the replacement; see §10 |

---

## 5. The three RESULTS waiters

All three become HTTP against `GET /api/requests/{request_id}`, which already
exists in `internal/httpapi/requests.go` and already returns `session_id`,
`status`, and the full stored `result` JSON as `workRequestRow`. **No new
endpoint is required.**

The one gap worth knowing about: `GET /api/requests/{id}` 404s until the pool
claims the request, because `work_requests` rows are created by
`ClaimWorkRequest`, not at enqueue. That 404 is the correct "still queued"
signal and every waiter treats it as such. (Pre-creating a `queued` row is
explicitly not being done — see §11.)

### 5.1 `deepseek_result` (`internal/mcp/collect.go`)

Contract today: never blocks, one Fetch with a 300 ms floor, returns the final
result if there is one or `renderPending` otherwise.

After: one
`getJSON(ctx, svc.HTTPClient, svc.Cfg.HarnessBaseURL, "/api/requests/"+in.RequestID, &row)`.

- Transport error, or a non-200 that is not 404 → `errorResult`.
- 404, or `row.Status == "running"`, or `row.Result` empty →
  `renderPending(in.RequestID)`.
- Otherwise `json.Unmarshal(row.Result, &res)` into `queue.Result`, then the
  existing `svc.Registry.updateFromResult` + `renderFinal` unchanged.

`minFetchWait` is deleted. The tool description in `registerCollectTool`
changes: "reads the RESULTS stream once" → "reads the harness's work-request
row once". The "even long ago, even from a different process" promise gets
*stronger* — the row has no 7-day retention window, so results outlive what the
stream retained. `getJSON` must be extended (or a sibling added) to distinguish
404 from other non-200s; today it collapses them into one error string.

### 5.2 `deepseek_agent`'s accepted wait (`internal/mcp/launch.go`)

The pre-publish subscription is deleted outright — under HTTP there is no race
to guard against, because the row is durable and can be read at any time after
the fact. Replace with, after `svc.Publisher.Publish(...)` and
`svc.Registry.recordLaunch(...)`:

```go
deadline := time.Now().Add(time.Duration(waitMS) * time.Millisecond)
for {
	var row workRequestRow
	if err := getJSON(ctx, ..., "/api/requests/"+requestID, &row); err == nil && row.SessionID != "" {
		status, sessionID = "running", row.SessionID
		break
	}
	if time.Now().After(deadline) { break }
	time.Sleep(50 * time.Millisecond)   // or a ticker bounded by ctx
}
```

`Cfg.AcceptedWaitMS` keeps its meaning and its default. The current default
path (`waitMS <= 0 → 1`) means one immediate read, which is what a 1 ms Fetch
gives today. The "no accepted message within the wait window; the pool is
likely full" text stays, with "accepted message" reworded to "the pool had not
claimed it".

### 5.3 `harness publish -wait` (`cmd/harness/publish.go`)

The whole NATS block goes. The command becomes:

1. Resolve the base URL: `DEEPSEEK_HARNESS_BASE_URL`, else
   `http://127.0.0.1:<port of DEEPSEEK_HTTP_ADDR>`.
2. `GET /api/control-token` for the bearer token (loopback-only, no auth — the
   same thing `internal/mcp/control.go` does).
3. `POST /api/runs` with the `queue.Request` body plus the token. **Note that
   `handleStartRun` overwrites `parent_is_user`, `parent_agent_type`, and
   `parent_agent_id`** — so `-parent-agent-type`, `-parent-agent-id`, and
   `-parent-is-user` would silently stop working on this path. Either drop
   those three flags from `publish` with a message pointing at the API, or
   (better) leave them and note in the help text that `POST /api/runs` stamps
   provenance server-side. This is a genuine behaviour change and must be
   called out in the phase-5 commit message, in DESIGN §4.10's
   producer-stamping list, and in README.
4. `-wait`: poll `GET /api/requests/{id}` every 2 s until `finished_at != null`
   or the timeout; print the decoded `result` as today. A 404 means "not
   claimed yet" and keeps polling. The existing 60-minute default (mirroring
   `run.deadline` because publish has no database handle) is unchanged — and
   note publish now has an HTTP handle, so it *could* read the real setting,
   but do not: it would add a settings-read endpoint dependency for no gain.

The doc comment "It is an operator and demonstration tool, not the harness's
only ingress path — a NATS client in any language can publish the same JSON
body directly" must be rewritten: it is now an HTTP client of the same endpoint
the browser uses, and the language-agnostic ingress is `POST /api/runs`.

**`harness publish` now requires `harness serve` to be running.** Today it can
publish into an empty queue with serve down. This is a real regression; it
belongs in the README troubleshooting section and in the command's own error
message ("could not reach the harness at %s — `harness publish` posts to the
running service; start `harness serve` first").

---

## 6. `/api/queue`

Redefined, not dropped. `internal/httpapi/server.go`:

```go
// QueueStats is the subset of the queue the health endpoint reads.
type QueueStats interface {
	Stats(ctx context.Context) (queue.Stats, error)
}
```

replacing `QueueConsumer`, and `Server.Consumer QueueConsumer` →
`Server.Queue QueueStats`. `queue.Stats`:

```go
type Stats struct {
	Depth       int // claimable now: visible_at <= now, unleased
	Scheduled   int // visible_at > now — the Nak-delayed backlog. New; JetStream never exposed this
	InFlight    int // leased and not expired
	Redelivered int // rows with delivery_count > 1
}
```

`handleQueueHealth` keeps its shape: `Halted`/`HaltReason` from `s.Pool` first,
`Available: false` when `s.Queue == nil` (a CLI-only harness), the store error
surfaced through `Error` rather than a 500.

**Wire field names.** `web/src/components/SessionListScreen.tsx` already says
the counters "(pending / in flight / redelivered) are gone" from the UI — only
`available`, `halted`, and `halt_reason` are rendered. So rename `consumer_lag`
→ `queue_depth` (there is no consumer to lag any more) and add `scheduled`,
updating `QueueHealth` in `web/src/api/types.ts` to match. A two-line frontend
change with no rendering impact.

**The compose healthcheck depends on this endpoint** (`docker-compose.yml`'s
`harness` service probes `http://localhost:8080/api/queue`). It must keep
answering 200 with `s.Queue` wired, which it does.

---

## 7. Test strategy

### `queue.IsolateForTest` can be deleted

Its entire justification, spelled out in its own comment and in the three
`TestMain`s, is that `internal/queue`, `internal/worker`, and `internal/mcp`
shared one broker. With per-test SQLite files there is nothing shared:
`internal/store`'s `openTestStore` and `internal/worker/pool_test.go` already do
`store.Open(filepath.Join(t.TempDir(), "harness.db"))`. Delete
`IsolateForTest` and all three `testmain_test.go` files (they contain nothing
else).

### Delete outright

- `internal/queue/stream_integration_test.go` — all four tests are about
  JetStream server behaviour: stream convergence, `MaxAckPending` update,
  `Nats-Msg-Id` dedup, `MaxDeliver` on the consumer. None has a successor
  concept.
- `internal/queue/progress_test.go` — with `ProgressLimiter`.
- The `connectOrSkip` / `testNATSURL` / `ensureTestStreams` helpers in
  `internal/queue/stream_integration_test.go`, `internal/worker/pool_test.go`,
  and `internal/mcp/integration_test.go`.
- `capturingJS` (integration_test.go), `captureJS` / `emptyConsumer` /
  `emptyBatch` (`internal/mcp/launch_validation_test.go`), `fakeConsumer`
  (`internal/httpapi/server_test.go`).

### Rewrite

- `internal/worker/pool_test.go` — the largest single test edit (985 lines).
  Two families:
  - *Broker-driven:* `testHarness` drops `js`; enqueue goes through
    `q.Enqueue`; the result-reading helpers (the two `OrderedConsumer`+`Fetch`
    blocks) become a poll of `st.GetWorkRequest(ctx, requestID)` until
    `FinishedAt != nil`. This is a *simplification*: the assertions were always
    about the result contents, which the row holds verbatim.
  - *`fakeMsg`-driven:* shrinks to `queue.Msg` (phase 1). These tests keep
    working unchanged through phases 3–6.
- `internal/worker/control_test.go` and `creating_test.go` — check for
  `jetstream` references; both drive `p.handle` with `fakeMsg` and should need
  only the interface shrink.
- `internal/mcp/integration_test.go` — the three launch-outcome tests and the
  two collect tests. `Service.JS` → `Service.Publisher` (a `capturePublisher`
  recording the marshalled request, replacing `capturingJS`), and the two tests
  that publish a final result to RESULTS and then call `handleCollect` instead
  seed a `work_requests` row via a real store behind an `httptest.Server`
  running the real `httpapi.Server` — the package already has
  `startTestServer` and already reads over HTTP, so this is a small extension
  of an existing pattern. The `HarnessBaseURL: "http://127.0.0.1:0"`
  placeholder becomes the test server's URL.
- `internal/mcp/launch_validation_test.go` — the three fakes become one
  `capturePublisher`.
- `internal/httpapi/server_test.go` — the three queue-health tests:
  `fakeConsumer{info: &jetstream.ConsumerInfo{NumPending:7, NumAckPending:2, NumRedelivered:1}}`
  → `fakeQueueStats{stats: queue.Stats{Depth:7, InFlight:2, Redelivered:1}}`.
- `internal/config/config_test.go` — drops `cfg.NATSURL` from its assertion.
- `internal/settings/registry_test.go` — drops the `queue.results_max_age` pin.
- `internal/worktree` tests — port-count and env-block assertions.

### New

`internal/store/work_queue_test.go` (the SQL) and
`internal/queue/sqlite_test.go` (the `Msg` semantics), each on a fresh
`t.TempDir()` database, each passing an explicit `now` so nothing sleeps:

1. **Enqueue → claim round-trips the payload byte-identically** and
   `ParseRequest` gets back an equal `Request` (the successor to
   `TestPublishRequestLandsOnWorkStream`).
2. **Claim respects `visible_at`** — a Nak'd row is invisible until `now`
   passes the delay, then claimable.
3. **Lease expiry redelivers** — claim, do not ack, advance `now` past the
   lease, claim again: the same row, `delivery_count == 2`.
4. **Crash-then-redeliver, end to end** — the §4.10 pre-session-death path:
   claim, do not ack, expire, re-claim, feed the re-claimed `Msg` to
   `store.ClaimWorkRequest` with `numDelivered = 2` against a sessionless
   `running` row, and assert `Claimed == true`. This is the semantic the whole
   surrogate-key decision exists to protect and nothing tests it end to end
   today.
5. **Concurrent claim is disjoint** — N goroutines each claiming with
   `limit: 1` against M rows get M distinct rows, never a duplicate, and
   `N > M` leaves the extra claimers empty-handed.
6. **Claim honours the limit** — `Claim(ctx, 2)` against 5 rows returns 2, in
   `visible_at, id` order.
7. **Delivery ceiling** — a Nak on the attempt that reaches `MaxDeliveries`
   deletes the row; `Claim` never returns a row at or over the ceiling.
8. **Ack and Term delete; InProgress extends** — after `InProgress`, a claim at
   the original expiry time finds nothing.
9. **Stats** — depth / scheduled / in-flight / redelivered against a hand-built
   table.
10. **The wake channel** — `Wake()` releases a blocked `Wait()`; a second
    `Wake()` with the buffer full does not block; `Wait()` returns on
    `ctx.Done()`.

Plus, in `internal/worker/`, a new **in-process end-to-end test**: enqueue → the
real pool claims → the fake DeepSeek `httptest.Server` answers → the
`work_requests` row reaches `ok`. This is now cheap enough to be an ordinary
unit test, which is the whole point.

### `scripts/test.sh` and TESTING.md

`scripts/test.sh` loses `env_value`, the URL resolution, the compose project
derivation, the compiled TCP probe, the `nats-server` fallback, and the three
traps — roughly 150 lines down to about 20. What must survive is the
`go list ./... | grep -v /workspaces` filter and its comment explaining why
(agent workspaces are part of this Go module and one prod session's `scratch/`
breaks `./...`). Keep the script as the documented entry point.

`TESTING.md`: the "Go integration" row in the layers table is redefined (or
merged into "Go unit") — there is no external dependency left in the Go suite
except `git`. The whole "### The broker the integration tests use is not the one
the harness uses" section is deleted. "Prerequisites" drops Docker entirely.
"What to test where" — the `internal/queue, internal/worker, internal/mcp —
integration only. Assert against a real broker` bullet becomes "assert against a
real SQLite file in a temp dir; the queue is a table now and its edges — lease
expiry, concurrent claim, the delivery ceiling — are the things to cover".
"Known-awkward tests" drops the `HARNESS_TEST_NATS_OPTIONAL` entry.

---

## 8. Config, compose, worktrees

**`internal/config/config.go`** — delete `defaultNATSURL`, the `NATSURL` field,
and the `envOr("NATS_URL", ...)` line. Update the `Config` doc comment ("only
the five values below stay environment-only" → four).

**`docker-compose.yml`** — delete the `nats` service (25 lines including the
healthcheck), the `harness` service's `depends_on` block, the
`NATS_URL: nats://nats:4222` environment line and its two-line comment, and the
`nats-data:` volume. Same three deletions in `docker-compose.prod.yml`.
**Delete `docker-compose.test.yml` entirely** — it contains nothing but the
broker.

**`internal/worktree/ports.go`** — delete `bandNATSClient`,
`bandNATSMonitor`, `bandTestNATS`, the `NATSClient`/`NATSMonitor`/`TestNATS`
fields on `Ports`, and their entries in `list()`. Five ports per slot become
two (`HarnessHTTP`, `Vite`). Update the band comment block, which currently
enumerates what the bands clear.

*Effect on existing allocated worktrees:* benign, because of two properties.
`PortsForSlot` is a pure function of the slot, and `AllocateSlot` (in
`internal/worktree/registry.go`) keys off `usedSlots()`, not off stored ports —
so every existing worktree keeps its slot number and simply gets a shorter port
list. The registry entry stores `Ports` as JSON; `encoding/json` ignores the
three now-unknown keys on read and stops writing them on the next save. Nothing
breaks and nothing needs re-allocating. The freed 4600/8600/4700 bands are
simply no longer used; leave the band comments explaining why they were chosen,
since a future service may want them.

**`internal/worktree/env.go`** — remove the four `NATS_*` lines from
`managedBlock`, but **keep `"NATS_CLIENT_PORT", "NATS_MONITOR_PORT",
"NATS_URL", "HARNESS_TEST_NATS_PORT", "HARNESS_TEST_NATS_URL"` in
`managedKeys` for this release**, with a comment saying why: `stripManagedKeys`
is what removes them from an existing worktree's `.env` on the next
`harness worktree init`, so leaving them listed is how stale definitions get
cleaned up rather than lingering as dead lines that shadow nothing. Remove them
from `managedKeys` a release later.

**`cmd/harness/worktree.go`** — delete the three `fmt.Printf` lines printing the
nats client, nats monitor, and test nats ports. `Compose.TestProjectName` and
`dockerTeardownProject` stay: a worktree created before this change may still
have `<project>-test` containers, and teardown works from a project name alone
with no compose file. Remove `TestProjectName` in a later cleanup, not here.

**`scripts/docker-entrypoint.sh`** — drop
`NATS_CLIENT_PORT NATS_MONITOR_PORT HARNESS_TEST_NATS_PORT` from the list of
compose-control variables it strips before a session can see them.

**`Dockerfile`** — delete `ARG NATS_SERVER_VERSION=2.10.29` and the
`wget | tar` layer that installs `nats-server`, plus the comment block above
them. The image gets smaller and loses a network dependency at build time.

**`.env.example`** — delete the NATS block (`NATS_URL`, `NATS_CLIENT_PORT`,
`NATS_MONITOR_PORT`) and the `HARNESS_TEST_NATS_*` block; fix the line that says
the environment block "overrides `NATS_URL`, the HTTP…".

**`internal/settings/registry.go`** — remove `KeyQueueResultsMaxAge` and its
descriptor. Reword two descriptions: `worker.pool_size`'s "also the WORK
consumer's `MaxAckPending`" → "also how many queue rows the pool holds leased at
once"; `worker.max_delivery_attempts`'s "before JetStream stops redelivering it"
→ "before the queue stops redelivering it". Both stay `.withRestart()` — they
are still read once at startup.

---

## 9. Documentation, file by file

| File | What changes |
| --- | --- |
| `docs/DESIGN.md` | **§4.10 is rewritten and retitled** — "Work ingress over NATS JetStream" → "Work ingress and the durable work queue". The stream/consumer diagram becomes the `work_queue` table and its claim query. The `docker-compose.yml` NATS paragraph goes. The acknowledgement-discipline bullet list survives almost verbatim with the mechanism substituted per §4, including the note that publish-then-ack collapses into one transaction. The "Idempotency is a row, not a convention" block is **unchanged** — it was always the authority and now has no competitor. The `progress` paragraph goes (no consumer; the SSE stream is the progress channel). Add a paragraph on the ingress change: `POST /api/runs` is the remote ingress and `harness publish` is a client of it. Cross-references from §4.2, §4.5, §5.8 need checking for stream language |
| `ARCHITECTURE.md` | Overview paragraph ("Requests arrive over NATS JetStream; results go back over a second stream"). The mermaid diagram: `WORK[(WORK stream)]` → `WORK[(work_queue table)]`, `RESULTS` node and both its edges deleted, `caller` reads back from `http`. `RunPublisher` "over the queue's own JetStream handle" → "over the store-backed queue". The bullet "**`internal/worker` is the only package that acks a JetStream message**" → "acks a queue message". Configuration section: "the NATS address" removed from the bootstrap list |
| `CLAUDE.md` | The two-line opening ("Work arrives on a NATS JetStream queue … returns a result to a results stream") |
| `internal/CLAUDE.md` | `internal/queue` entry rewritten (no longer "JetStream wiring shared by every NATS caller"; now the request shape, validation, and the store-backed queue). `internal/hub`'s "Touches no JetStream — the browser reads the store and the hub, never NATS". `internal/worker`'s "publishes the result, then acks". `internal/mcp`'s "handed serve's own JetStream handle" and "Backed by the WORK and RESULTS streams". `internal/httpapi`'s "holds no JetStream handle". `internal/evals`' "Publishes a suite of tasks … through the WORK stream". `internal/config`'s bootstrap list |
| `README.md` | The opening two lines; the "Two services come up: NATS with JetStream and the harness itself" paragraph → one service; the ports table (`NATS client / monitor` row removed); the `nats-data` volume bullet; the "`harness publish` is an operator's tool, not the only ingress: a NATS client in any language can publish" paragraph → `POST /api/runs`; `docker compose up -d nats` in the manual-run section; the "A port is already in use. NATS on 4222…" troubleshooting entry. Add: "`harness publish` requires `harness serve` to be running" |
| `TESTING.md` | As §7 |
| `RELEASE.md` | As §3 — the drain note, the volume cleanup, the "`deploy` drains rather than cuts" mechanism, and the rollback caveat's "or in a JetStream stream" |
| `docs/WORKTREES.md` | The port table's NATS client / NATS monitor / test NATS rows; the "they'd become the *same* compose project fighting over one `nats`" rationale; the `nats-data` named-volume mention |
| `docs/DATA-API.md` | "the HTTP server holds no JetStream handle"; "publishes … to the WORK stream through the declared `RunPublisher`"; the attachments section's "never inline in the NATS request — the default `max_payload` is 1 MB" (the size argument dies with NATS, but the *rule* stays for a better reason: the store is the authority `harness export` derives from, which is already the second half of that sentence — keep the rule, replace the justification); the §4.10 link anchor `#410-work-ingress-over-nats-jetstream` changes with the heading |
| `docs/RUN-CONTROL.md` | "CLAUDE.md's warning against giving `httpapi` a NATS…"; "NATS stays what §4.10 designed it to be: durable, decoupled, at-least-once"; the `RunPublisher` comment block; "It holds no JetStream handle — it already imports `jetstream` for `QueueConsumer`'s types"; the `inflight` struct listing `msg jetstream.Msg`; "dispose of the JetStream message"; "**The JetStream message must be disposed of by whoever gets there first**"; "over the SSE stream and, for a queue caller, over the RESULTS stream"; the "control subject on NATS" speculation near the end |
| `docs/EVALS.md` | "Runs go through the WORK stream like any other work request"; "still holds no JetStream handle" |
| `docs/KIMI-INTEGRATION.md`, `docs/DSH-COMPARISON.md` | Incidental mentions; check and reword |
| `docs/RUN-CONTROL-PLAN.md`, `docs/reviews/*` | **Leave alone.** These are historical records of decisions as they were made; rewriting them falsifies the record |
| `.claude/skills/prod-diagnostics/SKILL.md`, `.claude/skills/worktree-create/SKILL.md` | Both name NATS ports / the nats container. Update — these are live operator tooling, not history |
| `assets/skills/deepseek-flash-task/SKILL.md`, `assets/skills/deepseek-harness-wait/SKILL.md` | Shipped skills that describe the queue to an agent. Update the mechanism descriptions |

---

## 10. Risks, honestly

**SQLite contention under pool concurrency.** Every queue operation is a
single-row read or write inside the store's one writer goroutine — the same
goroutine the session loops use for their per-sub-turn event commits. At pool
size 4, steady-state additional writes are: one heartbeat per 20 s per
in-flight run (≤ 0.2/s), one claim per wake, one ack per finished run.
Negligible next to a sub-turn's event batch. It becomes worth measuring at pool
sizes in the dozens, at which point the claim's `LIMIT n` batching already
amortises. The real hazard is not throughput but **an idle harness taking the
write path once a second forever**, which is why `Queue.Claim` gates on a read
(`WorkQueueClaimable`) before submitting. Do not skip that.

**Ticker latency on run start.** With the nudge on `Enqueue` and on slot
release, every in-process producer sees ~zero added latency. The ticker only
covers two cases neither of which is latency-sensitive: a lease expiring
(detected within one tick against a 60 s lease) and a Nak delay maturing (the
delay is already 5 s). If the nudge is ever dropped or mis-wired, the symptom is
a uniform 1 s delay on run start — a good, loud symptom. Start at
`PollInterval = 1s` and treat any pressure to shorten it as evidence the nudge
is broken rather than as a tuning problem.

**Loss of the language-agnostic ingress.** §4.10's "a NATS client in any
language can publish the same JSON body directly" is real and is going away.
`POST /api/runs` is the mitigation and is arguably better — HTTP, validated with
the same `queue.Request.Validate`, provenance stamped server-side, attachments
already carried. What is genuinely lost: publishing while the service is down.
Nothing in this repo depends on that, and `restart: unless-stopped` covers the
operational case, but say so rather than pretending the promise transferred
cleanly.

**Permanent co-location of the queue with `serve`.** The queue now lives in
serve's SQLite file. A second `harness serve` on another machine cannot consume
it. This formalises a constraint that already existed in practice — the store is
opened by one writer process and `internal/mcp` is embedded in serve precisely
because "serve is the single writer" — but it forecloses a horizontal-scaling
option the broker theoretically kept open. If that ever becomes real the answer
is a Postgres-backed store, not re-adding a broker; write that down in §4.10 so
the next person does not re-litigate.

**The delivery ceiling now has two enforcement points.** `Pool.retryLater`
(which publishes a terminal `failed` on the last attempt) and `NakWork` (which
discards at the ceiling). The paths that Nak *outside* `retryLater` —
`recoverPanic` and `finish`'s encode/store failure branches — hit the second,
which silently discards, exactly as JetStream did. Resist the temptation to
route those through `retryLater`: `retryLater` calls `finish`, and `finish`'s
failure path calls Nak, so that route is a recursion when the store is down.

**Timestamp comparison.** Covered in §1.2 — integer milliseconds, not
RFC3339Nano. If someone "harmonises" the columns with the rest of the schema
later, claim ordering silently breaks for sub-second timings. The comment in
`schema.go` is load-bearing.

**`RETURNING` support.** Verify against `modernc.org/sqlite` in phase 4 before
the claim query is built on it; the SELECT-then-UPDATE-in-one-transaction
fallback is equivalent because of the single writer goroutine.

**Test-suite time dependence.** The queue's semantics are all about `now`. Every
store method takes an explicit `now time.Time` (the convention
`ClaimWorkRequest`, `CloseWorkRequest`, and `DeleteWorkspaceLease` already
follow) so no test sleeps. The pool's own ticker still needs `PollInterval`
overridable in tests, like `HeartbeatInterval` and `LeasePollInterval` already
are.

**`harness publish` provenance flags.** `handleStartRun` overwrites
`parent_is_user`/`parent_agent_type`/`parent_agent_id`. §5.3 covers it; it is
the one user-visible behaviour change that is easy to ship without noticing.

---

## 11. Explicitly not being done

- **No dead-letter table.** A `Term`'d row and a ceiling-exhausted row are
  deleted, exactly as JetStream discarded them from a WorkQueue stream. The
  `work_requests` row already carries the terminal `failed` result for every
  path that produces one.
- **No pre-created `queued` row in `work_requests`.** Tempting — it would make
  `GET /api/requests/{id}` answer before the claim instead of 404ing — but it
  would perturb `store.shouldClaim`, whose three-way refusal (`terminal` /
  `spent` / `owned elsewhere`) currently distinguishes "no row" from "row
  exists". Not worth touching in this change.
- **No priorities, no fairness, no per-model queues, no batching by profile.**
  FIFO by `visible_at, id`, matching WorkQueue retention.
- **No multi-process or multi-host consumers.** The lease machinery would
  support it; nothing else in the harness would.
- **No change to `queue.Request`, `Validate`, `Repo`, or the request wire
  shape** — only the comment justifying `requestIDDisallowed`.
- **No change to `work_requests`**: no columns, no `shouldClaim`, no single-use
  rule, no abandoned-session close, no `ClaimOutcome` vocabulary.
- **No change to the run-control seams** (`RunController`, `RunPublisher`,
  `EvalController`), the hub's shapes, the SSE streams, or the session list.
- **No embedded `nats-server` as a library, no river/asynq/watermill/goqite.**
  Settled.
- **No Postgres.**
- **No removal of `harness publish`** — it survives as an HTTP client.
- **No rewriting of `docs/RUN-CONTROL-PLAN.md` or `docs/reviews/`** — those are
  the record of what was decided when.
- **No `harness queue import` drain tool.** The drain is a two-minute
  operational step documented in RELEASE.md, not code that exists forever for
  one deploy.

---

## Critical files

- `internal/worker/pool.go` — the claim loop, `handle`/`run`/`finish`/
  `retryLater`/`heartbeat`, and every `jetstream.Msg` touch point
- `internal/store/schema.go` — the `work_queue` DDL, its indexes, and the
  integer-milliseconds decision
- `internal/queue/stream.go` — deleted; its replacements are `msg.go` and
  `sqlite.go` in the same package
- `cmd/harness/serve.go` — the only composition point; `queue.Connect`/
  `EnsureStreams` out, `queue.Queue` in, all three publisher adapters rewired
- `internal/mcp/launch.go` and `internal/mcp/collect.go` — two of the three
  RESULTS waiters, and the last NATS handle outside `cmd/`
- `docs/DESIGN.md` §4.10 — the section this whole change is a rewrite of
