# Can `agent-vision-toolkit` replace this harness's vision tools?

Investigated against `Anionex/agent-vision-toolkit` at a shallow clone of
`main`, 828 stars, created 2026-08-01 and last pushed 2026-08-14 (**observed**,
`gh api repos/Anionex/agent-vision-toolkit`). MIT, © 2026 Anionex
(**observed**, `LICENSE`). Python, ~42 MB checked out.

Short answer: **not as a replacement, and its headline mechanism — the
transparent proxy — cannot work here at all.** What it has that we want is the
half nobody talks about: the operations either side of "describe this image".
The cheapest way to find out whether those operations improve DeepSeek's design
advice is a skill-plus-CLI spike that touches no Go code.

---

## 1. What the toolkit actually is

Three separable pieces, and only one of them is interesting to us.

**The CLIs** (`bin/`): `glance` (describe / ask / OCR, with `--region`),
`ground` (locate a named thing, print a pixel box), `detect` (inventory every
instance of a kind, with boxes), `trace` (image → SVG with pixel-derived
geometry), `crop` (cut a box to a file, local, no API call). Plus
`skills/vision-tools/scripts/`: `long_screenshot_ocr.py`, `extract_fg.py`,
`html_shot.py`, `dominant_colors.py`, `pixel_diff.py`.

**The skill** (`skills/vision-tools/SKILL.md`, 312 lines): standard frontmatter
`name` + `description`, a routing table from question to tool, and
`references/` playbooks — `restore-ui.md`, `restore-structure.md`,
`restore-graphic.md`, `gui.md`, `long-screenshot-ocr.md`.

**The proxy** (`vision_proxy.py`, 1262 lines) plus `extensions/` for OpenCode
and Pi: a local endpoint that intercepts requests carrying images, describes
them, and substitutes text before the request reaches the text-only model.

There is also a `dsh-vision-toolkit` git submodule pointing at a separate repo —
an integration for DeepSeek's own harness, not for this one (**observed**,
`.gitmodules`).

### Configuration and dependencies

`vision_client.py` imports only the standard library — `base64`, `http.client`,
`json`, `mimetypes`, `os`, `pathlib`, `sys`, `time`, `urllib` (**observed**,
`vision_client.py:4-16`). Pillow is needed only for `--region` and `crop`;
`vtracer` only for `trace`. The runtime image already carries Python 3.14 and
pip (**observed**, `Dockerfile:73-85`), so the core path installs to nothing.

Config is three environment variables — `VISION_API_KEY`, `VISION_BASE_URL`,
`VISION_MODEL` — plus optional `VISION_API_PROTOCOL` (`chat_completions`,
`responses`, or `anthropic`), `VISION_REASONING_EFFORT`, `VISION_USER_AGENT`,
`VISION_ENV_FILE`, and `LANG` (**observed**, `vision_client.py:80,173-228`).

Note what is missing from that list: **there is no native Gemini protocol.**
Pointing it at our vision model means Gemini's OpenAI-compatible endpoint under
`chat_completions`, which is a different wire path from `internal/gemini`'s.
Thinking level would ride on `VISION_REASONING_EFFORT` rather than our
`WithThinkingLevel`.

---

## 2. The proxy cannot apply here

The proxy exists to rescue harnesses that put images into the model request and
discover the model is text-only. This harness never does that. Images reach
Gemini through the tool seam and never enter a DeepSeek request, so there is
nothing on the wire for a proxy to intercept — it would be a no-op process.

Worth recording the adjacent fact, because it changes the shape of this question
within a release or two: `internal/wire` already carries `image_url` and
`video_url` parts, added because **Kimi K3 reads images and DeepSeek does not**
(**observed**, `internal/wire/content.go:5-13`, docs/KIMI-INTEGRATION.md §4.5).
When Kimi lands, a Kimi session sees images natively and needs none of this;
DeepSeek sessions keep needing the seam. Any work here should not assume one
vision path for all providers.

---

## 3. What replacing the Go tools would cost

Our `AskVision` is 159 lines and `ReviewScreenshot` is 811 (**observed**,
`wc -l`). Most of that is not the API call — it is everything around it, and
each item below is something the CLIs do not do.

| What our tool does | Where | Under the CLIs |
| --- | --- | --- |
| Reports Gemini token usage per call, priced through the table, as a separable event | `askvision.go:126-136` | **Gone.** `vision_client.py` reports no usage at all — grep finds no `usage`, `prompt_tokens`, or `total_tokens` |
| Appends the cost line to the answer so the model sees what it spent | `askvision.go:133` | Gone with the above |
| Caps images per call, downscales, and tells the model it downscaled | `askvision.go:75-83,120-124` | Not present |
| Forces the "what is actually visible" sentence | `askvision.go:50-54` | Caller's problem again |
| Truncates to the session's output cap | `askvision.go:118` | Bash tool's own cap applies instead |
| Structured findings contract | `reviewscreenshot.go` | Gone entirely |

The usage line is the blocker, not a detail. The settings registry already
warns that a vision model missing from the price table makes its "spend silently
vanish from every figure in the UI rather than erroring"
(**observed**, `internal/settings/registry.go:207`) — moving every vision call
to a CLI does exactly that deliberately, for all of them. One measured session
spent 23% of its cost on the vision path (docs/reviews/vision-path-2026-08-14.md);
that number would have been unobservable under this architecture.

Two smaller consequences:

- **Cache prefix.** Removing tools from the array changes the frozen request
  head, so every existing session's prefix is invalidated once (docs/DESIGN.md
  §3.2). One-time, but real.
- **Credentials in the environment.** The bash tool runs
  `exec.CommandContext` with no `cmd.Env`, so a command inherits the harness
  process environment (**observed**, `internal/tools/bash.go:45`). Putting
  `VISION_API_KEY` there hands the Gemini key to every bash-capable session.
  The DeepSeek key is presumably already exposed the same way; this widens the
  blast radius rather than opening it.

---

## 4. What the toolkit has that we don't

This is the real finding, and it is worth acting on independently of whether
any of its code ever runs here.

`AskVision` and `ReviewScreenshot` both answer in **prose**. The toolkit's
premise is that prose is the wrong return type for half the questions:

- `ground` / `detect` return **pixel boxes**. "The CTA sits at (412,880)-(596,924)"
  survives being read by a model that cannot see; "the button looks slightly
  low" does not.
- `crop` + `glance --region` let a second look be **cheaper and sharper** than
  the first, instead of re-describing the whole page.
- `trace` turns a shape into geometry, `dominant_colors.py` returns palette
  values, `pixel_diff.py` compares two renders.
- `long_screenshot_ocr.py` chunks a tall screenshot on safe cut bands and merges
  overlaps — the failure mode we have not had to face yet but will.

That maps directly onto the design-review run that went badly on 2026-08-14: a
single prose description cannot carry spacing, alignment, or hierarchy precisely
enough for DeepSeek to give advice worth applying. The fix is not a better
description — it is a return type that isn't a description.

The toolkit also derives a **focus hint** from the caller's intent before
describing, which is the one prompting idea we could adopt this week: our system
instruction is fixed, and the caller's `prompt` is the only steer.

---

## 5. Three ways in, and which I'd pick

**A. Skill + CLIs, no Go changes.** Vendor `bin/` and `skills/vision-tools/`
into the image, put `VISION_*` in the harness environment, and let the model
drive them through bash. Discovery already works: `internal/skills` scans
`<repo>/.claude/skills/<name>/SKILL.md` and the toolkit's frontmatter is the
right shape — though note only the one-line description reaches the model
(**observed**, `internal/skills/skills.go:4-5`), so the model must `Read` the
SKILL.md itself before it knows the routing table. Cheapest by a wide margin,
loses cost accounting, and only works in permission modes that allow bash.
**This is the spike.**

**B. Wrap the CLIs in Go tools.** Keep confinement, caps, and the tool array;
shell out to `ground`/`detect`/`crop` and return their stdout. Keeps the
model-facing surface stable and the workspace rules enforced. Still no usage
numbers, and now we own a Python dependency in the request path.

**C. Port the operations into Go against `internal/gemini`.** `ground` and
`detect` are a prompt plus a JSON schema plus a coordinate contract; `crop` is
image manipulation with no API call at all. Keeps every guarantee, including
priced usage. Costs real development time and re-derives prompting the toolkit
has already tuned.

**Recommendation: A to learn, C to keep.** Run the spike to find out whether
coordinate-returning operations actually change the quality of DeepSeek's design
advice — that is the open question, and no amount of reading settles it. If they
do, port the two or three that earn their place into Go, because losing vision
spend from the transcript permanently is not a trade worth making for tools we
intend to keep. B only if we want them in production sooner than C can land.

Either way, `ReviewScreenshot` stays: nothing here replaces its findings
contract.

---

## 6. How `ground` and `detect` actually work, and what a Go port is

Read in full: `ground.py` (216 lines), `detect.py` (56), `vision_client.py`
(272). There is less here than the capability suggests, and almost none of it
is Python-specific.

### The whole contract

`ground` sends the image plus this prompt (**observed**, `ground.py:31-38`):

> Locate every visible object or region matching this target:
> `<target>`
>
> Return only a JSON array. Each item must contain "box_2d" as [y0, x0, y1, x1]
> on a 0-1000 grid and "label" as a short description. Use tight boxes in the
> original image. Return [] when nothing matches.

`max_tokens` is 8192, with a comment recording why: 2048 truncated the JSON
mid-array on a dense screen (`ground.py:157-159`).

**That 0-1000 grid with `[y0, x0, y1, x1]` ordering is Gemini's own
bounding-box convention**, which is why a prompt this thin works at all. It is
also the property that makes the port safe against our existing image pipeline:
the coordinates are resolution-independent, so `loadReviewImages` downscaling a
capture before sending does not corrupt them — scale the returned box by the
**original** width and height and it lands correctly. (Perception still
degrades with the downscale; the arithmetic does not.)

`detect` is `ground` with a canned target and no other logic at all
(**observed**, `detect.py:14-20`):

> every distinct `<category, default: UI element (buttons, links, inputs,
> icons, labels, headings, images, badges)>` — include the exact visible text in
> each label

### The parse pipeline

Four stages, each earning its place (**observed**, `ground.py:41-122`):

1. Strip markdown fences, taking the **last** fenced block if several.
2. `json.loads`. On failure, a regex scavenger pulls out any `{...}` containing
   a `box_2d`/`bbox_2d`/`box2d`/`bbox`/`box` key with four numbers.
3. Accept a bare array, or a dict wrapping it under `boxes`,
   `bounding_boxes`, `bboxes`, `objects`, `items`, or `results`.
4. Normalise each box: swap inverted axes, scale `/1000 * dimension`, clamp to
   bounds, drop anything degenerate. Label falls back through
   `label` → `caption` → `description` → the target string.

`--region` crops locally, sends only the crop, and translates the returned
boxes back into original-image coordinates (`ground.py:137-165`).

Output formatting is deliberate: one match prints bare `x1: … y1: … x2: … y2:`,
several print numbered lines each prefixed with a coarse 3×3 position word
(`top-left`, `center`, `bottom-right`) computed from the box centre
(`ground.py:168-190`).

### What `vision_client.py` does that we already do better

It is a `urllib` poster with three request shapes (`chat_completions`,
`responses`, `anthropic`), bearer auth, two retries on 429/5xx with
`Retry-After`, a 180s timeout, and key redaction in error bodies. Every one of
those is already in `internal/gemini`, against the **native** Gemini API rather
than an OpenAI-compatible shim — plus thinking levels, streaming, and the usage
accounting their client has none of. None of this file should be ported.

### Port size

| Piece | Go equivalent | Notes |
| --- | --- | --- |
| `ground` | ~150 lines + tests | Prompt, the four-stage parser, box normalisation |
| `detect` | ~10 lines | The canned target string, calling the same code |
| `crop` | ~80 lines | `image/png`, `image/jpeg`, `x/image/draw` for the upscale — no API call |
| `glance` | already `AskVision` | Missing only `--region`, which `crop` covers |
| `trace` | skip | Depends on `vtracer` (Rust); not worth a cgo dependency |
| `vision_client` | already `internal/gemini` | Do not port |

So the honest scope is **one new tool (`Locate`, with a category mode) and one
local image operation (`Crop`)**, both against the client we already have,
keeping priced usage, workspace confinement, output caps, and the permission
policy that the CLI route gives up.

### Risks worth naming before writing it

- **The parser's tolerance is the product.** Porting the prompt without stages
  2–4 gets a tool that works in testing and fails on the dense screens that
  matter. Their regex fallback exists because the model does return prose
  around the JSON sometimes.
- **Structured output is tempting and unproven here.** Gemini's response
  schema could make stage 2 deterministic, but `askvision.go:106-110` records
  that constraining the container without a schema "empties or mangles the
  contents". Ship the tolerant parser first; try the schema as an optimisation
  with an eval behind it.
- **Accuracy is unmeasured.** Whether the boxes are tight enough on a rendered
  page to act on is still the open question a spike answers, and it is cheaper
  to answer with their CLIs than with our port.

## 7. Not verified

- **Nothing was run.** The local settings table is empty and `.env` carries no
  Gemini key, so no CLI was executed against the live API. Everything above is
  from reading. Running the spike needs a key from the production stack — a
  credential-handling decision I left alone.
- **Output quality is unmeasured.** Whether `ground`'s boxes are accurate enough
  on a rendered web page to act on is exactly what the spike would tell us.
- **Licence read, not audited.** MIT at the root; vendoring means checking the
  bundled `scripts/` and any optional dependency (Pillow, vtracer) separately.
- **The `dsh-vision-toolkit` submodule was not cloned.** It targets DeepSeek's
  harness, and may or may not carry ideas worth reading.
