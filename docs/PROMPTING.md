# Prompting V4 flash and pro

External research, gathered 2026-08-09 from the web and GitHub. Scope is how to
write the system prompt, tool descriptions, and instructions — not the request
path, which [DESIGN.md](DESIGN.md) covers.

Three tiers of confidence, marked throughout. Measured means
[OBSERVED.md](OBSERVED.md) or the vendored docs. Reported means a named
implementation whose author ran the model. Claimed means a blog post with no
published method; several of these publish percentages without saying how they
were obtained, so their numbers are omitted here and only the direction of the
claim is kept.

## In thinking mode the prompt is the only lever

Measured. Two independent restrictions land in the same place.

`temperature`, `top_p`, `presence_penalty` and `frequency_penalty` are accepted
and ignored in thinking mode. No error is raised
(`guides/thinking_mode.md`). Separately, `tool_choice: required` and the named
form return 400 in thinking mode, so a tool call cannot be forced
([OBSERVED.md](OBSERVED.md)).

The main loop runs thinking-on permanently. It therefore has no sampling knob
and no way to compel an action. Every behaviour we want has to be obtained from
the wording of the system prompt and the tool descriptions.

This also invalidates most prompting advice found online. Nearly all of it opens
with a temperature table — 0.0 for code, 1.3 for chat, 1.5 for creative. That
advice only applies with `thinking: {"type": "disabled"}`, which for us means
the side conversations described in [MODELS.md](MODELS.md) and nothing else.

## Tool definitions are prompt text

Measured, via the published chat template. The model's native tool format is
DSML, and the template renders the `tools` array into the prompt itself:
`<｜DSML｜invoke name="$TOOL_NAME">`, with parameters marked `string="true"`
carrying raw strings and everything else JSON
([V4-Pro encoding README][enc]).

Three consequences.

Tool descriptions are prompt engineering, not metadata. They occupy the same
cached head as the system prompt, and one small tool schema measured about 280
prompt tokens ([OBSERVED.md](OBSERVED.md)).

The system message sits immediately after the BOS token with no delimiter
([V4-Pro encoding README][enc]). Nothing separates it from the tool block, so
the ordering discipline in [CACHE.md](CACHE.md) governs both.

`drop_thinking` defaults to enabled and is automatically disabled when tools are
present ([V4-Pro encoding README][enc]). That is the template-level cause of the
reasoning round-trip rule: once tools exist, history is retained in full.

## DSML can leak into content

Reported, and not on our build. Tool calls sometimes arrive as DSML markup
inside the assistant `content` string rather than parsed into `tool_calls`
([HF discussion 209][hf209], [OpenCode 24566][oc24566]). Neither report pins a
server build.

Ours does not do it. Searched on 2026-08-13 across every production response
body for 10–13 August — 83 sessions, 10,542 exchanges, 11,766 tool calls — for
`DSML` in both its fullwidth and ASCII-pipe forms, for the older
`<｜tool▁calls▁begin｜>` special-token run, and for leaked chat-template markers
(`<｜User｜>`, `<｜Assistant｜>`, `<｜end▁of▁sentence｜>`, `<think>`). No response
contained any of them; every tool call arrived structured. Re-check if the
server build in [OBSERVED.md](OBSERVED.md) changes.

A guard is still cheap if it ever appears: if `content` contains `<｜DSML｜`,
treat the turn as suspect rather than surfacing markup to the user. What we
actually see go wrong in assembled tool calls is a misplaced brace, which
[OBSERVED.md](OBSERVED.md) measures and `deepseek.RepairArguments` handles.

## Effort mapping wastes two of five levels

`guides/thinking_mode.md` gives the mapping. The 2026-08-13 GA release made it
one table for both models:

| Requested | Actual |
| --- | --- |
| low | low |
| medium | high |
| high | high |
| xhigh | high |
| max | max |

`medium` and `xhigh` both land on `high`, so three settings out of five reach it.

Our own measurement predates this. On the preview build, pro collapsed `low`
into `high` — 12397 against 12196 total tokens, inside 1%
([OBSERVED.md](OBSERVED.md)) — which is the behaviour the new table drops. Lowering
effort on pro should now buy something, but we have not measured it.

## Flash against pro

Reported and claimed, not yet measured by us.

Flash needs explicit output anchors. Several sources say it drifts into preamble
unless the prompt names the first token of the answer, e.g. "Begin with: ##"
([deepseekai.guide][dsg]). Pro is said to tolerate fewer examples for the same
result ([Lightrains][lr]).

Flash may answer in Chinese. A harness author notes the model tends to speak
Chinese unless the system prompt contains an explicit English instruction
([HologramSteve/deepseek-harness][hs]). Our English anchor is much heavier than
theirs, so the risk is lower than it reads; see
[language-interaction-investigation.md](language-interaction-investigation.md).

The retrained flash build (0731) redid post-training only — same base model,
same size — and the gains claimed are in alignment and agent behaviour
([DeepSeek blog][flashga]). If that holds, prompting advice written against
flash-preview may be stale in exactly the area we care about.

## Technique claims we have not tested

All claimed. Listed because they are cheap to test, not because they are
established.

| Claim | Source |
| --- | --- |
| Negative constraints ("don't hallucinate") have no measurable effect | [Lightrains][lr] |
| Paired counterexamples ("Wrong: X / Right: Y") outperform "avoid X" | [deepseekai.guide][dsg] |
| Persona prompts ("you are a witty developer") reduce consistency | [Lightrains][lr] |
| JSON mode needs the literal word "json" plus a schema example, not a description | [deepseekai.guide][dsg] |
| A self-check step before the final answer reduces unsupported claims | [Lightrains][lr] |
| Section headers and end-of-prompt constraint recaps help at long context | [Lightrains][lr] |

The JSON-mode item is the least relevant to us. Forcing a named tool in a
thinking-off side conversation is a better structured-output mechanism than
`response_format`, for the reasons in [OBSERVED.md](OBSERVED.md).

## Where external sources contradict our measurements

Ours win. Recorded so the same claims are not re-imported later.

Cache block size. [HenryZ838978/deepseek-harness][hz] reports 256-token
alignment over 24 trials. We measured 128 exactly, at five prefix sizes on flash
and two on pro ([OBSERVED.md](OBSERVED.md)). Possibly a different server build.

The reasoning round-trip 400. The same repo quotes the error verbatim, and the
vendored docs prescribe it. It did not reproduce on either model across eight
configurations on build `prod0820_fp8_kvcache_20260402`
([OBSERVED.md](OBSERVED.md)). We keep replaying reasoning anyway; there is just
no cliff to engineer around.

Double-billing of reasoning tokens. [deepseekai.guide][dsg] says returning
`reasoning_content` without a tool call means paying for it twice.
`guides/thinking_mode.md` says the API ignores it. One of them is wrong, and
`prompt_tokens` settles it in a single request.

Thinking off by default. [HenryZ838978/deepseek-harness][hz] makes this a
normative rule, on the grounds that trivial prompts still burn 30–300 reasoning
tokens. We measured 49 reasoning tokens for a two-token answer, which agrees
with the observation. It is a cost argument, not a correctness one, and
[MODELS.md](MODELS.md) already decides this for us.

## Implementations worth reading

[antirez/ds4][ds4] is the most interesting for our design. It stores
`tool id -> exact sampled DSML block` and replays the model's own bytes when
tool results come back, rather than re-rendering a canonical approximation. For
a cache-sensitive harness that is a real idea: a re-rendered tool call is a
mutated prefix, and the mutation sits in the middle of the conversation where
[CACHE.md](CACHE.md) says it costs most.

[HenryZ838978/deepseek-harness][hz] is the closest prior art — 16 protocol
quirks over 12 probes and 270+ trials, and it reports that the flash and pro
protocol contracts are identical across all of them. Our own pro-against-flash
table agrees as far as it goes.

[Composio's harness comparison][comp] ran flash through four harnesses on 30
agentic tasks. Success 14–17 of 30, cost per success $0.073 to $0.195, median
task time 122s to 272s. Seven tasks flipped on harness choice alone. No single
harness won on more than one metric.

The ecosystem breakage is worth a skim for failure modes we might inherit:
[reasoning_effort stripped before it reaches the API][ll27439],
[`reasoning_effort: "none"` rejected where `thinking: {"type": "disabled"}` is
required][ow1260], and [reasoning_content not round-tripped][oc24190].

## Worth testing

In rough order of what would change a decision.

Whether output anchors measurably reduce preamble on flash. Whether paired
counterexamples beat plain prohibitions in the system prompt. Whether
`reasoning_content` returned outside a tool call is billed. Whether DSML ever
leaks into `content` on our build.

Everything about prompt and reasoning language moved to
[language-interaction-investigation.md](language-interaction-investigation.md),
which specs the experiment and prices it.

[enc]: https://huggingface.co/deepseek-ai/DeepSeek-V4-Pro/blob/main/encoding/README.md
[hf209]: https://huggingface.co/deepseek-ai/DeepSeek-V4-Pro/discussions/209
[oc24566]: https://github.com/anomalyco/opencode/issues/24566
[oc24190]: https://github.com/anomalyco/opencode/issues/24190
[ll27439]: https://github.com/BerriAI/litellm/issues/27439
[ow1260]: https://github.com/OpenWhispr/openwhispr/issues/1260
[hz]: https://github.com/HenryZ838978/deepseek-harness
[hs]: https://github.com/HologramSteve/deepseek-harness
[ds4]: https://github.com/antirez/ds4
[comp]: https://x.com/composio/status/2085330847951970801
[flashga]: https://deepseek.ai/blog/deepseek-v4-flash-ga-agent-benchmarks
[lr]: https://lightrains.com/blogs/deepseek-prompt-engineering-best-practices/
[dsg]: https://deepseekai.guide/tutorials/deepseek-prompt-engineering/
