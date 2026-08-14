# Ground truth, captured 2026-08-14 from deepseek-harness-prod

These are the facts the graders check against. Re-derive them before a later
iteration — prod keeps running, so the spend figures and session counts move.

## Eval 0 — failed-run-diagnosis

The only session with `status = 'failed'` on 2026-08-12:

| Field | Value |
| --- | --- |
| id | `sess-be798f5cb15f1be0a2151328e4fe67f3` |
| status | `failed` |
| complete_status | empty |
| created_at | 2026-08-12T14:02:13 |
| finished_at | 2026-08-12T14:33:14 (≈31 minutes) |
| model | deepseek-v4-flash |

Its one `error` event: `session: sub-turn 220: deepseek: stream read: unexpected EOF`

Event counts confirm where it stopped: `turn_started` 220, `turn_finished`
219, `usage` 219 (max `sub_turn` 219), `tool_call` 245, `tool_result` 245.
So sub-turn 220 opened and never completed.

Attribution: the provider connection dropped mid-stream. That is transport,
not a model mistake and not a harness logic bug.

The distractor: `sess-b48818ba872aca823547c7c30e2ee106` is `cancelled` on
2026-08-13 with `session: sub-turn 13: context canceled`. Different day,
different cause. An answer that names this one is wrong.

## Eval 1 — spend-aggregate

Spend by day, `sum(json_extract(payload,'$.cost_usd'))` over `usage` events
joined to session `created_at`:

| Day | USD |
| --- | --- |
| 2026-08-14 | 2.3777 |
| 2026-08-13 | 1.1858 |
| 2026-08-12 | 3.4370 |
| 2026-08-11 | 2.1652 |
| 2026-08-10 | 3.6753 |
| 2026-08-09 | 0.0754 |

Total across all `usage` events: **12.9211**.

Most expensive single run: `sess-b949743ff7766606eb210ae59f2c1bcd`,
**$0.3828** over 286 sub-turns, status `ok`. Runners-up
`sess-208dc1e5d8a7d66a68499c7274c8dff3` ($0.3773) and
`sess-1e461dc3d39fe95d3a75bbd7aa3a3374` ($0.3538).

Prompt cache hit rate, `prompt_cache_hit_tokens / prompt_tokens`: **99.5%**
overall, 98.8–99.6% per day. The cache is working; that is the answer to
"is it doing anything for us".

Session statuses overall: ok 105, max_turns 3, failed 2, running 1,
cancelled 1 (112 total).

## Eval 2 — provider-wire-inspection

`sess-fc5403c722171ac60de8b1b0f8d885c5`, trace at
`/data/http/2026-08-14/sess-fc5403c722171ac60de8b1b0f8d885c5/exchanges.jsonl.gz`:

- **166 exchanges, 0 failed, 0 retries.** Every `attempt` is 0, every
  `status` 200.
- The 166 span **two hosts**: 165 to `api.deepseek.com/chat/completions` and
  one to `generativelanguage.googleapis.com/v1beta/interactions` at `seq 148`,
  the vision call behind `ReviewScreenshot`. Both eval runs found this. The
  first version of `trace.py` truncated long URLs from the left, which made
  the Gemini row read as a DeepSeek one; the summary now prints host and path
  in separate columns and adds a host breakdown when there is more than one.
- Exchange `seq: 1` — `POST https://api.deepseek.com/chat/completions`,
  ttfb 647ms, total 2393ms, started 2026-08-14T02:21:22Z.
- `req_body` (44,393 bytes) opens
  `{"model":"deepseek-v4-flash","messages":[{"role":"system","content":"You are a headless coding agent. You work inside one workspace directory for the whole session and finish tasks by editing files…`
- `req_headers` shows `"Authorization": "[redacted]"` — the capture redacts
  auth headers, bodies are verbatim.
- `resp_body` is SSE (`data: {"id":…,"object":"chat.completion.chunk"…`).

The tool list is in the `tools` array of that same `req_body`. Distinct tools
called across prod: 14.
