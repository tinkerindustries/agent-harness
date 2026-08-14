# Prompting Gemini 3.5 Flash for Web UI Screenshot Review

Use case: a coding agent takes screenshots of a web page under development and
sends them to a vision model to diagnose layout/design issues, feeding the
diagnosis back into the coding loop for fixes.

## Why Gemini 3.5 Flash / 3.5 Flash-Lite

- Cheap, fast, strong at UI/screenshot/document understanding specifically
  (vs. general photo quality).
- 1M token context window — room for multiple screenshots + spec/CSS per call.
- `gemini-3.5-flash` = higher quality, higher cost (~$1.50/$9 per 1M tokens).
- `gemini-3.5-flash-lite` = the actual budget option for high-volume,
  repeated calls — start here if this runs many times per session.
- `gemini-3.6-flash` = newer, improved computer-use/OSWorld and multimodal
  document/chart parsing scores over 3.5 Flash — worth benchmarking if 3.5
  misses layout bugs.

## Key differences from prompting older models (2.5 Flash, GPT-4o, etc.)

Gemini 3.x are reasoning models. Google's own migration guidance:

- **Be concise, not verbose.** Chain-of-thought scaffolding ("think step by
  step", long reasoning instructions) written for older models can cause
  the model to over-analyze. Strip it out.
- **Don't set `temperature`, `top_p`, `top_k`.** No longer recommended for
  Gemini 3.x — the model's reasoning is tuned for defaults. For consistency,
  use explicit rules in a system instruction instead.
- **Default output is less verbose** than older Gemini models. If you want
  conversational tone, ask for it explicitly.

## Control reasoning depth: `thinking_level`

Replaces the old `thinking_budget` parameter (still works but deprecated;
don't mix the two in one request — it 400s).

| Level | When to use |
|---|---|
| `minimal` | Fast, simple checks. Chat-like, quick factual answers. |
| `low` | Lower latency/fewer steps. Good for simpler diagnostic passes. |
| `medium` (default) | Best quality for most tasks — recommended starting point. |
| `high` | Complex reasoning — subtle bugs, hard-to-spot layout issues. |

For this use case:
- Quick "does this roughly match" pass → `low`
- Real diagnostic pass on a broken layout → `medium` or `high`

Start at `medium` (the default) and only drop to `low` once you've confirmed
quality holds up for your actual screenshots.

## Image resolution — matters a lot for UI work

Gemini 3 supports per-image `resolution` settings: `low` / `medium` / `high`
/ `ultra_high`.

| Media | Recommended | Max tokens | Notes |
|---|---|---|---|
| Images | `high` | 1120 | Recommended for most image analysis tasks — this is close to the default anyway. |
| PDFs | `medium` | 560 | Quality saturates at medium; `high` rarely helps standard docs. |

- Default (`unspecified`) already resolves to the equivalent of `high` for
  images, so you likely don't need to change anything for a single
  screenshot per call.
- If sending **multiple** screenshots per call (before/after, multiple
  breakpoints, reference design + actual render), set `high` only on the
  one you need scrutinized and drop the others to `medium`/`low` to save
  tokens.
- `ultra_high` exists but is mainly for computer-use / cases where testing
  shows a clear benefit over `high` — not a default choice.

## Prompt structure: data first, question last

Google's guidance: when working with large context (docs, code, images),
put the data first and the specific question at the end, anchored with a
phrase like "Based on the preceding information...".

For this use case: send the screenshot(s) and any design spec / target
CSS first, then ask the diagnostic question last.

## Practical prompt skeleton

```
[system instruction]
You are reviewing a web page screenshot against its intended design.
Be precise and concise — flag concrete issues (element name, expected vs
actual position/size/color/spacing), not general impressions.
Output as a JSON list: [{ "element": "", "issue": "", "expected": "", "actual": "" }]

[input: screenshot, resolution=high]
[input: design spec / target CSS, if available]

Based on the preceding screenshot and spec, identify all visual
discrepancies. Only report issues you can point to specifically.
```

Ask for structured JSON output rather than free-text prose — makes it
trivial to feed straight into the coding agent as a task list without a
separate parsing step.

## `response_format` is a type, not a schema — measured

Everything above this line is the vendor's advice. This section is what the
live API actually did on 2026-08-14, and it wins where the two disagree (the
same rule [OBSERVED.md](OBSERVED.md) states for DeepSeek).

`response_format` is a top-level request field carrying a single `type`. There
is no schema field beside it on this surface, and that turns out to matter:
the type constrains the *shape of the container* and tells the model nothing
about what goes in it. One image, one instruction asking for
`{"observed": ..., "elements": [...]}`, three requests differing only in this
field:

| `response_format` | What came back |
| --- | --- |
| `{"type":"object"}` | `{ }` — a literally empty object, 5 output tokens |
| `{"type":"array"}` | A JSON array, but the object inside it mangled: `elements` came back as a list of the key *names* (`["image","text","role","styling"]`), and the whole entry was duplicated |
| *omitted* | Exactly the requested object, fully populated and correct — wrapped in a ```` ```json ```` fence |

So asking for `"object"` is worse than asking for nothing: it produces a
well-formed answer with nothing in it, which is the single most expensive
failure this pipeline has (a caller that cannot see the image cannot tell an
empty answer from a correct one — [TOOLS.md](TOOLS.md), "Every answer says
what it saw"). `"array"` is what the earlier bare-list shape used, and it
worked only because that shape *was* a top-level array; it does not survive
the move to an object.

`internal/tools/reviewscreenshot.go` therefore sends no `response_format` at
all and strips the fence. Letting the model choose its own container and
paying three backticks for it is a far smaller problem than constraining the
container and losing the contents. Re-measure before reintroducing the field —
a schema parameter appearing on this endpoint would change the answer.

## Reducing unnecessary tool calls (if using function calling / agentic loop)

If the model over-uses tools in an agentic setup:
1. Lower the thinking level first (`medium` → `low` → `minimal`) — higher
   thinking levels encourage more exploratory tool use.
2. If that's not enough, add an explicit system instruction:
   `"You have a limited action budget of <n> tool calls. Use them efficiently."`

## Known limitations

- **No image segmentation** in Gemini 3.x. If the pipeline ever needs
  bounding boxes / pixel masks rather than described issues, use
  Gemini 2.5 Flash (thinking off) instead — 3.x doesn't support it.
- **Text-only output.** Multimodal input only; it won't generate images,
  annotated screenshots, etc.
- **Knowledge cutoff January 2025** — irrelevant for this use case since
  it's working from what you send it, not general knowledge.

## Things to verify empirically before locking in the pipeline

- Whether `low`/`medium` thinking catches subtle layout bugs as reliably as
  `high` on your actual UIs — varies by how cluttered/component-heavy pages
  are.
- Whether structured JSON output vs. free-text diagnosis changes quality —
  worth a quick A/B on 5–10 real screenshots.
- `gemini-3.5-flash-lite` vs `gemini-3.5-flash` vs `gemini-3.6-flash` on your
  actual screenshots — pick the cheapest one that doesn't miss things.

## Sources

- https://ai.google.dev/gemini-api/docs/whats-new-gemini-3.5
- https://ai.google.dev/gemini-api/docs/media-resolution
