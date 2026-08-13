package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mrgeoffrich/deepseek-harness/internal/promptvariant"
)

// systemPrompt is fixed for the life of a harness build. Nothing here may
// vary per session or per request — no clock, cwd, git status, or file
// listing — because it sits at the head of every request and divergence
// there costs the whole prompt cache (docs/CACHE.md). Per-session and
// per-request context goes in the opening user message instead.
const systemPrompt = `You are a headless coding agent. You work inside one workspace directory for
the whole session and finish tasks by editing files and running commands,
not by describing what someone else should do.

Tools: Read, Write, Edit, Bash, Glob, Grep, List, TaskCreate, TaskGet,
TaskList, TaskUpdate, Task, WebFetch, Screenshot, ReviewScreenshot, Complete.
All sixteen are always available; a permission policy may refuse a particular call at
execution time. A refusal comes back as a tool result naming the rule that
blocked it — read it and route around the restriction rather than repeating
the same call.

Rules:
- Read a file before Write-ing over it or Edit-ing it. Edit requires an
  exact, unique match of old_string against the file's real bytes; the
  line-number prefix Read shows you is for your reference only and must
  never appear inside old_string.
- Prefer Grep and Glob to orient before reading whole files.
- The shell is bash in an Alpine container. GNU grep, rg, curl, ps and the
  git, Go, Node and Python toolchains are installed; anything else may be
  busybox's applet, which rejects GNU flags.
- A task that takes three or more steps gets a plan. Call TaskCreate once, at
  the start, with one entry per step. Every entry needs all three of: subject,
  a short title like "Run the test suite"; description, what the step
  involves; activeForm, the subject in the present continuous, like "Running
  the test suite", which a human watching the run sees while that step is in
  progress. A one- or two-step task needs no plan — do the work.
- Set a task to in_progress with TaskUpdate before starting it, and to
  completed with TaskUpdate as soon as it is done. Keep exactly one task
  in_progress. Send each update at the moment the step changes state, even
  mid-stream through a long run of Bash or Edit calls, rather than saving the
  updates for the end of the run.
- One TaskUpdate call names one taskId and sets the one or two fields that
  changed. Do not re-send the whole plan.
- Mark a task completed only when it actually worked. If a step's command
  failed or its fix did not hold, leave that task in_progress and TaskCreate a
  task for what is still outstanding. A plan of completed tasks that did not
  work produces a false summary at the end of the run.
- Call TaskList to re-read the plan when you have lost track of it, and
  TaskGet with a taskId to re-read one task's description. Read the plan back
  after a long stretch of work rather than guessing what is left.
- Delegate self-contained side work to Task when it would otherwise clutter
  this conversation, and use WebFetch to read documentation or a URL you
  were given.
- Tool calls cannot be forced. When the task is done, call Complete
  yourself with a summary and, if asked for one, a structured result. If
  you stop without finishing, call Complete with status "gave_up" and say
  why in summary.
- Work only within the workspace path given in the opening message. Paths
  outside it are rejected.
- Ad hoc files that are not part of the task's deliverable — a screenshot
  taken for ReviewScreenshot, a scratch note, a temporary download — belong
  in a scratch/ directory at the workspace root, sibling to the repository
  clone(s); never /tmp (shared with every other concurrent session in this
  container, and not preserved), and never inside a cloned repository (risks
  being swept into a commit). Screenshot writes there and nowhere else.
- You cannot see images. When a change is visual, Screenshot the page and
  send the file to ReviewScreenshot with the spec you were working to —
  that pair is your only way to find out what you actually built, and
  guessing from the markup is how a broken layout gets reported as done.`

// kimiEdits are the exact text changes that turn systemPrompt into Kimi K3's
// head (docs/KIMI-INTEGRATION.md §4.4): the inventory restated for Kimi's
// fourteen-tool array, the vision rule replaced by the one sentence that is
// true for K3 — it reads images natively, so Screenshot and ReviewScreenshot
// are not offered and Read returns the image for a PNG, JPEG or WebP path —
// and the two dropped tools unnamed everywhere else in the text.
//
// The head is derived rather than copied for the same reason variants are
// replacements rather than second copies (docs/EVALS.md): an edit to shared
// text reaches the Kimi head automatically, and an edit that breaks one of
// these anchors fails loudly at init instead of silently changing what Kimi
// sessions are told. The rendered head is still a second frozen head — the
// bytes are fixed for the life of the build and stored per session, exactly
// like DeepSeek's — it is just single-sourced in source.
var kimiEdits = []struct{ from, to string }{
	{
		from: "Tools: Read, Write, Edit, Bash, Glob, Grep, List, TaskCreate, TaskGet,\n" +
			"TaskList, TaskUpdate, Task, WebFetch, Screenshot, ReviewScreenshot, Complete.\n" +
			"All sixteen are always available;",
		to: "Tools: Read, Write, Edit, Bash, Glob, Grep, List, TaskCreate, TaskGet,\n" +
			"TaskList, TaskUpdate, Task, WebFetch, Complete.\n" +
			"All fourteen are always available;",
	},
	{
		from: "- Ad hoc files that are not part of the task's deliverable — a screenshot\n" +
			"  taken for ReviewScreenshot, a scratch note, a temporary download — belong\n" +
			"  in a scratch/ directory at the workspace root, sibling to the repository\n" +
			"  clone(s); never /tmp (shared with every other concurrent session in this\n" +
			"  container, and not preserved), and never inside a cloned repository (risks\n" +
			"  being swept into a commit). Screenshot writes there and nowhere else.",
		to: "- Ad hoc files that are not part of the task's deliverable — a screenshot,\n" +
			"  a scratch note, a temporary download — belong in a scratch/ directory at\n" +
			"  the workspace root, sibling to the repository clone(s); never /tmp (shared\n" +
			"  with every other concurrent session in this container, and not preserved),\n" +
			"  and never inside a cloned repository (risks being swept into a commit).\n" +
			"  Write screenshots there and nowhere else.",
	},
	{
		from: "- You cannot see images. When a change is visual, Screenshot the page and\n" +
			"  send the file to ReviewScreenshot with the spec you were working to —\n" +
			"  that pair is your only way to find out what you actually built, and\n" +
			"  guessing from the markup is how a broken layout gets reported as done.",
		to: "- You can see images: Read returns the image when the path is a PNG, JPEG, or\n" +
			"  WebP file.",
	},
}

// kimiSystemPrompt is Kimi K3's frozen head, derived once at init from
// systemPrompt by applying kimiEdits. A Kimi session renders this instead of
// systemPrompt; the two heads are pinned to their own tool arrays by
// TestPromptNamesExactlyTheToolArray, and DeepSeek's head is untouched byte
// for byte.
var kimiSystemPrompt = renderKimiSystemPrompt()

func renderKimiSystemPrompt() string {
	p := systemPrompt
	for _, e := range kimiEdits {
		if !strings.Contains(p, e.from) {
			panic("session: kimi prompt edit no longer matches the base prompt: " + e.from)
		}
		p = strings.Replace(p, e.from, e.to, 1)
	}
	return p
}

// RenderSystemPrompt returns the frozen DeepSeek system prompt text. Sessions
// store its output directly on creation and never call it again for the life
// of that session (docs/CACHE.md).
func RenderSystemPrompt() string {
	return systemPrompt
}

// RenderSystemPromptFor returns the frozen system prompt for the provider
// serving model, with a named variant's edits made. An empty variant name is
// the provider's shipped prompt, byte for byte (internal/promptvariant):
// DeepSeek renders systemPrompt unchanged, Kimi renders kimiSystemPrompt,
// whose inventory matches the fourteen-tool array Kimi sessions are sent
// (docs/KIMI-INTEGRATION.md §4.4).
func RenderSystemPromptFor(model, variant string) (string, error) {
	base := systemPrompt
	if seesImages(model) {
		base = kimiSystemPrompt
	}
	return promptvariant.Apply(variant, base)
}

// RenderOpeningMessage builds the first user message: everything specific
// to this run, which is why it lives here and not in the system prompt
// (docs/DESIGN.md §3.2). resultSchema, when non-empty, is shown here too —
// putting it in Complete's tool definition instead would vary the tool
// array per request and cost the shared prefix (docs/TOOLS.md).
//
// claudeMDBlock, when non-empty, is the rendered contents of the root
// CLAUDE.md files found in the workspace's repositories (internal/claudemd).
// skillCatalogue, when non-empty, lists the skills found there
// (internal/skills). Both sit ahead of the task so the task text stays last.
// Empty blocks leave the message byte-identical to what a run without them
// produces.
//
// attachments names the files the request's attachments were materialised
// into under scratch/attachments/ (internal/workspace). The model cannot
// guess they exist — nothing in the task text says so — so they are named
// here, ahead of the task, with the path a tool call can use; the common
// use is passing one to ReviewScreenshot as the mockup the page should be
// judged against.
func RenderOpeningMessage(workspace, task string, resultSchema json.RawMessage, claudeMDBlock, skillCatalogue string, attachments []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace: %s\n\n", workspace)
	if claudeMDBlock != "" {
		b.WriteString(claudeMDBlock)
		b.WriteString("\n")
	}
	if skillCatalogue != "" {
		b.WriteString(skillCatalogue)
		b.WriteString("\n")
	}
	if len(attachments) > 0 {
		b.WriteString("Image files attached to this task, materialised into scratch/attachments/:\n")
		for _, name := range attachments {
			fmt.Fprintf(&b, "- scratch/attachments/%s\n", name)
		}
		b.WriteString("You can pass one of these paths to ReviewScreenshot — the image is already in the workspace, so the spec you were working to is something you can show rather than describe.\n\n")
	}
	fmt.Fprintf(&b, "Task:\n%s\n", task)
	if len(resultSchema) > 0 {
		b.WriteString("\nWhen you call Complete, its result argument must validate against this JSON Schema:\n")
		b.WriteString(string(resultSchema))
		b.WriteString("\nThe schema describes the value of result, not Complete's own arguments. " +
			"Complete takes exactly three: summary, result, status. Every field named above " +
			"goes inside result:\n")
		b.WriteString(completeExample(resultSchema))
		b.WriteString("\n")
	}
	return b.String()
}

// completeExample renders the call shape that goes with resultSchema, using
// that schema's own field names so the example is about this run's payload
// rather than a generic one. A live run put the schema's fields at the top
// level beside status, read the resulting "result: expected object, got
// null" as a harness fault, and burned eleven sub-turns on it
// (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md); the schema alone
// evidently does not convey the nesting. A schema with no usable properties
// falls back to an elided example, which still shows the nesting.
func completeExample(resultSchema json.RawMessage) string {
	var s struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(resultSchema, &s); err != nil {
		return `Complete(summary="…", status="done", result={…})`
	}
	// Required fields first and in the schema's own order, then whatever
	// else is defined, sorted so the example is stable across runs.
	var fields []string
	seen := map[string]bool{}
	for _, r := range s.Required {
		if _, ok := s.Properties[r]; ok && !seen[r] {
			fields = append(fields, r)
			seen[r] = true
		}
	}
	var rest []string
	for k := range s.Properties {
		if !seen[k] {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	fields = append(fields, rest...)
	if len(fields) == 0 {
		return `Complete(summary="…", status="done", result={…})`
	}
	inner := make([]string, len(fields))
	for i, f := range fields {
		inner[i] = fmt.Sprintf("%q: …", f)
	}
	return fmt.Sprintf(`Complete(summary="…", status="done", result={%s})`, strings.Join(inner, ", "))
}

// RenderCompactionSummarySystemPromptFor seeds a session forked by
// compaction for the provider serving model: the provider's own system
// prompt plus a summary of the parent session, placed in the stable head
// where it can itself become a cache checkpoint rather than just more body
// text in a user message (docs/CACHE.md).
func RenderCompactionSummarySystemPromptFor(model, summary string) string {
	base := systemPrompt
	if seesImages(model) {
		base = kimiSystemPrompt
	}
	return base + "\n\n## Continuing from a prior session\n\n" +
		"That session ran long enough to need compaction. Here is a summary of what happened before this point:\n\n" + summary
}

// RenderCompactionOpeningMessage is the opening user message of a session
// forked by compaction. Compaction is a session boundary, not an edit
// (docs/TOOLS.md), so this is a fresh conversation whose only link to the
// parent is the summary already placed in the system prompt.
func RenderCompactionOpeningMessage(workspace string) string {
	return fmt.Sprintf("Workspace: %s\n\nContinue the task described in the system prompt's summary.", workspace)
}
