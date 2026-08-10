---
name: deepseek-harness-wait
description: How to wait for a deepseek-harness agent run to finish without spending a conversation turn per check — poll the status endpoint in the background at an interval matched to the work, then collect the result and verify the parts that are cheap to verify. Use this whenever a harness run is in flight and you need its outcome: right after deepseek_agent hands back a request_id, when the user asks "is it done yet", "check on that run", "did deepseek finish", "what is it working on", "what happened with that job", or when you notice you are about to call deepseek_result a second time. Reach for it even when the user never names it — any time you catch yourself polling for a harness run's status, this is the skill. Also use it when a run seems stuck, when you have lost the request_id, or when a result has come back and you are about to relay what the agent claims it did.
---

# Waiting for a harness run

`deepseek_agent` returns the instant the request is published to the queue. Everything after that is waiting, and waiting is where an otherwise good run gets spoiled.

Two failure modes, pulling in opposite directions. Polling from the main loop spends a whole conversation turn on each check, so a twelve-minute run becomes twenty messages of "still running, sub-turn 22" that the user has to scroll past — and they will interrupt you, correctly, to ask why you are doing it that way. Going quiet is worse: the user cannot distinguish waiting from stuck from forgotten, so they have to ask.

What you want is for something other than the conversation to do the waiting, and to wake you when it is over.

## Hand the wait to the harness

Background Bash is that something. A command launched with `run_in_background` keeps running across turns and re-invokes you when it exits — so a loop that polls until the status changes and then exits is a single turn that ends exactly when the run does. Foreground `sleep` is blocked precisely so you reach for this instead.

The status lives behind a read-only HTTP endpoint, keyed on the request id you already hold:

```bash
RID=mcp-...                  # request_id from deepseek_agent
BASE=http://127.0.0.1:8180   # from the transcript_url it returned

for i in $(seq 1 90); do
  J=$(curl -s "$BASE/api/requests/$RID/status")
  S=$(printf '%s' "$J" | python3 -c "import json,sys; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
  if [ -n "$S" ] && [ "$S" != "running" ]; then
    echo "FINISHED after $((i*30))s with status=$S"
    printf '%s' "$J" | python3 -m json.tool
    exit 0
  fi
  sleep 30
done
echo "STILL RUNNING after 45 minutes"
```

Read the base URL off the `transcript_url` that `deepseek_agent` returned rather than guessing — that is what tells you whether this run went to production (`8180`, MCP server `deepseek-harness`) or dev (`8080` by default, `HARNESS_HTTP_PORT` can move it, MCP server `deepseek-harness-dev`). Getting this wrong means polling a harness that has never heard of your request.

Three details in that loop are load-bearing:

- **Key on the request id, not the session id.** The request row exists from the moment the pool claims the message, before the workspace is built and before any session exists. A clone that is refused therefore reports `status: failed` with `error_code: workspace_setup` rather than leaving you to infer death from silence. Poll by session id instead and a request that died in setup looks identical to one that has not started yet.
- **The iteration cap.** A loop with no ceiling can outlive the thing it is watching. When it expires it prints a line you can recognise, so you find out rather than assuming success.
- **`status` is the only field you need to decide when to stop.** It is one of `running`, `ok`, `failed`, or `timeout`. The rest of the payload — `sub_turn`, `todos`, `active_form`, `tool_calls` in flight, `usage`, `duration_ms` — is there for free whenever you want to say something specific about progress.

A 404 means the request id is genuinely unknown, which is a mistake to report rather than wait out.

## Choose the interval from the work, not from impatience

A harness sub-turn takes seconds to a couple of minutes, and a whole run typically five to twenty. Thirty seconds is a sensible check against state that moves on that scale. Below about fifteen you are querying SQLite hundreds of times to learn nothing, and since the background loop costs you no turns, a tighter interval buys only marginally faster notification.

Do not schedule a wakeup to poll for work the harness already tracks for you. The background command's exit is the signal; adding a timer on top is duplicated effort that can only race with it.

## Use the waiting time

The run is going to take minutes whether you watch it or not. Before you settle in, give the user the transcript URL — a user who can watch does not have to wait on you. Then do the work that does not depend on the outcome: read the code the run is about to change, prepare the verification commands you will run against the branch, or get on with an unrelated part of the task.

If the run passes roughly ten minutes with no result, say where it has got to and ask whether to keep waiting. The run continues either way; you are checking that the user still wants to spend the time, not asking permission to keep waiting.

## Which interface for which job

Using the wrong one is what makes waiting expensive:

| | Reach for it when | Not for |
| --- | --- | --- |
| `GET /api/requests/{id}/status` | The wait itself. Free, instant, costs no turn, and carries the todo list and what is in flight. | Nothing, really — it is the right default for anything automated. |
| `deepseek_status` | The user asks what the run is doing and you want it in the conversation. Same data, no wait parameter, returns immediately. | A loop. It is still a turn per call. |
| `deepseek_result` | The outcome, once the run is terminal: the schema-validated report — branch SHAs, commits, PR URL, files changed, per-command verification — plus `error.code`. | Progress. It answers with a short pending line and points you at `deepseek_status`. |
| `harness://session/{id}/transcript` | Reading what the run actually did, tool call by tool call, when the report is thin or does not add up. | Waiting. It pages the whole event log to build the markdown. |
| `harness://sessions` | Finding a run that `deepseek_runs` does not have. This is the harness's own history rather than one process's memory. | The state of a run you already have an id for. |

So: poll the endpoint, call `deepseek_status` when a human asks, call `deepseek_result` once at the end, and read a resource only when you are digging into a specific run.

Nothing here blocks. `deepseek_result` keeps a 300 ms floor on its fetch, which is not a wait for the run — a JetStream fetch needs a non-zero window to notice a message already sitting in the stream, and without it an already-finished run would report as queued.

## Collecting the result

`deepseek_result` only reads the results stream and the read-only API, so it is safe to call more than once, and it will still find the result later — from a different session, hours afterwards. There is no window you can miss, which is another reason not to hover.

It inlines at most 4000 characters of the run's answer. When it truncates, the footer names the `harness://session/{id}/transcript` resource, which holds the whole thing.

If you have lost the `request_id`, `deepseek_runs` lists what this MCP server process launched, with two limits on it. The state beside each row is a snapshot from the last time `deepseek_agent` or `deepseek_result` touched that entry, so a run that finished ten minutes ago still reads `running` — the row gives you the id to call `deepseek_result` with, not the outcome. And the list is one process's memory: a run launched before an MCP restart, or from another editor's server against the same harness, is simply absent. `harness://sessions` has those.

Never re-run `deepseek_agent` because a run seems to have vanished — that launches a second agent against the same branch, and you will not find out until two PRs appear.

## Read the outcome honestly

`status` and `complete_status` answer different questions, and a run can be green on one and failed on the other:

| Signal | What actually happened |
| --- | --- |
| `status: ok`, `complete_status: done` | Finished on its own terms. Read `result.error` anyway — it is the field most likely to contradict a cheerful summary. |
| `status: ok`, `complete_status: gave_up` | Followed the protocol and failed the task. Lead with `result.error`. |
| `complete_status: ""` | Stopped without calling `Complete`, so there is no structured result. Say that plainly rather than reconstructing one from the final assistant text. |
| `status: timeout` | Hit the sub-turn budget or the deadline. Check for a pushed branch — partial work often survives. |
| `status: failed`, `error.code: workspace_setup` | A clone was refused or the base branch does not exist. Nothing ran. |

Then check the claims that are cheap to check, because the agent is reporting on itself. `pushed: true` with an empty `pull_request_url` means the branch landed and the PR did not, and the reason belongs in what you tell the user. If the report says the branch was pushed, `git fetch origin <branch>` settles it; `gh pr view <url>` settles the PR. A verification command in the report is a claim until you have run one of your own.

## What to tell the user

While waiting: where it has got to — `active_form` is written for exactly this — and the transcript URL. When it lands: what the run actually did, what it says it left undone (`deferred`), what its own `error` field says, and which of its claims you confirmed independently. A summary that repeats the agent's self-assessment without checking it is not a report, it is a relay.

## Compatibility

`GET /api/requests/{id}/status` and `deepseek_status` arrived in v0.5.0, which also removed `wait_ms` from `deepseek_result`. Against an older harness, poll `GET /api/sessions/{session_id}` instead and treat a session that never appears as a probable setup failure worth asking `deepseek_result` about.

## Related

`deepseek-flash-task` covers the other half — writing the brief and launching the run. This skill picks up at the `request_id`.
