# Tool specification

## Why these names

The names follow the vendor's own harness,
[`deepseek-ai/deepseek-harness`](https://github.com/deepseek-ai/deepseek-harness)
(`dsh`). It names every built-in tool `lower_snake_case`: `read`, `write`,
`edit`, `bash`, `glob`, `grep`, `job_output`, `job_list`, `job_kill`,
`web_fetch`. It ships `apply_patch` too.

`apply_patch` is the one tool name DeepSeek's own API documents as tuned:
`{"type": "custom", "name": "apply_patch"}` is accepted on the Responses API
and any other custom name returns 400
(`third_party/deepseek-docs/guides/responses_api.md`, `Tools`). The harness
sends `apply_patch` as an ordinary function tool on the surface it uses, and
the name is the one the model has seen.

That is the evidence. `dsh` sits closer to the model's training data than an
inference drawn from which harnesses DeepSeek names as integration targets:
DeepSeek-V4 shipped with agent optimisations and names Claude Code, OpenClaw
and OpenCode as targets, but naming them says nothing about what they call
their tools. This document followed Claude Code's `TitleCase` set for that
reason until the vendor shipped its own harness. Where the two disagree, the
vendor's own wins.

Function names must match `[a-zA-Z0-9_-]{1,64}` and a request may carry at most
128 of them.

One caveat on the evidence, recorded in full in [VALIDATION.md](VALIDATION.md).
The vendored docs establish that DeepSeek optimised V4 for agentic coding, and
that `apply_patch` is a name it accepts. They say nothing about the rest of
`dsh`'s catalogue; those names come from the harness's own repository, outside
this one. That makes them the weakest load-bearing assumption in the design,
and also a cheap one to be wrong about, since a rename is the whole fix.

## The built-in set

| Tool | Arguments | Purpose |
| --- | --- | --- |
| `read` | `file_path`, `offset?`, `limit?` | Read a file, line-numbered |
| `write` | `file_path`, `content` | Create or overwrite a file |
| `edit` | `file_path`, `old_string`, `new_string`, `replace_all?` | Exact-match replacement |
| `apply_patch` | `input` | Apply a Codex-format patch to one or more files |
| `bash` | `command`, `timeoutMs?`, `description?`, `run_in_background?`, `workdir?` | Run a shell command |
| `job_output` | `job_id`, `filter?`, `wait?` | Read a background job's output since the last read, optionally waiting for it to finish |
| `job_kill` | `job_id` | Stop a background job |
| `job_list` | none | List every background job with its command, start time and state |
| `glob` | `pattern`, `path?` | Path matching by pattern |
| `grep` | `pattern`, `path?`, `glob?`, `type?`, `output_mode?`, `-i?`, `-n?`, `-A?`, `-B?`, `-C?`, `multiline?`, `head_limit?` | Content search, ripgrep-backed |
| `list` | `path`, `ignore?` | Directory listing |
| `task_create` | `tasks[]` | Append tasks to the working plan |
| `task_get` | `taskId` | Fetch one task by taskId |
| `task_list` | `status?` | List the working plan (optionally one status) |
| `task_update` | `taskId`, `status?`, `subject?`, `description?`, `activeForm?` | Patch one task by taskId, or remove it with `status: "deleted"` |
| `task` | `description`, `prompt`, `subagent_type` | Delegate to a flash-backed subagent |
| `web_fetch` | `url`, `prompt` | Fetch a URL and extract against a question |
| `screenshot` | `url`, `path`, `width?`, `height?`, `device_scale_factor?`, `color_scheme?`, `full_page?`, `selector?`, `wait_for_selector?`, `wait_ms?`, `actions?` | Capture a page with a headless browser into `scratch/` |
| `glance` | `image_paths[]`, `query?`, `ocr?`, `ocr_extra?`, `conversation_id?`, `region?` | Ask Gemini's vision model about one or more images: describe, answer a question, or transcribe the text |
| `transcribe` | `image_path`, `region?` | Read all the text off an image too tall for one look: chunk it on blank bands, one vision call each, merged with a seam report |
| `ground` | `image_path`, `target`, `region?` | Locate something in an image; every match comes back as a pixel box |
| `detect` | `image_path`, `category?`, `region?` | Inventory every instance of a kind of thing in an image; a numbered list of pixel boxes |
| `crop` | `image_path`, `region`, `output?`, `scale?` | Cut a pixel box out of an image into its own file: local, no model call |
| `complete` | `summary`, `result?`, `status?` | Emit the run's machine-readable result |

Most of these have a trained-in analogue in `dsh`. `list`, the vision path,
`complete` and the four task tools do not. The six vision tools exist only in
the base array: every model this harness routes reads images natively, so no
routed session is offered them, and `internal/tools/definitions.go` is where
that is decided. `complete` and the task tools are this harness's own run
machinery.

The array is chosen per model, through `provider.SeesImages` and
`internal/tools.DefinitionsFor`
(docs/KIMI-INTEGRATION.md §4.5, decision 5; docs/DEEPSEEK-VISION.md), and
`internal/tools/definitions.go` is the authority on what each one holds:

- **Kimi, Gemini and Claude** carry the full set minus the six vision tools
  (`screenshot`, `glance`, `ground`, `detect`, `transcribe`, `crop`), and keep
  all seven harness-added tools (`list`, the four task tools, `task` and
  `complete`): those models read images natively, so all six vision tools are
  redundant, and capture happens
  through `bash` and the `playwright-cli` skill instead. Claude's array drops
  `web_fetch` as well, because Anthropic's own server-side `web_search` and
  `web_fetch` ride every Claude request instead.
- **DeepSeek** carries the same vision-free set minus `list`, `task_create`,
  `task_get`, `task_list`, `task_update`, `task` and `complete`. None of the
  seven has a counterpart in `dsh`: `list` is a directory listing `glob` and
  `bash` already cover, the four task tools are this harness's plan machinery,
  `task` is its blocking subagent, and `complete` is its run-ending report.
  The array being aligned with the model's own harness is the one DeepSeek is
  offered, so the harness-added tools come off it. A DeepSeek session cannot
  keep a task list, delegate, or end with a structured result; `Runner.Run`
  refuses a create that asks for a result schema and resolves to an array
  without `complete`, naming the missing tool.
- Any other model gets the base array, unchanged byte for byte: `Definitions`
  returns it and an unknown name falls back to it. No routed model is sent it,
  and request validation rejects an unknown name before a session exists.

Each array is a frozen request head shared by every session that sends it,
pinned by its own golden file
(`internal/tools/testdata/tools_*.golden.json`, asserted by
`TestToolArrayGolden`); the `read` section below covers how an image reaches
a model that can see one.

## Per-tool notes

### read

Returns line-numbered content, `cat -n` style, because that is the shape the
harnesses this vocabulary comes from return and the model reads offsets out of
it.

A file under a skill directory discovered outside the workspace opens like
any other, which is how the model reads a skill installed for the user. The
catalogue names such a skill by its absolute path, and its directory is the
allowance the read-only tools resolve against (see "Execution rules").

`limit` bounds how many lines come back, and defaults to 2,000. A file longer
than that is cut, and the cut is marked in the result:

    [showing lines 1-2000 of 2145; read again with offset=2001 for the rest]

The note is the only thing that shows it. Nothing in the numbered lines says
where the file ended, and the output cap's own label does not cover this
either: 2,000 lines of source sit well under it, so a cut file comes back
looking exactly like a whole one. A model that believes it has seen the end
goes on to edit on that belief. The total after "of" is counted by scanning
the rest of the file, which is cheap beside the read that just happened; a
remainder the scanner cannot get through costs the total and nothing else,
and the note still reports the cut.

On a model that sees images (Kimi K3, Gemini, and `deepseek-flash`
— `provider.SeesImages`), a `read` of a PNG, JPEG, or WebP path returns the
file as an `image_url` part — the bytes base64-encoded into a
`data:image/<fmt>;base64,...` data URI, the exact shape the Kimi guide
prescribes (third_party/kimi-docs/guide/use-kimi-vision-model.md), with a
short text label naming the file alongside. The encoded size is capped by the
existing screenshot cap setting (`tools.reviewscreenshot_max_bytes` — the
vision tools keep this setting's original name, docs/VISION-TOOLKIT.md — the
same 5 MB default they enforce per file), and an over-cap image is
refused with a result naming the limit and suggesting a resize — a refusal
the model can act on, never a failed run. The image bytes are stored on the
`tool_result` event, so the fold replays them identically
(docs/KIMI-INTEGRATION.md §4.5). An SVG reads as text even on a vision-capable
model — it is XML, and the API prescribes sending the source as text — and
any other binary type is refused with a message that says so. On a model
with no vision capability the behaviour is unchanged: an image is a binary
file and is refused like any other.

This creates a known friction with `edit`: the line-number prefix is display
only and must not appear in `old_string`. Detect a leading line-number pattern
in `old_string` and return a targeted error naming the problem rather than a
generic no-match. The model corrects on a specific error and flounders on a
vague one.

### write

Overwriting an existing file requires that the session has already read it. The
gate forces the model to work from real bytes rather than remembered ones.
Creating a new file needs no prior read.

### edit

The highest-risk tool, and the one where harnesses diverge most in quality.

- `old_string` must match exactly once unless `replace_all` is set. On multiple
  matches, return the count and refuse. On zero matches, return the nearest
  candidate line range if one exists.
- A prior `read` of the file is required.
- The result carries the applied diff, so the model sees what actually landed
  rather than assuming its intent was realised.

### apply_patch

One call for an edit that spans files or separated regions. `input` is a patch
in Codex's format, the one `dsh` uses and the one the name is tuned for:

    *** Begin Patch
    *** Update File: internal/tools/policy.go
    @@ func (p *Policy) Check
    -	old line
    +	new line
    *** Add File: internal/tools/patch_test.go
    +package tools
    *** End Patch

The grammar is `*** Add File:`, `*** Update File:` with an optional
`*** Move to:`, and `*** Delete File:`. A hunk is a `@@` line followed by
space (context), `+` (add) and `-` (remove) lines. Text after `@@` anchors the
hunk after the first line matching it, and `*** End of File` pins the last hunk
to the end of the file.

Every operation is staged against an in-memory copy of every file the patch
names before a byte is written, so one hunk that does not match fails the whole
call and leaves the workspace exactly as it was. Writes are committed before
deletes. Paths resolve against the workspace root and go through the same
escape check `write` and `edit` go through.

The prior-read gate is the one `write` and `edit` carry: every existing file
the patch changes or deletes must have been read in this session. A file the
patch creates needs no read, and neither does a file an earlier operation in
the same patch created. The patch's own context lines are not a substitute for
the read. A binary file cannot be deleted through the tool, because `read`
refuses a binary and the gate can never be satisfied; use `bash` for that.

Hunk matching is exact. Trailing whitespace is not normalised, and a
unified-diff header (`@@ -1,3 +1,3 @@`) is read as anchor text and will not
match: the anchor form this grammar takes is `@@ <context>`.

The tool ships enabled beside `edit`. The `no-patch` prompt variant drops it
(`internal/promptvariant`), which is how the two editing tools get compared on
the same tasks.

### bash

The tool the permission policy exists for. Wall-clock timeout and an output byte
cap on every invocation, with truncation labelled in the result so the model
knows it saw a fragment. Both are defaults, not fixed values: an operator
changes them in the settings table:
`tools.bash_timeout` (and `tools.bash_timeout_max`, the ceiling a request's
own timeout is clamped to) and `tools.output_cap` — the whole `tools.` group
of settings — and the next tool call picks the change up without a restart.

Because both numbers are settings, neither appears in the tool description:
the description is part of the frozen request head, and a number an operator
can change must not vary the head per installation (docs/CACHE.md, "Never
quote a configurable limit in a tool description"). The expiry message is
where the model learns the real bound instead. It names the limit that
actually applied, and says when that limit is not the one the call asked for:

    command timed out after 10m0s; 30m0s was requested and clamped down to
    the harness ceiling, so asking for longer will not help

    command timed out after 2m0s, the default for a call that names no timeout

A model told only "command timed out" reads a clamped request as a hung
command and retries it unchanged. The `timeoutMs` argument's own description
carries the same three facts without the numbers: there is a default, there is
a ceiling, and a request above the ceiling is clamped rather than refused.

Commands run through `bash` where the machine has one and `/bin/sh` otherwise,
resolved once at first use. The image installs bash, so a session gets it;
models write `${PIPESTATUS[0]}`, `[[ ]]` and arrays regardless of what the
shell is, and busybox ash answers those with a syntax error.

`workdir` names the directory the command runs in, absolute or
workspace-relative, and defaults to the workspace root. It goes through the
same escape check every other path argument does, and must name a directory
that exists. It applies to a `run_in_background` call as well.

Foreground by default, and that is the wrong shape for a dev server or a
test watcher: those need `run_in_background`, which starts the command
without waiting for it, returns a job id straight away, and lets
`job_output`, `job_kill` and `job_list` act on it afterward. A
backgrounded command is not tied to the tool call's own timeout: it runs
under its own process group, exactly like a foreground command. The shell
belongs to the session rather than to the run that started it: `job_output`
in the next run still finds the same `job_id`, and only `job_kill`, a
stopped or failed run, or the process ending ends it before it exits on its
own. Compaction moves the registry to the successor session, so a shell
started before or after the fork stays addressable by the session that is
still running. A shell that exits is reported as a `background_finished`
event, which the model reads as a user message at the next sub-turn
boundary, and which starts a run of its own when the session is idle.
`job_output` returns only what a shell has produced since the last time it
was read (never the whole buffer again), so polling a long-lived process
does not re-send, or re-bill, output the model has already seen.

`job_output`'s `wait` blocks until the job finishes or the call's own timeout
expires, and a wait that runs out returns the output so far and leaves the job
running. `job_list` reports every job the session holds, in start order, with
its command, start time and whether it is still running, which is the answer to
"what did I leave running" without a guess at an id. `job_kill` on a job that
has already finished is a no-op.

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

### grep and glob

`grep` runs ripgrep. Claude Code's own `Grep` bundles a ripgrep binary and
execs it, and the shape the trained-in vocabulary expects is ripgrep's own, so
this tool hands ripgrep's stdout back as it printed it: `path:line:text` for a
match, `path-line-text` for a context line `-A`/`-B`/`-C` added, a bare `--`
between two blocks that are not contiguous, one path per line for
`files_with_matches`, `path:count` for `count`. `head_limit` cuts the first N
lines of that output and the output byte cap truncates it. Nothing else
re-labels a match.

The binary is resolved once, when the process starts: `-rg PATH` on either
subcommand, then `AGENT_HARNESS_RG`, then `rg` on the `PATH`. A path named by
the flag or the variable has to exist, and the process refuses to start
without it: a caller that names a binary is stating where ripgrep is, and
silently searching the tree instead would answer its searches out of the
wrong place. Nothing found anywhere means the Go walk below runs, which is
what a developer running `go run ./cmd/harness` without ripgrep installed
gets.

The arguments become flags. Always `--hidden`, `--max-columns 500` and
`--sort=path`, plus one `--glob !<dir>` for each version-control directory
(`.git`, `.svn`, `.hg`, `.bzr`, `.jj`, `.sl`), since `--hidden` otherwise
brings `.git` into every search. `files_with_matches` adds `-l --null`, and
`count` adds `-c -H --null`. Content mode adds `-H`, `-n` unless the caller
turned line numbers off, and `-C`, or `-B` and `-A`, when context was asked
for. `-i`, `multiline`, `glob` and `type` add `-i`, `-U --multiline-dotall`,
`--glob` and `--type`, and a pattern that starts with a dash is passed as
`-e <pattern>` so it is never read as a flag. `--sort=path` keeps the order of
results stable, so `head_limit` cuts the same lines twice, which ripgrep's
parallel walk would otherwise not guarantee. `--max-columns 500` replaces a
matching or context line at least 500 bytes long with
`[Omitted long matching line]` or `[Omitted long context line]`, so one
minified file cannot fill a result.

`grep` defaults to returning matching file paths; content and count modes are
selected by `output_mode`. Keeping the default cheap matters because the model
uses search to orient and would otherwise pull large content into a context
that gets re-sent every sub-turn. `-n` is on by default in content mode, so a
match a model reads carries the line number it needs to hand to `read`; an
explicit `-n: false` leaves the numbers off. `multiline` matches the file as
one string, with `.` matching newlines, the only way a pattern spanning more
than one line can match at all. `head_limit` applies last, across every mode,
the same `| head -N` shape whichever mode produced the lines it is cutting.

`path` names one file or one directory. Paths are reported as the caller
spelled them, with the file's own path underneath: a search of `src` reports
`src/main/prompts/cad.md`, a search of `.` reports `./src/main/prompts/cad.md`,
an absolute path stays absolute. A search of a directory the caller named is
therefore reported under a path the model can hand straight to `read`. Every
mode names the file each result came from, whether the root was a file or a
directory: `files_with_matches` and `count` print the path for a single file
anyway, and content mode passes `-H` so it does too. That is one deliberate
difference from raw ripgrep, which prints no path in content mode when one
file was named. Uniform output is worth more than byte-identical output when
the caller is a model that should not have to remember which shape a mode
returns for which kind of root.

`files_with_matches` and `count` are asked for `--null`, so a path containing
a colon is framed by a NUL byte and cannot be read as two fields; the path
list comes back one path per line. Content mode keeps ripgrep's
`path:line:text` exactly as printed, colons and all.

`type` is ripgrep's own type list, whose names run from `js` and `py` to
`haskell` and `elixir`; an unrecognised name is refused by ripgrep, which
names the one it did not recognise. The pattern is ripgrep's regex dialect,
Rust `regex`'s: no backreferences and no lookaround. That is a caller-visible
change from the Go `regexp` this tool used to compile patterns with, and it is
the engine the trained-in vocabulary assumes.

ripgrep's exit codes carry the outcome. 0 is matches, 1 is none and returns
the usual no-matches result, and 2 is a real error whose stderr becomes the
result — which is how a missing path comes back as a message naming it
instead of as a search that found nothing.

The Go walk is the fallback for a machine with no ripgrep, and it answers a
call the way the ripgrep path does: the same labels, the same separators, the
same long-line omissions.
`TestGrepFallbackMatchesRipgrep` runs a fixture tree through both and fails on
any difference. Two things it cannot mirror. `type` filtering is ripgrep's
table, so the fallback refuses a type filter by name instead of approximating
it with an extension list. And it does not read ignore files, which ripgrep
does inside a git repository, so a search of a gitignored directory returns
results on the fallback and nothing on the ripgrep path. Its own pattern
compiles as Go's `regexp` (RE2), the same no-lookaround, no-backreference
dialect, and it skips binary files the way ripgrep's walk does.

`glob` runs the Go walk and nothing else: it matches paths with its own glob
syntax and refuses a file as its root, since the walk never offers its own
root as a match.

### task_create, task_get, task_list, task_update

`dsh` carries `todo_write` instead of these four, and the harnesses this
vocabulary came from all carry a todo tool, so the model will reach for one
either way. Where they write the whole plan array on every change, this
harness splits that into a CRUD family: `task_create` appends tasks (ids
minted in call order and stable for each task's whole life), `task_update`
patches or removes one task by taskId, and `task_get`/`task_list` read one
task or the whole plan back.
The field names match Claude Code's own task tools: each item carries
`subject` ("a brief, actionable title"), `description` ("what needs to be
done"), and `activeForm`, and the task identifier is `taskId`. The deliberate
deviations are that `task_create` keeps its batch-array shape
(`{"tasks": [...]}`) so seeding a whole plan is one call, and `task_list`
keeps its optional `status` filter. No dependencies, `owner`, or `metadata`:
a harness session is a single agent, not a team with tasks to claim.

These four are this harness's own; DeepSeek's array does not carry them (see
"The built-in set").

The split exists so a per-item update is cheap enough that an agent does not
skip it mid-run. Rewriting the whole plan on every status change costs the
model the full checklist to produce and the session a bigger message to
append; naming one taskId in a short `task_update` call makes keeping the
plan current the obvious move. A terminal client renders the list as a
checklist that scrolls away; a client with a screen can pin it as a live plan
panel beside the transcript.

`task_update` patches only the fields present. `status: "deleted"` removes the
task — it is only ever an input trigger, never a stored status, so a
"deleted" combined with a patch, an update with nothing to set, an unknown
taskId, or an invalid status are all errors and mutate nothing — the same
validation the event-log replay applies (`internal/store/status.go`), so a
session's recovered plan can never drift from what the loop actually did.
`task_get`'s single-item result shows the task's `description` too, which the
compact checklist line deliberately leaves out.

### task

Delegates to a subagent on `deepseek-flash` (`model.flash`). DeepSeek's own
recommended Claude Code configuration sets
`CLAUDE_CODE_SUBAGENT_MODEL=deepseek-v4-flash` — `deepseek-flash`'s retired
name — so this is both the trained-in pattern and the cheap one.

The subagent runs in its own conversation. Its transcript never joins the parent
message array — only its final result does. That keeps the parent prefix stable
and avoids mixing caches across models.

### web_fetch

Ours, not DeepSeek's. See "Web search is not available on the surface we
speak" below. Fetch, extract to text,
then have flash answer the caller's `prompt` against the extracted content, so
the parent context receives an answer rather than a page. The fetch size cap,
the extraction cap, and the call's timeout are defaults under the `tools.`
group of settings (`tools.webfetch_max_body`, `tools.webfetch_max_extract`,
`tools.webfetch_timeout`).

### screenshot

The capture half of the vision path. It drives a headless Chromium to a URL
and writes one PNG or JPEG, which `glance`, `ground`, or `detect` can then
send to Gemini and the transcript renders inline.

The harness owns the capture rather than leaving the agent to compose a
Playwright invocation through `bash`, because the settings that decide whether
a screenshot is worth anything — the viewport, the colour scheme, whether the
image is clipped to the element actually in question — are exactly the ones an
ad hoc invocation omits. An agent left to its own devices takes a full-page
capture of a desktop-width page in the light scheme and reviews that, which is
the one combination least likely to show a problem.

So the defaults are the standard:

- **The viewport, not the full page.** `full_page` is off. A full-page capture
  of a long document is downscaled to the same token budget as a viewport one,
  so every control on it shrinks until it is unreadable — the failure the
  `glance` notes below describe. When a viewport capture cuts the
  document off, the result says so, with both heights, so the model knows what
  it did not see rather than concluding the page ends there.
- **`selector` clips to one element.** This is the answer to "the button looks
  wrong": capture the button. It is also why the tool drives the Playwright
  library through an embedded Node script rather than the `playwright
  screenshot` CLI, which can neither clip to an element nor set a device scale
  factor.
- **`device_scale_factor` stays at 1.** The image's next stop is a vision model
  that downscales it regardless, so doubling the pixels doubles the bytes
  against the vision tools' per-file cap and buys nothing in what Gemini
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
  production run used `screenshot` zero times and drove Playwright through
  `bash` for all of it — which is exactly the ad hoc invocation the tool
  exists to replace, so the capture went back to being full-page, desktop-
  width and light-scheme with none of the standard above
  ([`docs/reviews/vision-path-2026-08-14.md`](reviews/vision-path-2026-08-14.md)).

Output is confined to `scratch/`, the directory the system prompt already
reserves for files that are not part of the deliverable. That confinement is
what lets the tool run in a read-only session (see "Permissions"): the capture
cannot land in a cloned repository whatever the mode.

A **relative path is taken as relative to `scratch/`**, so `after/01.png`
writes `scratch/after/01.png`. The prefix the schema asks for is still what a
caller should write, and the result line names the absolute path it wrote, so a
relocated capture is visible rather than silent. Dropping the prefix used to be
a refusal, and the refusal only cost sub-turns: one run capturing sixteen
screens lost the prefix on three separate batches, twelve calls in all,
correcting itself each time and forgetting again after the next success. The
invariant that matters is that the bytes land under `scratch/`, and joining the
path there satisfies it exactly as the explicit spelling does. An **absolute**
path is left as given — it is a statement about where the file goes, so one
outside `scratch/` is still refused rather than quietly re-rooted.

The reading side matches, or the pairing would be pointless: `glance`,
`ground`, `detect`, and `crop` all retry a relative image path that names
nothing under `scratch/`, and so does the transcript's own image endpoint (see
"Seeing the screenshots"). A path that does resolve to a real file is never
second-guessed, so a file at the workspace root is never shadowed by a
same-named one in `scratch/`.

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

### glance

This is the general-purpose vision tool for a model that cannot see images
itself, every model `provider.SeesImages` resolves false. No model this harness
routes resolves false (`provider.SeesImages`, docs/DEEPSEEK-VISION.md), so no
routed session is offered it today; it stays in the base array for a provider
added later that cannot see. It sends one or more images to Google Gemini and
returns whatever comes back as prose — a description, an answer to a
question, or a verbatim transcription. It is one of four tools ported from `Anionex/agent-vision-toolkit`
(docs/VISION-TOOLKIT.md is the assessment behind the port) that between them
replaced this harness's own `ReviewScreenshot` and `AskVision`, and it is
designed to be used with the other three in sequence: `screenshot` captures a
page, `ground` locates a spot on it, `crop` cuts that spot out, and `glance`
reads the crop closely — capture, locate, cut, read. A caller that only ever
calls `glance` against a full page is skipping the rest of that pipeline, and
paying for it in how much of a busy screen actually gets seen.

It accepts PNG, JPEG, or WebP files, workspace-confined like every other
path-taking tool, and a relative `image_paths` entry that names nothing at
the workspace root is retried once more under `scratch/`, the same as
`screenshot`'s own output (see "screenshot"). The image count and the
per-file size cap are defaults, not fixed values (`tools.reviewscreenshot_max_images`,
`tools.reviewscreenshot_max_bytes` — the vision tools kept these setting
names from the tools they replaced, so an operator's existing configuration
survived the port unchanged, `internal/tools/vision.go`), so the tool description
quotes no numbers — it is part of the frozen request head (docs/CACHE.md) and
a number there would make the head vary per installation. A call that
exceeds a bound is refused with an error stating the actual limit, which is
how the model discovers it.

The first image is sent at `high` resolution and the rest at `medium`, per
the prompting notes' advice that only the image needing scrutiny should be
high — the tool description tells the model to put the screenshot it cares
about first
([`docs/gemini-3.5-flash-ui-review-prompting.md`](gemini-3.5-flash-ui-review-prompting.md)
has the request-shape rationale: no temperature/top_p/top_k, `thinking_level`,
"data first, question last"). An image over the byte cap is downscaled rather
than refused — decoded and shrunk, preserving aspect ratio, until the
re-encoded bytes fit — and the answer is prefixed with a note naming which
file was downscaled and to what dimensions, so the model knows it saw less
detail than the file holds before it reads what it concluded. A full-page
capture downscaled this way leaves a small control unreadable, which is the
case `region` exists for: crop to the spot in question (with `ground` or
`crop`) and `glance` the crop at full resolution instead of the whole page.

`query` and `ocr` are mutually exclusive. `query` sends exactly the question
written, with no fixed instruction wrapped around it; `ocr` transcribes every
piece of visible text verbatim, line by line, with `ocr_extra` for additional
requirements; omitting both describes the image or images in general — each
labelled "Image 1", "Image 2" when there is more than one (a text part
carrying the file's base name precedes each image part), so an answer about a
multi-image comparison says which one it means. `region` crops the one image
to a pixel box before sending, sending only the crop and nothing else — the
way to look closer at a spot `ground` or `detect` already found; it works
with exactly one image path.

An image can also arrive with the task rather than being captured mid-session.
A create body may carry attachments: the bytes are stored in SQLite, the
request carries only ids, and `internal/attachment.Write` materialises them
into `scratch/attachments/` before the loop starts. The opening message names the files,
so the model knows they exist and can pass one to `glance` as the image to
compare against — "make it look like this mockup" becomes a question against
the mockup itself instead of a prose description of it. Only PNG, JPEG, and
WebP are accepted, capped in count and per-file bytes by
`tools.attachments_max_count` and `tools.attachments_max_bytes`.

How hard the vision model thinks defaults to medium — a real question
deserves reasoning — and truncation is the plain byte cap: safe here because
there is no JSON document to sever. The model comes from the
`google.vision_model` setting (default `gemini-3.7-flash`) and the key from
`google.api_key`, both read through the settings table on every call so
either can change without a restart; the call has its own timeout (120s,
`tools.reviewscreenshot_timeout`) rather than the 30-second tool default. An
operator can override the thinking default with `google.vision_thinking_level`;
there is no longer a per-call `thinking_level` argument — `glance`, `ground`,
and `detect` each pick their own default and the operator's setting is the
only knob left over it.

#### What was lost, and what to do instead

`glance` is one tool doing the work `ReviewScreenshot` and `AskVision` used
to split between them, and it inherits neither tool's guarantees. Gone: the
structured findings list, each carrying a `confidence` and the screenshot it
concerned; the instruction that told Gemini to judge only against a supplied
`spec` and treat everything else as intentional; the required `observed`
sentence that stopped a clean answer from being mistaken for a blank page
(the fix for a real, measured failure —
[`vision-path-2026-08-14.md`](reviews/vision-path-2026-08-14.md), one session
spent 23% of its cost re-asking to find out a review of nothing was a review
of nothing); and `conversation_id` follow-ups that let a review continue
without re-uploading the images.

A session that needs any of that now has to build it into its own `query`:
state the standard being judged against, ask for a sentence on what is
actually on the screen before the verdict, and ask for findings in whatever
shape it wants back — as prose, not a validated schema. `screenshot`'s own
result still reports the page's title, console errors, and whether the
document ran past the viewport, which is real evidence a rendering-vs-review
question can be checked against without a second Gemini call. A multi-step
review — capture, judge, fix, re-capture, ask whether it is fixed — is now
several independent `glance` calls rather than one held conversation; each
has to restate its own context, because nothing on the harness side carries
it forward between them.

### transcribe

`glance` with `ocr` reads the text off an image in one call. `transcribe`
reads it off an image that one call cannot resolve.

The reason there is a difference is a fixed cost, not a size limit. An image
costs the vision model a flat ~1,120 input tokens at high resolution however
large it is — the documented budget
([`gemini-3.5-flash-ui-review-prompting.md`](gemini-3.5-flash-ui-review-prompting.md)),
and 1,121–1,195 per image measured across a live run
(docs/VISION-TOOLKIT.md §7). So a 1200×12000 page capture gets the same
visual budget as a 1200×800 viewport shot: roughly fifteen times less detail
per unit of page. That is not theoretical — a live `glance` describing a
"whole page" covered the 800px viewport rather than the 2,640px document, and
said so unprompted. Cutting the image up buys effective resolution that no
other argument to any of these tools can.

**The cut.** The tool measures, for every row, how many pixels differ from
that row's own background — its left and right margins, which is what makes
the measurement work unchanged on a dark theme or a page with a coloured
band. It then walks down the image choosing each cut inside a window around a
target height (scaled from the width, so the pieces come out roughly
page-shaped rather than ribbon-shaped), preferring the middle of the widest
run of near-blank rows it can find there. A cut in the leading between two
lines is clean by construction: a glyph crossing it would have put ink on
those rows.

**The overlap.** A clean cut gets no overlap at all — there is nothing at the
boundary for two chunks to see twice, and showing them the same strip only
gives the merge a chance to delete something real. A cut that had nowhere
clean to land (a dense table, a terminal capture) gets a fixed overlap on
each side, and that is the *only* case in which the merge dedupes anything.

**The merge.** Chunks are joined in order, dropping from each the longest run
of opening lines that exactly repeats the tail of what is already merged —
compared case-folded with runs of whitespace collapsed, and nothing else. A
run of blank lines does not count as a repeat. There is no fuzzy fallback:
upstream reaches for `difflib.SequenceMatcher` at this point, and a ratio
threshold on prose is a knob that silently deletes a real line for resembling
its neighbour. Under-deleting leaves a visible duplicate; over-deleting
leaves silence, which is worse.

**The seam report**, which is the point of the tool as much as the transcript
is. Every call returns, *ahead of* the text — so the output cap can never cut
it off — where the image was cut, in the source file's own y coordinates,
what happened at each boundary, and which boundaries are marked `CHECK`. A
boundary is flagged exactly when overlap had to be sent, which is exactly
when the cut went through content. The flag does not soften when the exact
match fires: this tool cannot check its own merge, and a wrongly merged seam
reads as ordinary prose rather than as anything broken. The y positions are
reported against the original file (the `region` offset applied) precisely so
a flagged seam can be handed to `crop` and looked at without arithmetic.

**Failure is all-or-nothing.** A chunk whose call fails fails the whole call,
with no partial transcript: a document with one chunk's worth missing from
the middle reads as a complete document, which is the exact failure the seam
audit exists to prevent, and it would be perverse to guard the boundaries and
then ship a gap. The chunks that did come back were billed, so their usage
still rides home on the failed result.

**Cost is one event, summed.** Every chunk is its own Gemini request, and
they are sent concurrently (`tools.transcribe_concurrency`, four at a time by
default) under one budget for the whole call (`tools.transcribe_timeout`,
300s — not the per-call vision timeout, which would kill a tall page
part-way through and bill for what had already returned). The chunk count is
capped by `tools.transcribe_max_chunks`; an image that would need more is not
refused, the split just stops and the last chunk keeps the remainder, which
the per-chunk heights in the report make visible.

The usage of all of them is summed into a **single** `usage` event carrying
`calls`, rather than one event per chunk. Both would give the right session
total — `SessionUsageSummaries` sums every usage event it finds — but the
transcript card absorbs at most one usage block per sub-turn and a later one
replaces an earlier one, so fifteen events would put one chunk's price on the
card and drop the other fourteen from the display. Vision spend has already been a third of a run's cost once
([`vision-path-2026-08-14.md`](reviews/vision-path-2026-08-14.md)), and
under-reporting it on the screen where anyone would notice is a worse trade
than losing the per-chunk breakdown, which nothing downstream reads and which
the result's own text carries anyway. `calls` exists so a summed event cannot
pass itself off as one enormous request. Any future tool that makes more than
one provider call inherits this: sum, or the card shows whichever call
happened to be last.

**What the live runs actually measured**, because the premise above is not
the whole story. Three runs on 2026-08-15 confirmed the flat budget twice —
one call on an 8,370px capture took 1,172 input tokens, one on a 3,709px
capture took 1,097 — and then found that chunking recovered **no more text**
than a single `glance` with `ocr` on either page: 44 setting keys against 43,
and on the shorter page an exact tie. `gemini-3.7-flash` reads a tall rendered
web page about as well in one call as in five. What `transcribe` did win on
the taller page was **cost** ($0.0209 against $0.0383, because five bounded
per-chunk outputs came to less than one unbounded 9,984-token one) and the
seam report, which is a statement about the transcript's own reliability that
a single `glance` does not make. Neither target had genuinely small text — a
scan, a dense spreadsheet, a chat log — which is the case §6 of
docs/VISION-TOOLKIT.md predicted and which remains untested. Prefer
`transcribe` for a long document you intend to rely on; do not assume it
recovers text a `glance` would miss until that has been measured on the kind
of image in front of you.

Every chunk goes at high resolution — buying resolution is the intent of the
split, and sending the later ones at medium would hand back whatever it does
buy. Thinking defaults to low: transcription is copying what is visible, and
budget spent deciding what the text *means* is budget spent wrong. The tool
writes nothing to disk, so like `glance`, `ground`, and `detect` it runs in
every permission mode.

### ground and detect

Where `glance` answers in prose, `ground` and `detect` answer in pixels:
"where is the submit button" gets back a box, not a description of roughly
where it is. Upstream, `detect` is `ground` with a canned target and no other
logic at all (docs/VISION-TOOLKIT.md §6), and this harness ports them the
same way — one implementation (`runLocate` in `internal/tools/vision.go`)
behind two tool names, sharing every property below.

`ground` takes `target` — anything nameable, a labelled control or a region
of the page — and returns every match. `detect` takes an optional `category`,
defaulting to UI elements generally (buttons, links, inputs, icons, labels,
headings, images, badges), and always numbers its results, with each entry's
label carrying the element's own visible text; `ground` prints a single match
bare and only numbers a list when there is more than one. Neither guesses:
"no match found" (`ground`) or "no elements detected" (`detect`) is a real
answer, not an error.

Both accept exactly one image — PNG, JPEG, or WebP — with the same workspace
confinement and `scratch/`-relative retry as `glance`. `region` narrows the
search to part of the image before sending, useful when roughly where to
look is already known; the returned boxes are reported in the original
image's coordinates either way (see below). An oversize image or crop is
downscaled the same way `glance`'s is; unlike `glance`, neither tool surfaces
a note about it in the result — the coordinate contract below means the
returned box still lands correctly regardless, though the model's perception
of a downscaled image can still be worse than of the original. Thinking
defaults to low rather than `glance`'s medium: locating something is
perception, not reasoning, and it is most of what a call costs.

#### The coordinate contract

Gemini returns boxes on a 0-1000 grid, `[y0, x0, y1, x1]`, resolution-independent
by construction. The harness scales each returned box by the dimensions of
the image actually sent — the crop's dimensions when `region` was given, the
whole file's otherwise — never by whatever dimensions survived a byte-cap
downscale, and reports every box back in the **original** file's pixel
coordinates. That is what makes the byte cap harmless for locating: an image
downscaled to fit under the cap still returns a box a caller can use directly
against the file on disk, even though perception itself degrades with the
downscale (docs/VISION-TOOLKIT.md §6).

The reply is parsed tolerantly rather than validated against a schema: a
```` ```json ```` fence is stripped, a bare array or an object wrapping one
under `boxes`, `bounding_boxes`, `bboxes`, `objects`, `items`, or `results` is
accepted, and failing that, a regex scavenger pulls box-shaped objects out of
surrounding prose. Constraining the reply's shape with `response_format` has
been measured to empty or mangle the contents instead
(docs/VISION-TOOLKIT.md §6), so the call sends none and leans on the parser.
A reply that still will not parse comes back as the model's raw text,
labelled as unparsed rather than passed off as structure — the usage and
cost still ride home, because the call was made and billed whatever the
answer turned out to be.

Each match reports a coarse position (`top-left`, `center`, `bottom-right`,
and the rest of that grid, from the box's centre) alongside its own
`x1: … y1: … x2: … y2:` pixel box. Feed a box straight to `crop` for a
full-resolution look at just that spot, or to `glance`'s `region` to ask a
question about it instead of the whole page.

The model, the key, the settings that decide either, the call's own timeout,
and the cost accounting below are all shared with `glance` unchanged.

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
invented `$0.0000` reads as "this was free". `glance`, `ground`, and `detect`
all attach the same line the same way.

### crop

Local image manipulation: no model call, no cost, no usage event. It cuts
`region` — the same `x1,y1,x2,y2` shape `ground` and `detect` report — out of
`image_path` into its own file, the way a box either tool located becomes an
image `glance` can read at full resolution instead of as a few pixels of a
whole page. `output` defaults to the source's own name with `.crop.png`
appended, written next to it; the extension of whatever path is given
chooses the encoding, falling back to the source's own format when it names
nothing the harness can encode. `scale` enlarges the cut region by an integer
factor from 1 to 8, for a source small enough that a plain crop would still
be hard to read; the resample is `x/image`'s CatmullRom, the same resampler
the byte-cap downscale uses standing in for upstream's LANCZOS.

`crop` writes a file, which is the one thing the other vision tools do not,
and it is still in read-only mode's always-allowed set. What earns it that is
where it is allowed to write: `output` resolves through
`resolveScratchImageOutput`, the same `scratch/`-confined rule `screenshot`'s
path uses, so a relative path lands under `scratch/` whether or not the
prefix is spelled and an absolute path outside it is refused. It therefore
cannot touch a deliverable or a cloned repository. Gating it by mode instead
— which it was, at first — put the whole `ground`→`crop`→`glance` pipeline
behind `full` permissions: a large grant to buy a closer look at a screenshot
(`internal/tools/policy.go`, "Permissions" below). It makes no Gemini call,
so it keeps the ordinary 30-second tool timeout rather than the vision
tools'.

### Seeing the screenshots

A `screenshot`, `glance`, `ground`, `detect`, or `crop` result names the
images it produced or read, so a client can render them above its text.
Without that a transcript reports what the vision model said about a page and
never shows the page, which leaves the one artefact that would settle whether
the model was right out of the record. `crop` is the odd one out among the
five: it makes no model call, and its `image_path` is usually already visible
from an earlier call in the transcript, so what is worth showing is what it
*wrote* (`output`, or the default `<stem>.crop.png` name `execCrop` falls back
to) rather than what it read — the produced crop, often upscaled, is the new
thing a reader has not seen yet.

The path is the one the model passed to the tool. A client resolving it should
do what the tools do: a relative path that names nothing at the workspace root
is tried once more under `scratch/`, which is where a relative capture lands.

A `task` child runs in its own workspace, so a subagent's screenshots resolve
against that one rather than its parent's.

### complete

The seam between an agent run and the queue that asked for it. `summary` is
prose for a human reading the transcript. `result` is the payload the requester
receives in `harness.work.result.<id>.final`. `status` distinguishes a finished
job from one the model gave up on, which is a different thing from a harness
error.

A call to `complete` ends the run. Parallel tool calls mean it can arrive
alongside others, so the rule is that the whole batch executes in `tool_calls`
order and the run ends after it.

Two consequences fall out of constraints stated elsewhere.

It cannot be forced. Thinking mode rejects `tool_choice: required` and named
tool forcing ([OBSERVED.md](OBSERVED.md)), so there is no way to make the model
call this before it stops. The system prompt asks for it, and a run that ends
without it returns its final assistant text with a null `result`. No error path
is needed for the omission.

Its schema never varies. When a create body supplies a `result_schema`, that
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
the last one is progress and restarts the count. Nothing counts sub-turns, so a
model cycling between several wrong shapes keeps going until it is cancelled.

Because the schema lives in the opening message rather than the tool definition,
that message also carries a worked example of the call built from the schema's
own field names — `complete(summary="…", status="done", result={"branch": …})`.
The schema alone does not say where its fields go, and the failure it invites is
sending them as top-level arguments beside `status`, which produces `result:
expected object, got null` — an accurate message about what is missing that says
nothing about what was sent. `execComplete` detects that specific case and names
the stray arguments.

`complete` ships in every session, including one a person starts and simply
watches, with no caller ever reading its `result`. One tool array across every
caller is what keeps the stable head shared.

### MCP tools

A server an operator has registered through the `/mcp-servers` screen
([`MCP.md`](MCP.md)) contributes its own tools, appended *after* the built-in
array rather than woven into the trained-vocabulary set. The whole point of
the array above is that its bytes never depend on what is or is not
configured, and MCP configuration is global rather than per-run, so what
gets appended is whatever was enabled the moment a session resolved its
array, not a per-request choice. A tool from server `blender` called
`get_objects_summary` is offered as `mcp__blender__get_objects_summary`, the
`mcp__<server>__<tool>` convention Claude Code uses.

Five fixed access tools sit between the two: `list_mcp_resources`,
`list_mcp_resource_templates`, `read_mcp_resource`, `list_mcp_prompts` and
`get_mcp_prompt`. They join an array only when at least one server is
configured, and [`MCP.md`](MCP.md) "Resources" has the table.

An MCP call is gated the same way `bash`, `write`, and `edit` are: it reaches
outside the workspace by definition, so `readonly` denies it unless the
server itself is marked `allow_readonly`, and `full` always allows it. Each
call is bounded by `tools.mcp_timeout` (120s by default, longer than the
ordinary 30-second tool timeout, because the calls that motivated MCP
support drive external applications — rendering a viewport, driving a
browser — rather than returning promptly). Whatever the server returns is
flattened the same way every time: text content joins into the result body
under the ordinary output cap, an image is written into the workspace at
`scratch/mcp/<server>-<tool>-<n>.<ext>` (`n` a running per-session counter,
so repeated calls to the same tool never collide) and named in the result so
a vision tool can be pointed at it, and a server-reported `isError` comes
back as an ordinary failed `Result` rather than a Go error — a tool call
that failed is something the model reads and routes around, never a reason
to end the run.

[`MCP.md`](MCP.md) is the full reference: registering and probing a server,
the naming and collision rules, and the permission table.

## Two decisions this rests on

### Strict mode is now per-tool, not global

`DESIGN.md` §4.6 said author every schema for strict mode from day one. That was
decided before the trained-vocabulary evidence.

Strict mode requires every object property to appear in `required` and
`additionalProperties` to be `false`. The trained-in schemas have genuinely
optional arguments — `read`'s `offset` and `limit`, `grep`'s `path` and `glob`.
Under strict those become `anyOf` null-variants that are still listed as
required, which is a different shape from the one the model was tuned against.

So: match the trained shape, non-strict, and validate arguments in Go. A bad
argument returns a clear error through the tool result channel and the loop
recovers — that channel exists for exactly this. Reserve strict mode for tools
where a malformed argument is dangerous rather than merely wrong. Strict is also
still Beta.

### Web search is not available on the surface we speak

`harness stdio-session` posts to `/responses`
([DEEPSEEK-RESPONSES.md](DEEPSEEK-RESPONSES.md)), and that surface gives a
model no server-side search tool. Its tool table supports `function` tools and
one `custom` tool, `apply_patch`; every built-in tool type, `web_search`
included, is listed as ignored
(`third_party/deepseek-docs/guides/responses_api.md`, `Tools`). DeepSeek's
Anthropic-compatibility endpoint carries `server_tool_use` and
`web_search_tool_result` blocks, and this process does not speak it.

So `web_fetch` is the harness's own function tool: it fetches a URL, extracts
the page to text, and has flash answer the caller's question against it. That
covers the documentation-lookup case a coding harness actually needs, and
DeepSeek's own note says its web search bills extra tokens for summarisation
anyway.

A server-side search tool would need a search provider, a credential and a
billing path of its own. That is a feature in its own right, not a field on a
tool definition.

## Execution rules

These hold for every tool and live in Go, not in prompt text.

- Paths resolve against the workspace root and are checked for escape after
  symlink resolution. Escapes are rejected, not sanitised. `read`, `list`,
  `glob` and `grep` have one read-only exception: a path under a skill
  directory `internal/skills` discovered outside the workspace resolves there,
  which is how a skill installed for the user is opened. `write`, `edit` and
  every other tool that changes the filesystem stay inside the workspace.
- Every tool has a wall-clock timeout and an output byte cap, with truncation
  labelled in the result. Both are settings (`tools.` group) with the defaults
  listed above; the values are resolved from the settings table on each call,
  so a limit changed in the settings table applies without a restart. One
  call site departs from the settings table: `harness claude-session`'s
  client-declared ("host") tools resolve asynchronously, on a client's own
  answer through `sessions.events` rather than on this process's own work, so
  that one process sets `tools.Timeouts.HostTool` to a figure with no natural
  ceiling instead (`internal/session.Runner.ToolTimeouts`,
  `docs/STDIO-MANAGED-AGENTS.md`, "The seam"). Every other tool, and this same
  namespace under `stdio-session`/`gemini-session`'s own blocking
  `harness.function_call`, keeps the ordinary MCP timeout unchanged.
- Tool results are appended in `tool_calls` array order, never in completion
  order. Parallel tool calling is always on and cannot be disabled: the
  Responses API guide states it outright, the Codex model catalogue declares
  `"supports_parallel_tool_calls": true` for both models, and the Anthropic
  table ignores `disable_parallel_tool_use`. Executing concurrently is fine;
  appending out of order is not, because it churns the prefix and costs the
  cache (`DESIGN.md` §3.2).
- Two calls that rewrite the same file run in order, not concurrently, and
  files are grouped separately so different files still overlap. `edit`,
  `write` and `apply_patch` each read whole files, change their own copy and
  write it back, so
  racing two of them on one path drops whichever finishes first while
  reporting both as successful — a silent lost edit, and one that was observed
  in a real run. Ordered, disjoint edits both land; an edit whose `old_string`
  the earlier one rewrote fails as it should, with a note naming the earlier
  call so the model does not go hunting for a typo. `tools.MutationTargets`
  decides what collides, by resolved path, and a patch reports every file it
  names, so a patch spanning two files is ordered against an edit of either.
- Never send `tool_choice`. Thinking mode accepts `auto` and `none` but rejects
  `required` and named-tool forcing, so no tool can be forced while thinking is
  on (`DESIGN.md` §4.4, [OBSERVED.md](OBSERVED.md)).

## Permissions

Permission is a policy the session is given at creation, not a question it
asks later. A run may have nobody watching it, so there is nobody to ask.

Two modes, fixed for the life of a session and required on every request:

- Read-only. `read`, `glob`, `grep`, `list`, `web_fetch`, `screenshot`,
  `glance`, `ground`, `detect`, `crop`, `transcribe`, `task_create`,
  `task_get`, `task_list`, `task_update`, `complete`, and the three MCP
  listing tools run. `write`, `edit`, `apply_patch`, `bash`, and `task` are
  denied. `screenshot` and `crop` are the two tools in that list that write to
  disk, and they are there because of where: their output is confined to
  `scratch/`, which holds nothing that is part of a run's deliverable, so a
  read-only session can look at a page it is reviewing without being able to
  mark a cloned repository. Reviewing a UI is the read-only run's whole job,
  and denying it the capture would have left it reading markup and guessing.
  `crop` used to be denied by mode; that put the whole `ground`→`crop`→`glance`
  pipeline behind `full` permissions, which is a large grant to buy a closer
  look at a screenshot, so its output moved under `scratch/` and it joined the
  readers (`internal/tools/policy.go`). The two MCP access tools that dial a
  server answer to the per-server allowance instead, which
  [`MCP.md`](MCP.md) "Permissions" covers.
- Full access. Everything runs, as this process's own user, in the workspace.

There is no third mode between them and no default. A create body that names
neither is rejected, so no configuration value decides a session's permissions
on a caller's behalf.

A middle mode existed until it was removed. It gated `bash` behind an
executable allowlist that included `go`, `npm`, `make`, and `python`, each of
which runs arbitrary code, so the boundary it drew was narrower than it
appeared. Read-only against full access is the distinction the harness can
actually enforce.

`web_fetch` runs in read-only mode. It reaches the network, so read-only bounds
what a session can change on disk rather than what it can send.
`glance`, `ground`, and `detect` are the same: each reads a file and sends it
over the network, changing nothing on disk. `crop` reads a file too, but it
writes one back — see above for why that puts it in the other group.

A create body may add `deny` patterns on top of its mode. They only ever
subtract; a request cannot widen the mode it asked for. A pattern matches the
call's descriptor: the shell command for `bash`, `"<Tool> <arg>"` for the
tools that name a path, URL, pattern, server or image, and the bare tool name
otherwise. `apply_patch`'s descriptor is its bare name, because a patch can
span files and there is no one path to name, so `-deny apply_patch` is the
pattern that keeps a session off the tool entirely.

Modes gate execution, never availability. All of a session's tools are sent
on every request in every mode, and a call the mode disallows is refused at
execution with an error result the model can read and route around. Removing
tools per mode would give each mode a different prefix and make every mode
switch a cold cache ([CACHE.md](CACHE.md)). The one thing that does vary the
array is the model, and `internal/tools/definitions.go` decides it through
`provider.SeesImages` and the provider table (docs/DEEPSEEK-VISION.md). That
is a per-session property, fixed at creation and never changed mid-session, so
it never varies within a provider's sessions ([CACHE.md](CACHE.md)).

A `stdio-session` create may also add an autonomy guard (`harness.guard`,
[`STDIO-PROTOCOL.md`](STDIO-PROTOCOL.md) "The autonomy guard"). It screens each
`bash` call under full access, after the mode and the deny patterns have
allowed it and before the command starts. Rules, then a local classifier (when
the policy has one), then a Claude model decide `allow` or `block` for the
command, and each decision is recorded in the event log. What the guard allows
and blocks is defined by the policy bundle the parent sends in
`harness.guard.policy`; this repository holds the mechanism and no policy. In `shadow` mode the model calls run beside the
command and never stop it; only the local read of the session's state comes
before it. In `enforce` mode a block is a denial like a deny
pattern: the command does not run, and the result names the guard and the
stage (and the rules, when they decided). It does not carry the Claude model's
reason, which goes to the log and the parent only. A create cannot select
`enforce`. Only `bash` is screened: the file tools already refuse paths outside
the workspace, and `web_fetch`, MCP tools and a client's function tools are
not checked. A command started with `run_in_background` is checked when it
starts and not afterwards. A `task` subagent runs under its parent's guard
setting and policy bundle.

A denial is not an error. It returns as a tool result naming the rule that
refused it, which is the shape the model recovers from — it picks a different
approach rather than retrying the same call. Denials are recorded as their own
event kind so they are findable after the fact.

A denial is unconditional: nothing consults a human or asks again before
returning it.

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

Notebook editing. FIM-backed
inline completion. Edit checkpointing and rollback. Each is additive against
this set.
