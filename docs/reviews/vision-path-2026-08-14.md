# Vision path review — two prod sessions, 2026-08-14

Two production runs that leaned on `Screenshot` and `ReviewScreenshot` hardest,
read end to end for one question: does the Gemini round-trip actually help a
model that cannot see?

| | A — `sess-c67a1c7536b5e42bea2939bf21883d19` | B — `sess-39a0fa2880c360b4c8397305e90adff9` |
| --- | --- | --- |
| Task | Session title/description/phase fields through to the list UI | Kimi in the model dropdown, inline tool-result images |
| Model / effort / mode | deepseek-v4-flash / max / full | deepseek-v4-flash / max / full |
| Outcome | ok | ok |
| Sub-turns | 246 | 153 |
| `Screenshot` calls | 8 | 0 |
| `ReviewScreenshot` calls | 8 | 4 |
| Total cost | $0.3301 | $0.1404 |
| Gemini share of that | **$0.1288 — 39%** | **$0.0335 — 24%** |
| Cost per review call | $0.0161 | $0.0084 |
| Cost per DeepSeek sub-turn | $0.00082 | $0.00070 |

**Verdict.** The capability is real and it caught things nothing else in the
harness could. But it is built as a *defect detector* and the model keeps
needing an *observer* — "what does this actually show?" — and the tool
structurally refuses that role. Three of twelve calls carried information the
run acted on. The other nine were empty arrays the model would not trust, or
re-asks provoked by them, at roughly 15× the price of a sub-turn.

---

## 1. `[]` is unfalsifiable, and the model says so in its own words

Session A, sub-turn 198: a full-page review of the session list against a spec
returns `0 findings`. The model's reasoning at 199:

> The reviewer reports zero findings — but I can't see the image myself, and the
> "0 findings" answer is only meaningful if the page actually rendered the
> content. […] 0 findings could also mean "nothing rendered at all".

It re-asks the same conversation ("Does the screenshot actually show the harness
session list with content? […] If the page is blank or shows an error, say so
explicitly") and gets `[]` again — because a blank-page question has no place in
a findings schema. At 200 it falls back to inferring the page rendered from the
fact that `wait_for_selector` matched.

At 202 it names the tool description and overrides it anyway:

> The reviewer keeps returning 0 findings — which per the tool description means
> "it found nothing to report, which is an answer". But given the ambiguity, let
> me do one more direct check […]

That wording was recommendation 3 of
[`sess-b949743ff7766606eb210ae59f2c1bcd.md`](sess-b949743ff7766606eb210ae59f2c1bcd.md).
It shipped. It did not change the behaviour, and it cannot: the model is not
disbelieving the sentence, it is correctly observing that `[]` and a blank page
produce identical bytes.

**What it cost.** Five calls returned `[]` — A/198, A/199, A/201, A/206, A/207 —
for **$0.0744, 23% of session A's entire spend**, carrying two characters of
information. A/206 is the extreme: 1,948 output tokens, 1,947 of them thinking,
to emit `[]`.

**Fix (highest value).** Make the reviewer say what it saw, not only what is
wrong. Change the contract in both instructions to
`{"observed": "<one or two sentences on what is in the image>", "findings": [...]}`
and print `observed` above the count line. A clean review then arrives as *"A
session list with an in-flight card and a six-row Finished table; 0 findings"* —
falsifiable in one read, and the whole re-ask spiral in A/199–204 never starts.
Touches `reviewScreenshotSpecInstruction`, `reviewScreenshotNoSpecInstruction`
and `formatReviewAnswer` in `internal/tools/reviewscreenshot.go`.

## 2. There is no way to ask "describe this", so the model breaks the schema to get it

Having failed to get a description through the review path, A/202 asks for a
verbatim transcription with a `"BLANK"` escape hatch. Gemini complies — and
abandons the schema to do it. The result came back as:

```
15 findings, 0 high confidence
[ { "image": "mock-inflight-card.png", "transcriptions": [] },
  { "image": "mock-inflight-card.png", "text": "RUNNING", "styling": "bold, blue text, light blue background badge" },
  … ]
```

A junk first entry, and fourteen `{text, styling}` objects that are not findings
at all. At 204 Gemini instead crams transcription into the findings shape —
`"issue": "Transcription & Styling Analysis"`, the transcribed line in `actual`,
the styling in `expected` — and the harness reports "3 findings, 3 high
confidence".

This *worked*: 202 and 204 are where the model finally got ground truth, and it
was right ("The card renders perfectly — bold title, phase 3/5 chip,
description, progress"). But the count line is a fabrication, and nothing stops
it: the Gemini call sets only `thinking_level` (`internal/gemini/types.go`,
`GenerationConfig`) — there is no `response_schema` — and `formatReviewAnswer`
counts array entries without looking inside them.

**Fix.** Add a describe/transcribe mode (a `mode` argument, or a second
`DescribeScreenshot` tool) with its own instruction and its own output shape,
and enforce shape with a real response schema so the count line means something.
If Finding 1 lands, this becomes a smaller job — most of the demand for it
disappears.

## 3. A follow-up is pinned to the original bytes, and that surprises the model

Session B, 104: the review correctly reports the Model dropdown is obscured by
the repo-suggestions overlay. The model closes the overlay and asks a follow-up
at 106 — "Now with the repo suggestions overlay closed: is the Model dropdown
visible…" — and gets the same finding back, because a follow-up re-reads the
*stored* paths and the new capture went to a different file. It diagnoses this
itself at 107:

> The reviewer is looking at the OLD image (start-form-fallback.png) — the
> conversation re-sends stored images from the first call. I need to start a new
> conversation with the new image path.

Cost of the lesson: $0.0088 and a sub-turn. The refusal that would have caught it
exists but points the wrong way — passing `image_paths` on a follow-up is an
explicit error ("a follow-up call takes no image_paths",
`reviewscreenshot.go:129`).

**Fix.** Let a follow-up carry `image_paths` and replace the conversation's
stored set while keeping the question/answer thread. That is exactly the
"I changed the thing, look again" loop, and it is currently the one shape the
tool forbids. Cheapest flexibility win on this list.

## 4. `Screenshot` cannot reach any page that needs a click

**Session B used `Screenshot` zero times** — 118 Bash calls, most of them
`playwright-cli`, and four reviews of images Playwright captured. Its target was
the start-run form, which needs a button click to open, an Escape to dismiss an
overlay, and a local proxy to stub `/api/models`. `Screenshot` takes a URL, a
selector and two waits, so it can photograph only what a fresh page load
produces. Session A used it eight times because its target was a static mock
page.

That is a clean split: the tool works for what it was built for and is unusable
for anything behind an interaction, which is most of a real frontend. The
description ("Capture a web page as an image with a headless browser") does not
say so.

**Fix.** Either add a short pre-capture action list (`click`, `press`, `fill`, on
selectors, executed before the wait) — `screenshot.js` already drives Playwright,
so the driver is there — or state plainly in the description that it captures a
page as loaded, and point at `playwright-cli` for anything requiring
interaction. Doing neither leaves the model to discover it per session.

## 5. The empty answer sent the model on a fourteen-sub-turn reconciliation

A/205–207: phone-width (390px) overflow, reviewed twice, `[]` both times,
$0.0292. Not trusting it, the model found `scripts/layout-check.mjs`, adapted it
for the mock server, and got a contradicting answer: three elements past the
viewport (`span.secondary +98px`, `b +41px`, `span.primary +18px`).

Sub-turns 208–221 — fourteen, including two full frontend rebuilds and a
`git checkout 82ba536 -- web/` to compare against main — went into deciding which
source to believe. Its conclusion was sound: the elements are inside
`.run-stats`, which has `overflow: hidden`, so they are clipped and invisible;
`getBoundingClientRect` reports unclipped geometry, so the checker flags what the
eye cannot see, and the same three findings exist on main. **Gemini was right and
the deterministic checker was over-reporting.**

Which is the point. The vision model gave the correct answer and the harness gave
it no way to be believed, so the run paid fourteen sub-turns to re-derive it.
This is Finding 1's cost, measured on a single question.

## Where it genuinely earned its keep

- **B/104 — occlusion.** Caught the repo-suggestions overlay covering the Model
  dropdown and the fallback hint, both high confidence, both correct. A DOM or
  accessibility snapshot cannot see this: the elements exist, are populated, and
  pass every assertion — they are simply covered. The model had *already*
  confirmed the combobox from a Playwright snapshot (refs e2208–e2210) and the
  review still told it something new. This is the tool's real sweet spot.
- **A/224 — dark-scheme contrast.** One high-confidence finding: the `—` empty
  subtitle placeholder is nearly invisible on dark. Correct, and the model
  correctly judged it pre-existing `var(--muted-foreground)` styling and out of
  scope rather than gold-plating.
- **A/204 — styling transcription.** Bold title, `phase 2/5` chip, normal
  description, dim subtitle, all three lines quoted accurately. This is the call
  that actually verified the change rendered — and it only worked because the
  model had learned to phrase a description question as a transcription request.

## Cost accounting is invisible to both the model and the operator

`geminiUsagePayload` maps a Gemini call into `store.UsagePayload`, which has no
model or provider field. In the event log the only thing distinguishing a $0.02
vision call from a $0.0008 DeepSeek turn is that it is the *second* usage event
on the same sub-turn. The session's headline cost silently absorbs it — session
A reads as $0.33 with no indication that $0.13 of it was Gemini.

The model has no signal either: neither the tool description nor the result
mentions what a call costs. That was the other half of recommendation 3 in the
previous review; only the "empty is an answer" half shipped.

Thinking is where the money goes. `thinking_level` is hardcoded `medium` for
every call (`client.go:154`), and on the empty answers thinking is ~100% of
output: A/206 spent 1,947 thinking tokens to print `[]`.

---

## Ranked fixes

1. **Return what the reviewer saw, not only what is wrong.**
   `{"observed": "...", "findings": [...]}` in both instructions, `observed`
   printed above the count line. `internal/tools/reviewscreenshot.go`. Would have
   removed A/199, A/201, A/202, A/207 and most of A/208–221 — call it 20 sub-turns
   and $0.06 of Gemini in one session.
2. **Let a follow-up replace `image_paths`.** `reviewscreenshot.go:129`. Turns the
   "I fixed it, look again" loop from a refusal into the cheap path it was meant
   to be.
3. **Put the price in the description and in the result.** "A call costs roughly
   fifteen sub-turns' worth of tokens" in the tool description; the call's cost on
   the result line. Completes the previous review's recommendation 3.
4. **Add a describe/transcribe mode, and enforce the output shape with a
   `response_schema`.** Today an off-schema answer is passed off as findings with
   a fabricated count.
5. **Decide what `Screenshot` is for.** Pre-capture actions in `screenshot.js`, or
   a description that says it captures a page as loaded and names `playwright-cli`
   for the rest.
6. **Make `thinking_level` a knob** — `low` for describe/transcribe, `medium` for
   spec review. `internal/gemini/client.go:154`.
7. **Tag Gemini usage events with their model** so vision spend is separable in
   the store and visible in the UI. `store.UsagePayload`.
8. **Point geometry questions at `scripts/layout-check.mjs`** — but note from
   Finding 5 that it over-reports clipped elements; worth teaching the model that
   the two disagree by design.

### Minor

- A/193: `Screenshot` refused `path: "mock-main-list.png"` with "pass a path under
  `scratch/`". One wasted sub-turn; the tool could simply place a bare filename in
  `scratch/` rather than refusing it.
- Kimi K3 sessions get neither tool (`definitions.go`), reading images natively
  instead. Every fix above is DeepSeek-only by construction — worth remembering
  when weighing the effort.
