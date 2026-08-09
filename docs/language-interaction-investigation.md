# Language interaction — deferred investigation

Opened 2026-08-09, not scheduled. Whether prompting or reasoning in Chinese is
worth anything to this harness, and what it would cost to find out.

Deferred rather than dismissed. The cost side is understated in the external
sources, because none of their authors had to read a language they do not speak.

Confidence tiers as in [PROMPTING.md](PROMPTING.md): measured, reported,
claimed.

## The question, split

Two axes, independently controllable. The model can think in Chinese and answer
in English, or the reverse ([DeepSeek-V3 1255][i1255], reported).

Thinking language governs `reasoning_content`, which is where nearly all our
output tokens go. Output language governs `content`, which is what reaches the
transcript and the result stream.

Only the first axis is interesting. Answers must stay in English regardless, so
there is no version of this where output language is the thing we change.

## What we know

Context sets the thinking language, not an explicit setting. Environment
alignment accounts for roughly 70–90% of the effect ([DeepSeek-V3 1255][i1255],
reported). Our system prompt, eleven tool descriptions, tool results, file paths
and source all arrive in English, and a single tool schema measured about 280
prompt tokens ([OBSERVED.md](OBSERVED.md), measured). The English anchor is
already 2–3K tokens before the first user message.

The reported Chinese drift is therefore unlikely to affect us. The one harness
author who hit it was wrapping a short system prompt with no tool block
([HologramSteve/deepseek-harness][hs], reported). Unprompted Chinese output was
also filed against V3 as non-deterministic on both web and API, then closed as
stale with no confirmation for V4 ([DeepSeek-V3 1226][i1226], reported).

Instructions aimed at thinking outlast instructions aimed at output. Three
conditions at temperature 0 on flash ([DeepSeek-V3 1255][i1255], reported):

| Condition | Result |
| --- | --- |
| No instruction | stable English |
| "Write a Chinese summary first" | drifted back to English at ~1600 lines |
| "Think in Chinese" | zero drift over 2000+ lines |

That result is worth keeping whatever we decide about Chinese. It says
long-context instruction stability depends on whether the instruction steers the
reasoning or the answer, and our system prompt is full of instructions that
steer the answer.

The prize is a token reduction of 20–40% on reasoning, usually without accuracy
loss (claimed, and from R1-era work rather than V4). Reasoning runs about five
to one against the answer on real work, and produced 49 of 51 completion tokens
on a trivial prompt ([OBSERVED.md](OBSERVED.md), measured). Reasoning is
substantially the whole output bill, so the claim implies 15–35% off output
spend if it survives on V4.

## Why it is deferred

The saving is real money and the test is cheap. The cost that is not cheap is
the one no source accounts for.

Nobody here reads Mandarin. A Chinese reasoning trace is unreadable in the live
transcript, and the read-only web UI streams `reasoning_content` by design
([DESIGN.md](DESIGN.md)). Every debugging session that currently starts by
reading what the model was thinking would first need a translation pass. That is
a permanent tax on the main diagnostic surface of the harness, paid on every
incident, against a one-off percentage saving on output tokens.

Two costs that turn out not to apply, worth recording so they are not
re-litigated:

Translating the system prompt is probably unnecessary. The winning condition in
the experiment was one English sentence. Full localisation of the prompt and
tool descriptions was the hermes-agent proposal ([hermes-agent 35321][i35321]),
not the thing that was shown to work.

Maintenance drift is therefore not in play either. Had we translated eleven tool
descriptions, each edit would need re-translation, and a mistranslated tool
description is a behaviour change nobody here could audit. One appended English
sentence has none of that exposure.

## The experiment, when we run it

Cheap enough to fold into an existing measurement session. No translation
required to run it.

Take one realistic coding task from the [OBSERVED.md](OBSERVED.md) set — the SSE
reader is the right size, at 10844 reasoning tokens against 2191 of answer. Run
it on flash at fixed effort, twice: unmodified, then with a single appended
sentence instructing the model to think in Chinese. Compare
`completion_tokens`, the reasoning-to-answer split, wall clock, and whether the
answer is still correct and still in English.

Three runs each, because reasoning volume varies. Pro only if flash shows
something, since pro cannot be made cheap by effort and the absolute saving
would be larger there.

## What would have to be true to adopt it

The saving has to hold on V4 at a size that justifies the reading cost. Below
about 20% I would not take it at any price. Above that it becomes a real
trade.

Answer quality and answer language both have to be unaffected. A Chinese answer
leaking into the result stream is a correctness bug, not a cosmetic one.

There has to be an escape hatch. Thinking language would need to be per-session
rather than global, so a session under investigation can be re-run in English.

If all three hold, the shape is probably: Chinese reasoning on unattended queue
work where nobody is watching the transcript, English everywhere a human is
present. That splits the cached prefix in two, which [CACHE.md](CACHE.md) says
is not free, and the split would have to be priced before committing.

## Open questions

Whether the 20–40% figure survives on V4 at all. Whether Chinese reasoning
changes tool-selection behaviour, which the token count would not reveal.
Whether the reported value-set and cultural-norm differences between prompt
languages ([Thoughtology][thought], claimed) touch anything in a coding harness,
which I doubt but have not checked.

[i1255]: https://github.com/deepseek-ai/DeepSeek-V3/issues/1255
[i1226]: https://github.com/deepseek-ai/DeepSeek-V3/issues/1226
[i35321]: https://github.com/NousResearch/hermes-agent/issues/35321
[hs]: https://github.com/HologramSteve/deepseek-harness
[thought]: https://arxiv.org/pdf/2504.07128
