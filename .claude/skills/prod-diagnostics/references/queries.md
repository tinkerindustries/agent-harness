# Worked queries against the harness store

Every query here has been run against the production database. Pass them to
`scripts/prodsql.py`, which handles the container hop and opens the file
read-only.

The joins are the part worth copying. Cost and token figures live in the
JSON payload of `usage` events rather than on the session row, so almost
every quantitative question goes through `json_extract`, and getting the
grain wrong (one row per sub-turn, not per session) is the usual mistake.

- [Payload shapes](#payload-shapes)
- [Sessions](#sessions)
- [Cost and tokens](#cost-and-tokens)
- [Failures](#failures)
- [Tools](#tools)
- [Queue and workspaces](#queue-and-workspaces)

## Payload shapes

`events.payload` is JSON text. The fields that matter, by kind:

| Kind | Fields |
| --- | --- |
| `usage` | `sub_turn`, `prompt_tokens`, `prompt_cache_hit_tokens`, `prompt_cache_miss_tokens`, `completion_tokens`, `reasoning_tokens`, `cost_usd`, `expected_miss_tokens` |
| `run_finished` | `reason`, and `text` or `summary` depending on how the run ended |
| `error` | `message` |
| `tool_call` | `index`, `id`, `name`, `arguments` |
| `tool_result` | tool output, shape varies by tool |

One `usage` event per sub-turn, so a session's cost is a `SUM` over its
usage events. There is no cost column on `sessions`.

Not every `usage` event is a DeepSeek call. The vision path behind
`ReviewScreenshot` bills a different provider at roughly 20× the cost per
call, and the session row's `model` column still says `deepseek-v4-flash`
for the whole run. So any cost query grouped by `sessions.model` books
vision spend as DeepSeek.

The obvious fix does not work on its own. Newer usage events carry a `model`
field in the payload, but it was only added part-way through the store's
history:

```sql
SELECT coalesce(json_extract(payload, '$.model'), '(untagged)') AS model,
       count(*) AS events,
       min(created_at) AS first_seen,
       round(sum(json_extract(payload, '$.cost_usd')), 4) AS usd
FROM events WHERE kind = 'usage' GROUP BY model ORDER BY usd DESC
```

Run that before trusting it. At the time of writing only a handful of events
are tagged and all of them are recent, so grouping by the payload `model`
finds a small fraction of the real vision spend and silently attributes the
rest to DeepSeek — an order of magnitude out.

Identifying older vision calls means reconciling each event's `cost_usd`
against the rates in the price table, which is analysis rather than a query.
[`docs/reviews/vision-path-2026-08-14.md`](../../../../docs/reviews/vision-path-2026-08-14.md)
already did it. Read that before redoing the work, and treat any
vision-versus-model cost split derived from the `model` field alone as a
lower bound rather than an answer.

A second trap sits in the price table rather than the store. Read it before
quoting any cost as current:

```bash
scripts/prod.sh exec -T harness cat /etc/harness/prices.json
```

It carries a `pending_change` block that the loader ignores — `Cost()` has no
clock — so stored `cost_usd` values are computed at the flat rates and go
wrong from that block's `effective_at`. A cost figure is only as current as
that file.

Timestamps are text and sort lexically, so `created_at >= '2026-08-01'`
works as a date filter and `substr(created_at, 1, 10)` is the day.

## Sessions

Recent runs with their outcome:

```sql
SELECT substr(id, 6, 8) AS sid, status, complete_status, model, effort,
       substr(created_at, 1, 19) AS started, title
FROM sessions
ORDER BY created_at DESC
LIMIT 20
```

`status` is the lifecycle (`ok`, `failed`, `max_turns`, `cancelled`,
`running`, `creating`); `complete_status` is what the model itself reported.
They disagree when a run finished mechanically but did not do the job, which
is worth noticing.

How runs ended, over a period:

```sql
SELECT status, count(*) AS n
FROM sessions
WHERE created_at >= '2026-08-01'
GROUP BY status ORDER BY n DESC
```

Runs per day:

```sql
SELECT substr(created_at, 1, 10) AS day, count(*) AS runs,
       sum(status = 'ok') AS ok, sum(status = 'failed') AS failed
FROM sessions GROUP BY day ORDER BY day DESC LIMIT 14
```

Wall-clock duration of finished runs, longest first. `julianday` differencing
is the only arithmetic SQLite offers on these text timestamps:

```sql
SELECT substr(id, 6, 8) AS sid, status,
       round((julianday(finished_at) - julianday(created_at)) * 24 * 60, 1) AS minutes,
       title
FROM sessions
WHERE finished_at IS NOT NULL
ORDER BY minutes DESC LIMIT 20
```

Who launched what — the provenance triple distinguishes a person's run from
one agent launching another:

```sql
SELECT job_type, parent_agent_type, parent_is_user, count(*) AS n
FROM sessions GROUP BY 1, 2, 3 ORDER BY n DESC
```

## Cost and tokens

Total spend over a period:

```sql
SELECT round(sum(json_extract(e.payload, '$.cost_usd')), 4) AS usd
FROM events e JOIN sessions s ON s.id = e.session_id
WHERE e.kind = 'usage' AND s.created_at >= '2026-08-01'
```

Spend by day:

```sql
SELECT substr(s.created_at, 1, 10) AS day,
       count(DISTINCT s.id) AS runs,
       round(sum(json_extract(e.payload, '$.cost_usd')), 4) AS usd
FROM events e JOIN sessions s ON s.id = e.session_id
WHERE e.kind = 'usage'
GROUP BY day ORDER BY day DESC LIMIT 14
```

The most expensive runs, with their token split:

```sql
SELECT substr(s.id, 6, 8) AS sid, s.status, s.model,
       count(*) AS sub_turns,
       round(sum(json_extract(e.payload, '$.cost_usd')), 4) AS usd,
       sum(json_extract(e.payload, '$.completion_tokens')) AS out_tok,
       s.title
FROM events e JOIN sessions s ON s.id = e.session_id
WHERE e.kind = 'usage'
GROUP BY s.id ORDER BY usd DESC LIMIT 20
```

Prompt cache hit rate, which is the first thing to check when spend jumps
without volume changing — a change to the frozen head of a request
invalidates the cache for every session (`docs/CACHE.md`):

```sql
SELECT substr(s.created_at, 1, 10) AS day,
       round(100.0 * sum(json_extract(e.payload, '$.prompt_cache_hit_tokens'))
             / sum(json_extract(e.payload, '$.prompt_tokens')), 1) AS cache_pct
FROM events e JOIN sessions s ON s.id = e.session_id
WHERE e.kind = 'usage'
GROUP BY day ORDER BY day DESC LIMIT 14
```

One session in detail:

```sql
SELECT json_extract(payload, '$.sub_turn') AS sub_turn,
       json_extract(payload, '$.prompt_tokens') AS prompt,
       json_extract(payload, '$.prompt_cache_hit_tokens') AS cached,
       json_extract(payload, '$.completion_tokens') AS out,
       json_extract(payload, '$.reasoning_tokens') AS reasoning,
       round(json_extract(payload, '$.cost_usd'), 6) AS usd
FROM events WHERE kind = 'usage' AND session_id = 'sess-…'
ORDER BY seq
```

## Failures

Every error message, newest first. `error` events do not always end a run —
a stream read failure mid-session gets retried — so an error here is not by
itself a failed session:

```sql
SELECT substr(e.session_id, 6, 8) AS sid, s.status,
       substr(e.created_at, 1, 19) AS at,
       json_extract(e.payload, '$.message') AS message
FROM events e JOIN sessions s ON s.id = e.session_id
WHERE e.kind = 'error'
ORDER BY e.created_at DESC LIMIT 30
```

Error messages grouped, to see whether one failure mode dominates:

```sql
SELECT json_extract(payload, '$.message') AS message, count(*) AS n
FROM events WHERE kind = 'error'
GROUP BY message ORDER BY n DESC LIMIT 20
```

How runs reported their own ending:

```sql
SELECT json_extract(payload, '$.reason') AS reason, count(*) AS n
FROM events WHERE kind = 'run_finished'
GROUP BY reason ORDER BY n DESC
```

Sessions that stopped without a `run_finished` event at all — these are the
ones killed by a restart or still live. Cross-check against `/api/queue`
before calling one lost:

```sql
SELECT substr(s.id, 6, 8) AS sid, s.status, substr(s.created_at, 1, 19) AS started
FROM sessions s
WHERE NOT EXISTS (
  SELECT 1 FROM events e WHERE e.session_id = s.id AND e.kind = 'run_finished')
ORDER BY s.created_at DESC LIMIT 20
```

Work requests redelivered more than once, which means a worker took the job
and did not ack it:

```sql
SELECT request_id, session_id, status, delivery_count,
       substr(received_at, 1, 19) AS received
FROM work_requests WHERE delivery_count > 1
ORDER BY received_at DESC
```

## Tools

Which tools get used, and how often a call was denied by the permission mode:

```sql
SELECT json_extract(payload, '$.name') AS tool, count(*) AS calls
FROM events WHERE kind = 'tool_call'
GROUP BY tool ORDER BY calls DESC
```

```sql
SELECT substr(session_id, 6, 8) AS sid, count(*) AS denied
FROM events WHERE kind = 'tool_denied'
GROUP BY session_id ORDER BY denied DESC LIMIT 20
```

Find sessions that ran a particular command. `arguments` is a JSON string
inside the payload, so this is a substring match rather than a structured
one:

```sql
SELECT DISTINCT substr(session_id, 6, 8) AS sid
FROM events
WHERE kind = 'tool_call'
  AND json_extract(payload, '$.arguments') LIKE '%docker compose%'
```

## Queue and workspaces

Held leases and how stale their heartbeat is — a lease whose heartbeat
stopped belongs to a dead session:

```sql
SELECT workspace, session_id,
       substr(acquired_at, 1, 19) AS acquired,
       substr(heartbeat_at, 1, 19) AS last_beat
FROM workspace_leases ORDER BY acquired_at
```

Work requests by status:

```sql
SELECT status, count(*) AS n FROM work_requests GROUP BY status ORDER BY n DESC
```

Settings the stack is running with. Values of secret keys are stored
literally in this table, so select `key` alone unless the value is genuinely
what you need, and prefer
`scripts/prod.sh exec -T harness harness config list`, which masks them:

```sql
SELECT key, substr(updated_at, 1, 19) AS updated FROM settings ORDER BY key
```
