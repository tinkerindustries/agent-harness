---
name: prod-diagnostics
description: Get diagnostic information out of the running deepseek-harness production stack — which of the four records (HTTP API, SQLite store, container logs, provider HTTP trace) answers a given question, the exact read-only command for each, and how to join a finding across them. Use this whenever someone asks what happened in prod, why a production run failed or stalled or cost what it did, what the API or the database says about a session or work request, what was actually sent to or returned by the model provider, how many runs or how much spend over some period, or asks to check, inspect, query, dig into, or pull anything out of production. Reach for it on a bare mention of prod, production, the prod stack, or port 8180, and whenever you are about to query the harness database, tail the prod container logs, or open an exchanges.jsonl.gz — even when the user does not name this skill and even when the question sounds like it needs only one quick command.
---

# Getting diagnostics out of production

Production keeps four separate records of the same run, and they do not
contain the same things. Most of the time lost on a production question is
lost in the wrong record: grepping six megabytes of provider trace for a
tool name the events API would have returned in one call, or querying the
database for a workspace failure that never reached the database because it
happened before the session row existed.

So the skill here is routing. Pick the record that holds the answer, and
know what each one cannot tell you.

## Read-only, without exception

Every command in this document reads. Do not write to the production
database, do not `PATCH`, `PUT`, `DELETE`, `POST /api/runs`, stop or steer a
session, restart or recreate a container, or edit a file under `/data`.

The reason is specific rather than general caution: prod runs real agent
sessions, the event log is append-only and is the only authoritative record
of a run, and a session holds a workspace lease that a container restart
strands. A diagnosis that damages the evidence is worse than no diagnosis.

If the investigation concludes that something must be changed, say what and
why and stop there. Applying it is the user's next request, not part of this
one.

The one safe-by-construction habit that makes this easy: open the database
with `mode=ro` (the helper script below does), and never pass a subcommand
to `scripts/prod.sh` other than `logs`, `status`, `ps`, `exec` and `cp`.

### Do not read production secrets

Reading is not automatically harmless. `.env.prod`, the `settings` table's
values, and `harness config get -reveal` all hold live credentials — the
DeepSeek and Google API keys, the control token. A diagnostic session's
output gets pasted into tickets and chat logs, so pulling a key into the
transcript leaks it somewhere it will outlive the investigation.

None of these questions need the values. `harness config list` masks them,
the `settings` table can be selected by `key` alone, and the compose file
tells you which variables exist without their contents. If a credential
genuinely is the subject — "is the API key set" — confirm its presence and
say so, rather than printing it.

## Orient first

Two commands, always, before anything else. They cost nothing and they
change how you read everything after.

```bash
scripts/prod.sh status                       # containers, promoted image, health
curl -s http://127.0.0.1:8180/api/queue      # {available, halted, in_flight, consumer_lag}
```

`in_flight` above zero means a run is live right now. That matters twice
over: a live session's records are still being written, so a missing final
event means "not yet" rather than "lost", and its provider trace file is an
unterminated gzip stream (see below).

Prefer the narrowest tool that answers the question. An HTTP GET touches
nothing; the helper scripts below run one bounded command in the container
and exit. Opening an interactive shell inside the prod container to poke
around is a much wider blast radius for the same answer, and it is where an
accidental write comes from.

Prod is `http://127.0.0.1:8180` and compose project `deepseek-harness-prod`.
The dev stack on the same machine is `8080` / `deepseek-harness`. Getting
these crossed produces a confidently wrong answer about the wrong system, so
check which one you are on before quoting a number.

## Which record answers which question

Cost rises down this table. Start at the top row that can answer the
question and stop as soon as it does.

| The question | Record | Opening move |
| --- | --- | --- |
| What sessions exist, what status, what did they cost, what did they do | HTTP API | `GET /api/sessions` |
| What happened inside one run, turn by turn | HTTP API events, or the `session-review` skill | `GET /api/sessions/{id}/events?kind=…` |
| Aggregates: counts, spend, durations, error rates across many runs | SQLite store | `prodsql.py` |
| Is the queue backed up, is a request stuck, who holds a workspace | HTTP API | `/api/queue`, `/api/requests`, `/api/leases` |
| Failures with no session row: startup, workspace prep, worker pool, queue | Container logs | `scripts/prod.sh logs` |
| What was actually sent to or returned by the provider | HTTP trace | `trace.py` |

The two rows people reach for too late are the third and the fifth.

Any question containing a number over a population — how many, how much,
how often, what is the average — is a SQL question. Paging the sessions API
and adding the results up by hand is slower, and it silently truncates at
the page limit.

Any failure whose symptom is "the run never started", "the workspace was
wrong", "it died before producing anything" is a logs question. Those
failures happen outside the session lifecycle, so the event log has nothing
to say about them and its silence is not evidence.

## The HTTP API

Base `http://127.0.0.1:8180`. Read endpoints, all `GET`:

```bash
curl -s 'http://127.0.0.1:8180/api/sessions?limit=20'            # newest first; ?status=running|finished &q=<substring>
curl -s  http://127.0.0.1:8180/api/sessions/<sid>
curl -s 'http://127.0.0.1:8180/api/sessions/<sid>/events?from=0&limit=500&kind=error,tool_call'
curl -s  http://127.0.0.1:8180/api/requests                      # work requests and their delivery counts
curl -s  http://127.0.0.1:8180/api/leases                        # who holds which workspace
curl -s  http://127.0.0.1:8180/api/queue
curl -sN http://127.0.0.1:8180/api/sessions/<sid>/stream          # SSE, live run only
```

Lists come wrapped in a pagination envelope — `{"items":[…],…}` — even when
you asked for no page, so index `items` rather than treating the body as an
array. Events page at 500 and carry `has_more` and `next`; a run of any size
needs several pages, and stopping at page one is a common way to conclude
that something never happened.

`?kind=` is the single highest-leverage parameter on this API. The 14 kinds
are `session_started`, `turn_started`, `reasoning_delta`, `content_delta`,
`tool_call`, `tool_denied`, `tool_stdout`, `tool_result`, `usage`,
`turn_finished`, `run_finished`, `error`, `steer_message`, `steer_applied`.
A long run is mostly `reasoning_delta` and `tool_stdout` by volume; filtering
to `error,tool_denied,run_finished` turns a 30,000-event log into something
you can read in one page. An unknown kind returns a 400 that lists the valid
ones.

Event payloads served over HTTP pass through credential redaction — tokens
shaped like `ghp_`, `sk-`, `AIza`, `xoxb-`, `AKIA` become a placeholder. The
database and the on-disk mirror keep the literal bytes. So if you are
chasing whether a secret leaked into a prompt, the API will not show it and
its absence there proves nothing.

The browser view of a run is `http://localhost:8180/sessions/<sid>`. Offering
that link is often more use to the user than pasting transcript back at them.

## The SQLite store

One file, `/data/harness.db`, inside the `harness` container on volume
`deepseek-harness-prod_harness-data`. **There is no `sqlite3` CLI in the
image** — the driver is pure Go and only the Python module is present. Use
the helper rather than reconstructing the invocation:

```bash
python3 .claude/skills/prod-diagnostics/scripts/prodsql.py "
  SELECT status, count(*) FROM sessions GROUP BY status"

python3 .claude/skills/prod-diagnostics/scripts/prodsql.py --dev "SELECT …"   # dev stack instead
python3 .claude/skills/prod-diagnostics/scripts/prodsql.py --schema           # tables and columns
```

It opens the database `mode=ro` over a URI, which is what makes it safe to
run against a live writer, and it refuses anything that is not a `SELECT`
or a `PRAGMA`. Do not work around that by calling `docker … exec … python3`
yourself; the guard is the point.

Tables: `sessions`, `events`, `work_requests`, `workspace_leases`,
`settings`, `attachments`, `eval_runs`, `eval_members`. Run `--schema` for
columns rather than trusting memory — the schema is one Go constant plus a
list of `ALTER TABLE ADD COLUMN` migrations, so several live columns
(`plan`, `summary`, `complete_status`, `recent_tool_calls`, `title`,
`phase`) do not appear in the `CREATE TABLE` you would find by reading
`internal/store/store.go`.

Two facts about shape that save a wrong query. `events.payload` is JSON
text, so cost and token questions go through `json_extract` on the `usage`
events rather than any column on `sessions`. And timestamps are stored as
text, so date filtering is string comparison (`created_at >= '2026-08-01'`),
which works because the format sorts lexically.

`references/queries.md` holds worked queries for the questions that come up
most — spend by day, cost of one session, sessions by exit status, longest
runs, tool-call frequency, error hunting. Read it before writing a
non-trivial query; the joins between `sessions`, `work_requests` and
`events` are easy to get subtly wrong.

## Container logs

```bash
docker compose -p deepseek-harness-prod logs --tail 300 harness
docker compose -p deepseek-harness-prod logs --since 2h harness | grep sess-84df48
scripts/prod.sh logs                                    # follows harness
```

These are plain `log.Printf` lines, `2026/08/14 06:44:43 message`, not
structured JSON. There is no log level and no verbosity environment
variable, so there is nothing to turn up — what is there is all there is.

The convention is a package prefix rather than fields: `worker:`,
`harness serve:`, `session:`, `workspace:`, `httplog:`, `evals:`,
`httpapi:`. Session and request ids appear inline in the message text where
they appear at all, so correlate by grepping the raw id.

What logs hold that nothing else does: container start and configuration,
workspace preparation (clone, `npm ci`), worker pool and queue delivery,
and any panic. A run that failed before its session row existed leaves a
trace here and nowhere else.

Retention is whatever Docker's json-file driver kept. Nothing in the repo
configures rotation, and nothing archives these, so a question about last
month may simply be unanswerable — say so rather than inferring from
silence.

## The provider HTTP trace

Every HTTP call to the model provider is recorded: request headers and body,
response headers and body, status, timings, retry attempt. This is the only
record of what the model was actually shown, which makes it the right and
sometimes only tool for "did it get the tool schema", "what did the API
return when it failed", "why did this retry".

Files live at `/data/http/<yyyy-mm-dd>/<session-id>/exchanges.jsonl.gz` in
the harness container, gzipped JSON Lines, one object per attempt. Calls
made outside any session land under a `harness` directory instead of a
session id. Capture is on in prod by default.

```bash
python3 .claude/skills/prod-diagnostics/scripts/trace.py --list                 # days and sessions
python3 .claude/skills/prod-diagnostics/scripts/trace.py <sid>                  # one summary line per exchange
python3 .claude/skills/prod-diagnostics/scripts/trace.py <sid> --seq 12         # one exchange, full bodies
python3 .claude/skills/prod-diagnostics/scripts/trace.py <sid> --seq 12 --field req_body
python3 .claude/skills/prod-diagnostics/scripts/trace.py <sid> --out /tmp/tr    # copy the file out
```

Start with the summary. It is the whole point of the file for most
questions — status codes, timings and `attempt` numbers across the run tell
you where the failures and the retries were, and only then is it worth
opening a body.

Three things about these files that mislead if you do not know them.

A live session's file is an unterminated gzip stream, so `gzip -dc` writes
every flushed line to stdout and *then* reports `corrupted data` with a
non-zero exit. The lines are good. The script tolerates this; if you decode
by hand, do not read the error as "the file is broken".

The bodies are the raw wire format and they are large — a single request
body carries the entire conversation so far, so exchange 40 of a run
contains 39 turns of history. Never cat one whole. Pull the field you want.

Only the listed auth headers are redacted here. Bodies are recorded
verbatim, which is what makes this record useful and also what makes it the
one place a secret in a prompt would be visible. Treat anything you copy
out of it accordingly.

Nothing prunes this tree and nothing rebuilds it. It is a primary record,
not derived — `harness export` does not produce it. A session that made no
provider call has no file at all, so `--list` is the way to find out what
was captured rather than assuming a session id will be there.

## Joining the records

Four identifiers thread through everything:

- `session_id` (`sess-…`) — the session row, its events, its trace directory,
  its workspace path, and the log lines that mention it.
- `request_id` (`mcp-…` for MCP launches) — the work request, the
  `work_requests` row (`GET /api/requests/<request_id>`), and
  `sessions.request_id`.
- `workspace` — an absolute path, the key of a workspace lease.
- The day — trace directories are per UTC day, so a run that crossed midnight
  has its exchanges split across two directories.

The join that answers most "why did this go wrong" questions runs in one
direction: session row for what it was meant to do, `error` and
`run_finished` events for how it ended, logs around that timestamp for
anything the harness itself said, then the trace for the exchange that
matches. Going the other way — starting in the trace — is how an hour
disappears.

## Deep transcript reading

When the question is really "read this whole run and tell me what happened",
that is the **`session-review`** skill, not this one. It fetches the session,
splits the transcript at sub-turn boundaries, computes metrics, and applies a
method for attributing failures to the harness versus the model. It already
defaults to prod's `8180`.

```bash
python3 .claude/skills/session-review/scripts/fetch_session.py <sid> --out /tmp/review
```

Use this skill to find *which* session, and that one to read it.

## The queue

`GET /api/queue` is the first stop and usually the last. The queue is the
`work_queue` table in the harness's SQLite store — there is no broker to
inspect. The endpoint reports depth (claimable now), scheduled (Nak-delayed),
in flight (leased), and redelivered rows:

```bash
curl -s http://127.0.0.1:8180/api/queue | python3 -m json.tool
```

A result lives on the request's `work_requests` row, not in any stream, so a
result never ages out: `GET /api/requests/<request_id>` finds it however old
it is. Its session and events are in the same database — reach for those
when the question is what the run did.

## Reporting what you found

Answer the question that was asked, at the size it was asked. "What did we
spend last week" wants a table and a total, not a pricing model. Prod is
deep enough that there is always another layer to go down, and a casual
question returned as a ten-section report buries the answer the person
actually wanted.

Things you notice on the way that matter — a cost figure that is about to go
stale, spend attributed to the wrong provider — are worth raising, in a
couple of lines at the end, marked as beside the point. That is different
from expanding the investigation until it finds them.

Cite the record for every claim: the endpoint, the query, the log timestamp,
the trace `seq`. A production finding that cannot be re-derived by the person
reading it is a hypothesis, and it should be labelled as one.

Separate what a record shows from what you infer it means. "No `run_finished`
event and the process restarted at 04:12" is an observation; "the run was
killed by the restart" is an inference, and it is worth marking which is
which, because the fix differs.

Where a record simply cannot answer the question — logs rotated away, results
aged out, redaction hiding the field — say that plainly. An unanswerable
question reported as unanswerable is a good outcome; one answered from
silence is not.
