# Tool specification

## Why these names

DeepSeek-V4 shipped with dedicated agent optimisations and names Claude Code,
OpenClaw, and OpenCode as its integration targets. DeepSeek runs its own
in-house agentic coding on it.

That is the observation. The inference this document acts on: the tool
vocabularies those harnesses use — names, argument names, result shapes — are
the ones V4 handles most reliably. The inference is not stated anywhere in
DeepSeek's docs, but matching a vocabulary costs nothing and diverging from one
costs accuracy, so the asymmetry decides it.

Claude Code and OpenCode converge on the same set. Where they differ in casing,
this spec follows Claude Code, which DeepSeek names first and supports with
explicit model mapping and a documented environment configuration.

Function names must match `[a-zA-Z0-9_-]{1,64}` and a request may carry at most
128 of them. Both vocabularies fit.

One caveat on the evidence, recorded in full in [VALIDATION.md](VALIDATION.md).
The vendored docs establish that DeepSeek optimised V4 for those harnesses. They
never state what those harnesses call their tools. The specific names below come
from outside this repository, which makes them the weakest load-bearing
assumption in the design — and also a cheap one to be wrong about, since a
rename is the whole fix.

The one concrete coding tool name DeepSeek does document is `apply_patch`:
`{"type": "custom", "name": "apply_patch"}` is accepted on the Responses API and
any other custom name returns 400. That is direct evidence of tuning against
Codex's patch format. It is Responses-API-only, so it does not
reach our endpoint, but it suggests patch-shaped editing is trained in and worth
trying against `Edit` if exact-match replacement underperforms.

## The v1 set

| Tool | Arguments | Purpose |
| --- | --- | --- |
| `Read` | `file_path`, `offset?`, `limit?` | Read a file, line-numbered |
| `Write` | `file_path`, `content` | Create or overwrite a file |
| `Edit` | `file_path`, `old_string`, `new_string`, `replace_all?` | Exact-match replacement |
| `Bash` | `command`, `timeout?`, `description?` | Run a shell command |
| `Glob` | `pattern`, `path?` | Path matching by pattern |
| `Grep` | `pattern`, `path?`, `glob?`, `output_mode?` | Content search, ripgrep-backed |
| `List` | `path`, `ignore?` | Directory listing |
| `TaskCreate` | `tasks[]` | Append tasks to the working plan |
| `TaskGet` | `taskId` | Fetch one task by taskId |
| `TaskList` | `status?` | List the working plan (optionally one status) |
| `TaskUpdate` | `taskId`, `status?`, `subject?`, `description?`, `activeForm?` | Patch one task by taskId, or remove it with `status: "deleted"` |
| `Task` | `description`, `prompt`, `subagent_type` | Delegate to a flash-backed subagent |
| `WebFetch` | `url`, `prompt` | Fetch a URL and extract against a question |
| `Screenshot` | `url`, `path`, `width?`, `height?`, `device_scale_factor?`, `color_scheme?`, `full_page?`, `selector?`, `wait_for_selector?`, `wait_ms?` | Capture a page with a headless browser into `scratch/` |
| `ReviewScreenshot` | `image_paths[]`, `question`, `spec?` | Send screenshots to Gemini's vision model and return its diagnosis |
| `Complete` | `summary`, `result?`, `status?` | Emit the run's machine-readable result |

Sixteen tools. The first thirteen have a trained-in analogue in at least two of
the three named harnesses. `Screenshot`, `ReviewScreenshot` and `Complete` do
not. The first two exist because DeepSeek cannot see images, so the harness
builds the whole visual path itself — a capture it controls, and a diagnosis
from Gemini; the trained-vocabulary argument above says nothing about any of
them, and each is named for what it does.

The array is per-provider (docs/KIMI-INTEGRATION.md §4.5, decision 5). A Kimi
K3 session gets the fourteen tools that remain when `Screenshot` and
`ReviewScreenshot` are dropped — K3 reads images natively, so both are
redundant for it, and capture happens through Bash and the `playwright-cli`
skill instead. DeepSeek's array is the full sixteen, unchanged byte for byte.
Each array is a frozen request head shared by every session on its provider,
pinned by its own golden file (`internal/tools/testdata/tools_*.golden.json`,
asserted by `TestToolArrayGolden`); the `Read` section below covers how an
image reaches the model on the provider that can see one.

## Per-tool notes

### Read

Returns line-numbered content, `cat -n` style, because that is the shape the
target harnesses return and the model reads offsets out of it.

On a provider that sees images (Kimi K3), a `Read` of a PNG, JPEG, or WebP
path returns the file as an `image_url` part — the bytes base64-encoded into a
`data:image/<fmt>;base64,...` data URI, the exact shape the Kimi guide
prescribes (third_party/kimi-docs/guide/use-kimi-vision-model.md), with a
short text label naming the file alongside. The encoded size is capped by the
existing screenshot cap setting (`tools.reviewscreenshot_max_bytes`, the same
5 MB default ReviewScreenshot enforces per file), and an over-cap image is
refused with a result naming the limit and suggesting a resize — a refusal
the model can act on, never a failed run. The image bytes are stored on the
`tool_result` event, so the fold replays them identically
(docs/KIMI-INTEGRATION.md §4.5). An SVG reads as text even on a vision
provider — it is XML, and the API prescribes sending the source as text — and
any other binary type is refused with a message that says so. On DeepSeek the
behaviour is unchanged: an image is a binary file and is refused like any
other.

This creates a known friction with `Edit`: the line-number prefix is display
only and must not appear in `old_string`. Detect a leading line-number pattern
in `old_string` and return a targeted error naming the problem rather than a
generic no-match. The model corrects on a specific error and flounders on a
vague one.

### Write

Overwriting an existing file requires that the session has already read it. The
gate forces the model to work from real bytes rather than remembered ones.
Creating a new file needs no prior read.

### Edit

The highest-risk tool, and the one where harnesses diverge most in quality.

- `old_string` must match exactly once unless `replace_all` is set. On multiple
  matches, return the count and refuse. On zero matches, return the nearest
  candidate line range if one exists.
- A prior `Read` of the file is required.
- The result carries the applied diff, so the model sees what actually landed
  rather than assuming its intent was realised.

### Bash

The tool the permission policy exists for. Wall-clock timeout and an output byte
cap on every invocation, with truncation labelled in the result so the model
knows it saw a fragment. Both are defaults, not fixed values: an operator
changes them with `harness config set tools.bash_timeout` (and
`tools.bash_timeout_max`, the ceiling a request's own timeout is clamped to)
and `tools.output_cap` — the whole `tools.` group of settings — and the next
tool call picks the change up without a restart.

Commands run through `bash` where the machine has one and `/bin/sh` otherwise,
resolved once at first use. The image installs bash, so a session gets it;
models write `${PIPESTATUS[0]}`, `[[ ]]` and arrays regardless of what the
shell is, and busybox ash answers those with a syntax error.

Foreground only. Background shells with separate output-polling and kill
tools are not built, and they matter for dev servers and test watchers.

A command that backgrounds a process without redirecting its output —
`node server.js &`, inheriting the captured pipe — leaves that pipe open after
the shell exits: `Wait` would otherwise block on the copy goroutines past the
tool timeout, past a cancelled context, forever. `tools.bash_wait_delay`
(default 2s) bounds that wait. When it fires, the harness
stops waiting, kills the process group the command ran in, and returns an error
naming the mistake and the fix — the model is the one who has to route around
it:

> the command exited but left a process holding its output open; the harness
> stopped waiting after 2s and killed the process group. Redirect output and
> detach (`cmd >/tmp/x.log 2>&1 &`) if you meant to leave something running.

The kill reaches the whole group, not just the direct shell child, so the orphan
that held the pipe dies with the call instead of outliving it and keeping
whatever port it bound. A command that detaches with its output redirected is
untouched: the pipes close with the shell, `Wait` returns normally, and nothing
is killed.

### Grep and Glob

Backed by ripgrep where available, with a Go fallback. `Grep` defaults to
returning matching file paths; content and count modes are selected by
`output_mode`. Keeping the default cheap matters because the model uses search
to orient and would otherwise pull large content into a context that gets
re-sent every sub-turn.

### TaskCreate, TaskGet, TaskList, TaskUpdate

The target harnesses all carry a todo tool, so V4 will reach for one. Where
they write the whole plan array on every change, this harness splits that
into a CRUD family: `TaskCreate` appends tasks (ids minted in call order and
stable for each task's whole life), `TaskUpdate` patches or removes one task
by taskId, and `TaskGet`/`TaskList` read one task or the whole plan back.
The field names match Claude Code's own Task tools — each item carries
`subject` ("a brief, actionable title"), `description` ("what needs to be
done"), and `activeForm`, and the task identifier is `taskId` — with the
deliberate deviations that `TaskCreate` keeps its batch-array shape
(`{"tasks": [...]}`) so seeding a whole plan is one call, and `TaskList`
keeps its optional `status` filter. No dependencies, `owner`, or `metadata`:
a harness session is a single agent, not a team with tasks to claim.

The split exists so a per-item update is cheap enough that an agent does not
skip it mid-run. Rewriting the whole plan on every status change costs the
model the full checklist to produce and the session a bigger message to
append; naming one taskId in a short `TaskUpdate` call makes keeping the
plan current the obvious move. A terminal harness renders the list as a
checklist that scrolls away. A web UI can pin it as a live plan panel beside
the transcript, which is one of the clearer wins the browser buys us.

`TaskUpdate` patches only the fields present. `status: "deleted"` removes the
task — it is only ever an input trigger, never a stored status, so a
"deleted" combined with a patch, an update with nothing to set, an unknown
taskId, or an invalid status are all errors and mutate nothing — the same
validation the event-log replay applies (`internal/store/status.go`), so a
session's recovered plan can never drift from what the loop actually did.
`TaskGet`'s single-item result shows the task's `description` too, which the
compact checklist line deliberately leaves out.

### Task

Delegates to a subagent on `deepseek-v4-flash`. DeepSeek's own recommended Claude
Code configuration sets `CLAUDE_CODE_SUBAGENT_MODEL=deepseek-v4-flash`, so this
is both the trained-in pattern and the cheap one.

The subagent runs in its own conversation. Its transcript never joins the parent
message array — only its final result does. That keeps the parent prefix stable
and avoids mixing caches across models.

### WebFetch

Ours, not DeepSeek's. See the endpoint trade-off below. Fetch, extract to text,
then have flash answer the caller's `prompt` against the extracted content, so
the parent context receives an answer rather than a page. The fetch size cap,
the extraction cap, and the call's timeout are defaults under the `tools.`
group of settings (`tools.webfetch_max_body`, `tools.webfetch_max_extract`,
`tools.webfetch_timeout`).

### Screenshot

The capture half of the vision path. It drives a headless Chromium to a URL
and writes one PNG or JPEG, which `ReviewScreenshot` then sends to Gemini and
the transcript renders inline.

The harness owns the capture rather than leaving the agent to compose a
Playwright invocation through `Bash`, because the settings that decide whether
a screenshot is worth anything — the viewport, the colour scheme, whether the
image is clipped to the element actually in question — are exactly the ones an
ad hoc invocation omits. An agent left to its own devices takes a full-page
capture of a desktop-width page in the light scheme and reviews that, which is
the one combination least likely to show a problem.

So the defaults are the standard:

- **The viewport, not the full page.** `full_page` is off. A full-page capture
  of a long document is downscaled to the same token budget as a viewport one,
  so every control on it shrinks until it is unreadable — the failure the
  `ReviewScreenshot` notes below describe. When a viewport capture cuts the
  document off, the result says so, with both heights, so the model knows what
  it did not see rather than concluding the page ends there.
- **`selector` clips to one element.** This is the answer to "the button looks
  wrong": capture the button. It is also why the tool drives the Playwright
  library through an embedded Node script rather than the `playwright
  screenshot` CLI, which can neither clip to an element nor set a device scale
  factor.
- **`device_scale_factor` stays at 1.** The image's next stop is a vision model
  that downscales it regardless, so doubling the pixels doubles the bytes
  against `ReviewScreenshot`'s per-file cap and buys nothing in what Gemini
  sees. Raise it when a human is going to read fine detail in the transcript.
- **`color_scheme` defaults to light, and the tool description tells the model
  to capture both.** A layout that holds in one scheme can break in the other,
  and one capture cannot cover both.

- **`actions` drive the page before the shutter.** For most of a real
  frontend the state worth photographing is not the one a fresh load
  produces: a dialog has to be opened, an overlay dismissed, a field filled.
  The vocabulary is four verbs — `click`, `fill`, `press`, `hover` — capped at
  ten steps, and it is deliberately not a browser automation language: the
  moment a capture needs a conditional it has become a script, and the session
  should write one. They run after `wait_for_selector` (which is how the
  caller says the page is ready) and before `wait_ms` (the settle after them,
  for the animation opening the dialog). A step whose selector never appears
  fails the whole call and names the step by position and intent — capturing
  anyway would produce a screenshot of the wrong state, and nothing
  downstream could tell.

  This is the gap that cost the tool a whole session. Asked to photograph a
  form that needed a click to open and an Escape to clear an overlay, a
  production run used `Screenshot` zero times and drove Playwright through
  `Bash` for all of it — which is exactly the ad hoc invocation the tool
  exists to replace, so the capture went back to being full-page, desktop-
  width and light-scheme with none of the standard above
  ([`docs/reviews/vision-path-2026-08-14.md`](reviews/vision-path-2026-08-14.md)).

Output is confined to `scratch/`, the directory the system prompt already
reserves for files that are not part of the deliverable. That confinement is
what lets the tool run in a read-only session (see "Permissions"): the capture
cannot land in a cloned repository whatever the mode.

The result carries what the page did while it was captured — its title, the
steps that ran before the shutter, the document height against the viewport
height, and any console or uncaught page
errors, capped at twenty of each. A capture that came back blank therefore
arrives with the reason it was blank, instead of costing a second sub-turn to
go and find out. URLs are limited to `http`, `https`, and `file`; the timeout
is `tools.screenshot_timeout` (90s by default, covering the browser launch,
the navigation, the settle and the encode), and the driver's own navigation
timeout is derived from it so a slow page fails with a message rather than
being killed silently.

### ReviewScreenshot

DeepSeek is text-only, so this is the harness's vision path: `Screenshot`
captures the page (or the agent writes one itself with a headless-browser
script) and this tool sends it to Google Gemini for a diagnosis. It accepts PNG, JPEG, or WebP files,
workspace-confined like every other path-taking tool. The image count and the
per-file size cap are defaults, not fixed values
(`tools.reviewscreenshot_max_images`, `tools.reviewscreenshot_max_bytes`), so
the tool description quotes no numbers — it is part of the frozen request head
(docs/CACHE.md) and a number there would make the head vary per installation.
A call that exceeds a bound is refused with an error stating the actual limit,
which is how the model discovers it. The first image is sent at `high`
resolution and the rest at `medium`, per the prompting notes' advice that only
the image needing scrutiny should be high — the model is told to put the
screenshot it cares about first
([`docs/gemini-3.5-flash-ui-review-prompting.md`](gemini-3.5-flash-ui-review-prompting.md)
has the request-shape rationale: no temperature/top_p/top_k, `thinking_level`,
"data first, question last"). A full-page capture is downscaled to roughly a
thousand image tokens, which leaves a small control unreadable, so the
description tells the model to screenshot the element itself when the question
is about one.

#### Every answer says what it saw

The answer is an object, and its first field is `observed`: one or two plain
sentences on what is actually on the screen. It is required, it is never
empty, and the tool result leads with it, above the count.

This is the tool's central fix and it is worth stating why, because the
obvious design is the one that failed. An answer that is only a findings list
makes "the page is correct" and "the page never rendered" the same two bytes —
`[]` — and the caller cannot open the image to break the tie. DeepSeek noticed
this unaided and said so in its reasoning: *"the '0 findings' answer is only
meaningful if the page actually rendered the content […] 0 findings could also
mean 'nothing rendered at all'"*. It then re-asked, got `[]` again, and in one
measured session spent five calls and 23% of the run's entire cost
establishing that a clean answer was clean.

The previous attempt at this was a sentence in the instruction telling the
model an empty list is an answer
([`sess-b949743ff7766606eb210ae59f2c1bcd.md`](reviews/sess-b949743ff7766606eb210ae59f2c1bcd.md),
recommendation 3). It shipped, and it did not work — the same session quotes
that sentence back and overrides it anyway. The lesson is that the model was
never disbelieving the claim; it was correctly observing that the result
carried no evidence. Wording cannot fix a missing fact, so the fact is now in
every answer, and a blank capture arrives as words rather than as an empty
list ([`vision-path-2026-08-14.md`](reviews/vision-path-2026-08-14.md)).

The shape is asked for and not enforced, and that is deliberate. The API's
`response_format` carries a type with no schema beside it, and constraining
the container costs the contents — asking for `"object"` returns a literally
empty object
([`gemini-3.5-flash-ui-review-prompting.md`](gemini-3.5-flash-ui-review-prompting.md)
has the measurement). So the call sends no `response_format`, the model reads
the instruction and picks its own container, and the harness handles whatever
comes back:

- A ```` ```json ```` fence is stripped. It is what the model wraps its answer
  in when nothing constrains it, and it is a much smaller problem than an
  empty answer.
- A bare array is the pre-`observed` shape, still read as findings, with the
  missing description called out in so many words.
- An object without `observed` gets that same note.
- Anything that is not JSON comes back as raw text, labelled as unparsed
  prose rather than passed off as structure.

#### Modes and the standard applied

`mode` chooses what the call is for. `review`, the default, judges the
screenshots; `describe` makes no judgement and returns what is on the screen —
the layout, and each element's text, role and styling.

Describe exists because the model kept asking for it through the review path
and the review path kept refusing. A "what does this page actually show"
question has no place in a findings schema, so it came back as `[]`. Left with
no other route, DeepSeek asked for a verbatim transcription inside a review
call — Gemini complied by abandoning the findings shape, and the harness
reported the result as "15 findings, 0 high confidence", one of them the
object `{"transcriptions": []}`. The workaround worked and the count line was
a fiction; a mode is the honest version of it.

Within `review`, `spec` decides which instruction the call carries. With a
spec, the model is told the spec is the only standard of correctness and that
anything it does not cover is intentional; without one, it is held to defects
visible on their own terms — overlap, clipping, overflow, contrast — and told
not to report stylistic judgements. A vision model given no standard falls
back on general web-design convention and returns deliberate choices as
breakage, which is why the tool description urges a spec on every call. Both
forms ask for the same findings, each carrying a `confidence` and an `image`
naming the screenshot it concerns. `describe` is held to neither: a spec is
meaningless to a call that judges nothing.

The image name comes from a label: each image part is preceded by a text part
carrying its file's base name ("Image 1: home-dark.png"), so a finding on a
multi-capture comparison says which screenshot it is about, and the
transcript's rendered image sits under the same name.

The model comes from the `google.vision_model` setting (default
`gemini-3.5-flash`) and the key from `google.api_key`, both read through the
settings table on every call, so either can change without a restart. The call
has its own timeout (default 60s, `tools.reviewscreenshot_timeout`) rather than
the 30-second tool default.

How hard the vision model thinks is `google.vision_thinking_level`. It defaults
to `auto`, which lets the mode decide: `medium` to judge a page against a spec,
`low` to describe one, on the grounds that saying what is on a screen is not
the part that needs reasoning. Thinking is where a call's money goes — it bills
at the output rate, and an empty findings list has been measured at 1,947
thinking tokens against a one-token answer — so this is the cheapest lever the
tool has.

#### Following up, and re-capturing

A review can be continued without re-uploading. A call without
`conversation_id` starts a conversation and its result carries the id; a call
passing that id and a question continues it. The conversation is held on the
session's Executor and stores the image paths and the prior question/answer
pairs — never the image bytes — so a follow-up re-reads the files from disk (a
file deleted since the first call fails with an ordinary error naming it),
re-sends the images, the earlier exchanges, and the new question in one
request. "Look closer at the header" therefore costs one Gemini interaction
with the thread replayed, not a fresh review from scratch. Retained
conversations are capped at the most recent five per session, so a long
session cannot grow this without bound.

A follow-up may also carry new `image_paths`, which replace the ones under
discussion while the thread survives — review, change something, capture it
again, ask whether it is fixed. That loop is what the tool is used in, and it
used to be the one shape refused: a model that had re-captured a page got its
previous answer back, about the previous bytes, and paid for it. The
replacement is announced inside the request, because a fresh capture replayed
alongside the model's own earlier answer otherwise gets reconciled against
that answer instead of being looked at. The spec and the mode stay fixed for
the conversation's life — the thread being replayed was produced under them.

An image can also arrive with the task. A work request may carry attachments
(`POST /api/runs`, the MCP `deepseek_agent` tool — docs/DATA-API.md): the
bytes are stored in SQLite, the request carries only ids, and
`internal/workspace` materialises them into `scratch/attachments/` during
`Prepare`. The opening message names the files, so the model knows they exist
and can pass one to ReviewScreenshot as the image the page should be judged
against — "make it look like this mockup" becomes a review against the mockup
itself instead of a prose description of it. Only PNG, JPEG, and WebP are
accepted, capped in count and per-file bytes by
`tools.attachments_max_count` and `tools.attachments_max_bytes`.

#### What it costs, and who can see that

A Gemini call bills separately from the sub-turn that made it. Its usage rides
home on the tool result, and the runner stamps the sub-turn it happened in and
commits it as its own usage event, priced against `configs/prices.json` under
the vision model that ran — so a session's cost total covers Gemini the same
way it covers DeepSeek (docs/DESIGN.md §4.9). A model with no entry in the
price table leaves the cost at zero rather than failing the call: the run
succeeded, and the operator's price table is the incomplete thing.

That event carries a `model`, and it is the only thing separating it from the
session's own turns — it shares their shape and their sub-turn number. Without
it, two measured sessions spent 39% and 24% of their budget here with nothing
in the log to say so, because a two-cent vision call and a sub-cent DeepSeek
turn were the same record twice.

The cost also comes home **on the tool result**, in the place the model
decides whether to make another call, along with how much of it was thinking.
The tool description cannot carry the number: it is part of the frozen request
head, so a figure there would vary per installation and per price table
(docs/CACHE.md). It says a call is expensive in general; the result says what
this one actually cost. An unpriced model prints no line at all, because an
invented `$0.0000` reads as "this was free".

### Seeing the screenshots

Both screenshot tools' results render in the transcript with the images
above them, served by `GET /api/sessions/{id}/screenshot?path=…`
(`internal/httpapi/screenshots.go`). Without it a transcript reports what the
vision model said about a page and never shows the page, which leaves the one
artefact that would settle whether the model was right out of the record —
and a session review has to take Gemini's prose on faith.

The endpoint reads the session's live workspace. That is the trade-off it is
built on: no schema change and the image at full resolution, against the fact
that a workspace gets cleaned up (docs/RUN-CONTROL.md) and an old session then
has no images left to serve. A missing file is therefore an ordinary 404 that
the transcript renders as "screenshot no longer available", keeping the path
visible, rather than an error or a broken image. Storing a downscaled copy in
the database is the alternative if that becomes the common case.

Two properties keep it from being a general file read over the workspace: the
path must resolve inside that session's own workspace with symlinks fully
resolved, and the extension must be one of the three image types
`ReviewScreenshot` accepts. An escape and a missing file return the same 404
with the same text, so a caller probing for a path outside the workspace
learns only that it cannot have it. Like every other `GET` on the surface it
carries no control token; the write endpoints are the authenticated ones.

A `Task` child's transcript re-provides its own session id, so a subagent's
screenshots resolve against the workspace the subagent ran in rather than its
parent's.

### Complete

The seam between an agent run and the queue that asked for it. `summary` is
prose for a human reading the transcript. `result` is the payload the requester
receives in `harness.work.result.<id>.final`. `status` distinguishes a finished
job from one the model gave up on, which is a different thing from a harness
error.

A call to `Complete` ends the run. Parallel tool calls mean it can arrive
alongside others, so the rule is that the whole batch executes in `tool_calls`
order and the run ends after it.

Two consequences fall out of constraints stated elsewhere.

It cannot be forced. Thinking mode rejects `tool_choice: required` and named
tool forcing ([OBSERVED.md](OBSERVED.md)), so there is no way to make the model
call this before it stops. The system prompt asks for it, and a run that ends
without it returns its final assistant text with a null `result`. No error path
is needed for the omission.

Its schema never varies. When a work request supplies a `result_schema`, that
schema goes in the opening user message and validation happens in Go against the
stored copy. Putting it in the tool definition would give every request a
different tool array and cost the whole shared prefix ([CACHE.md](CACHE.md)). A
payload that fails validation returns the errors through the tool result channel
and the run continues, so the model gets to correct it — but only three times.
Three consecutive rejections carrying the *same* message end the run:
`run_finished` reason `complete_rejected`, session status `failed`, queue result
`failed` with code `complete_rejected`, and an `error` event carrying the
validation message. A correction opportunity the model is not taking is a budget
leak, and one live run spent eleven sub-turns and 7% of its cost re-sending a
payload whose shape it never varied. A rejection whose message *differs* from
the last one is progress and restarts the count; the sub-turn limit remains the
backstop for a model cycling between several wrong shapes.

Because the schema lives in the opening message rather than the tool definition,
that message also carries a worked example of the call built from the schema's
own field names — `Complete(summary="…", status="done", result={"branch": …})`.
The schema alone does not say where its fields go, and the failure it invites is
sending them as top-level arguments beside `status`, which produces `result:
expected object, got null` — an accurate message about what is missing that says
nothing about what was sent. `execComplete` detects that specific case and names
the stray arguments.

`Complete` ships in every session including CLI ones that will never call it.
One tool array across every caller is what keeps the stable head shared.

## Two decisions this changes

### Strict mode is now per-tool, not global

`DESIGN.md` §4.6 said author every schema for strict mode from day one. That was
decided before the trained-vocabulary evidence.

Strict mode requires every object property to appear in `required` and
`additionalProperties` to be `false`. The trained-in schemas have genuinely
optional arguments — `Read`'s `offset` and `limit`, `Grep`'s `path` and `glob`.
Under strict those become `anyOf` null-variants that are still listed as
required, which is a different shape from the one the model was tuned against.

So: match the trained shape, non-strict, and validate arguments in Go. A bad
argument returns a clear error through the tool result channel and the loop
recovers — that channel exists for exactly this. Reserve strict mode for tools
where a malformed argument is dangerous rather than merely wrong. Strict is also
still Beta.

### Web search is not on the endpoint we use

DeepSeek serves a native, server-side web search tool on two of its three
endpoints. The Anthropic compatibility table supports `server_tool_use` and
`web_search_tool_result` content blocks, and the Responses API accepts
`web_search` and `web_search_2025_08_26` as tool types. The OpenAI-format Chat
Completions API, which is the one we use, supports `type: "function"` and
nothing else.

So the choice is:

- Stay on Chat Completions (`DESIGN.md` §2) and keep strict mode and the
  `prompt_cache_hit_tokens` / `prompt_cache_miss_tokens` split — but build
  `WebFetch` ourselves and go without trained-in web search.
- Move to `/responses` for server-side search. Cache accounting narrows to a
  single `input_tokens_details.cached_tokens`, and the compatibility table does
  not say whether a function tool's `strict` flag is honoured.
- Move to `/anthropic` for server-side search and lose both.

FIM and prefix completion sit on the `/beta` base URL and are unaffected by
this choice.

Recommendation: stay on Chat Completions. Search is one tool among sixteen, our
own `WebFetch` covers the documentation-lookup case that a coding harness
actually needs, and DeepSeek's own note says its web search bills extra tokens
for summarisation anyway. The decision is reversible per-session if it proves
wrong, since the endpoint is already config.

## Execution rules

These hold for every tool and live in Go, not in prompt text.

- Paths resolve against the workspace root and are checked for escape after
  symlink resolution. Escapes are rejected, not sanitised.
- Every tool has a wall-clock timeout and an output byte cap, with truncation
  labelled in the result. Both are settings (`tools.` group) with the defaults
  listed above; the values are resolved from the settings table on each call,
  so a limit changed with `harness config set` applies without a restart.
- Tool results are appended in `tool_calls` array order, never in completion
  order. Parallel tool calling is always on and cannot be disabled: the
  Responses API guide states it outright, the Codex model catalogue declares
  `"supports_parallel_tool_calls": true` for both models, and the Anthropic
  table ignores `disable_parallel_tool_use`. Executing concurrently is fine;
  appending out of order is not, because it churns the prefix and costs the
  cache (`DESIGN.md` §3.2).
- Never send `tool_choice`. Thinking mode accepts `auto` and `none` but rejects
  `required` and named-tool forcing, so no tool can be forced while thinking is
  on (`DESIGN.md` §4.4, [OBSERVED.md](OBSERVED.md)).

## Permissions

Permission is a policy the session is given at creation, not a question it asks
later. The browser is read-only, so there is nobody there to ask.

Two modes, fixed for the life of a session and required on every request:

- Read-only. `Read`, `Glob`, `Grep`, `List`, `WebFetch`, `Screenshot`,
  `ReviewScreenshot`, `TaskCreate`, `TaskGet`, `TaskList`, `TaskUpdate`, and
  `Complete` run. `Write`, `Edit`, `Bash`, and `Task` are denied.
  `Screenshot` is the one tool in that list that writes to disk, and it is
  there because of where: its output is confined to `scratch/`, which holds
  nothing that is part of a run's deliverable, so a read-only session can
  look at a page it is reviewing without being able to mark a cloned
  repository. Reviewing a UI is the read-only run's whole job, and denying it
  the capture would have left it reading markup and guessing.
- Full access. Everything runs, as root, inside the workspace mount.

There is no third mode between them and no default. Every ingress — a work
request, the MCP launch tool, `harness run` — rejects a request that does not
name one, so no configuration value decides a session's permissions on a
caller's behalf.

A middle mode existed until it was removed. It gated `Bash` behind an
executable allowlist that included `go`, `npm`, `make`, and `python`, each of
which runs arbitrary code, so the boundary it drew was narrower than it
appeared. Read-only against full access is the distinction the harness can
actually enforce.

`WebFetch` runs in read-only mode. It reaches the network, so read-only bounds
what a session can change on disk rather than what it can send.
`ReviewScreenshot` is the same: it reads a file and sends it over the network,
changing nothing on disk.

A work request may add `deny` patterns on top of its mode. They only ever
subtract; a request cannot widen the mode it asked for.

Modes gate execution, never availability. All of a provider's tools are sent
on every request in every mode, and a call the mode disallows is refused at
execution with an error result the model can read and route around. Removing
tools per mode would give each mode a different prefix and make every mode
switch a cold cache ([CACHE.md](CACHE.md)). The one thing that does vary the
array is the provider — DeepSeek's sixteen, Kimi's fourteen — and that is a
per-session property, fixed at creation and never changed mid-session, so it
never varies within a provider's sessions ([CACHE.md](CACHE.md)).

A denial is not an error. It returns as a tool result naming the rule that
refused it, which is the shape the model recovers from — it picks a different
approach rather than retrying the same call. Denials are recorded as their own
event kind so they are findable after the fact.

The CLI registers an interactive resolver against the same decision point, so a
terminal user is prompted for calls the policy would otherwise deny. Nothing
else registers one. Whether a session can ask a human is a property of its
caller, and every other part of the loop is identical.

## Context and compaction

DeepSeek's recommended Claude Code configuration sets
`CLAUDE_CODE_AUTO_COMPACT_WINDOW=786432` — compact at 768K of the 1M window.
Take that number as their guidance rather than deriving our own.

Compaction rewrites history and therefore resets the prompt cache (§3.2). Both
things are true and they reconcile as: compaction is a session boundary, not an
edit. At 768K the harness opens a new session seeded with a summary, links it to
the parent, and shows the boundary in the UI. The cache reset is deliberate and
visible rather than an unexplained cost spike.

## Deferred

Background shells with output polling and kill. Notebook editing. MCP tool
import. FIM-backed inline completion. Edit checkpointing and rollback. Each is
additive against this set.
