package session

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mrgeoffrich/agent-harness/internal/promptvariant"
	"github.com/mrgeoffrich/agent-harness/internal/tools"
	"github.com/mrgeoffrich/agent-harness/internal/wire"
)

// The head of every request is fixed for the life of a harness build.
// Nothing here may vary per session or per request — no clock, cwd, git
// status, or file listing — because it sits at the head of every request
// and divergence there costs the whole prompt cache (docs/CACHE.md).
// Per-session and per-request context goes in the opening user message
// instead.
//
// The tool section of the head is assembled, not stored: the inventory
// sentence and its count word are generated from the session's tool array,
// and the rules are a table of fragments each declaring the tools it needs
// (internal/tools). A session's head is therefore a pure function of its
// tool array and one provider capability — seesImages (docs/KIMI-INTEGRATION
// .md §4.5) — and adding a tool to an array updates the inventory, the
// count, and every rule about that tool in one place. The rendered bytes
// are pinned by TestPromptGolden against the committed golden files, the
// same byte-stability guard the tool arrays have
// (internal/tools/definitions_golden_test.go): the head is the shared
// prompt-cache prefix, so a byte that moves costs every session on that
// provider a full cache miss.
const promptPreamble = `You are a headless coding agent. You work inside one workspace directory for
the whole session and finish tasks by editing files and running commands,
not by describing what someone else should do.

`

// toolOrder is the order the frozen head lists tools in its inventory. It is
// the order the tools were added to the prompt, and it is NOT the tool
// array's own order (internal/tools/definitions.go); toolNamesInOrder
// reconciles the two by listing the array's membership in this order. Both
// orders are pinned — the array's by its own golden files, this one by
// TestPromptGolden and by TestPromptNamesExactlyTheToolArray.
//
// The vision block is `screenshot` then the five tools that read what it
// captures, which is the order a session uses them in: capture, then look
// (`glance`, or `transcribe` when the looking is at more text than one call can
// resolve), then locate (`ground`, `detect`), then cut a located box out (`crop`).
// Replacing ReviewScreenshot and AskVision with these four changed the head's
// bytes and therefore the cache prefix once, for every session
// (docs/CACHE.md, docs/VISION-TOOLKIT.md) — a cost paid deliberately at the
// swap rather than drifted into.
var toolOrder = []string{
	"read", "write", "edit", "apply_patch", "bash", "job_output", "job_kill", "job_list", "glob", "grep", "list",
	"task_create", "task_get", "task_list", "task_update", "task", "web_fetch",
	"screenshot", "glance", "transcribe", "ground", "detect", "crop", "complete",
}

// toolNamesInOrder maps a session's tool array onto the head's canonical
// inventory order: the array's membership, listed in toolOrder. A session
// whose array is the DeepSeek or Kimi one renders the frozen head byte for
// byte (TestPromptGolden); a variant that subtracts tools (internal/prompt
// variant) renders the same head with those tools' names and rules gone.
func toolNamesInOrder(array []wire.Tool) []string {
	have := make(map[string]bool, len(array))
	for _, t := range array {
		have[t.Function.Name] = true
	}
	names := make([]string, 0, len(array))
	for _, name := range toolOrder {
		if have[name] {
			names = append(names, name)
		}
	}
	return names
}

// inventorySentence renders the "Tools: <names>." paragraph of the head
// from the session's tool names. The wrapping is the frozen head's: each
// line holds at most 78 characters of text and a line break falls only
// between names. The count sentence that follows ("All <n> are always
// available; ...") is fixed text with the count word generated from the
// array (numberWord), so the whole inventory stays truthful for any tool
// set without anyone editing a sentence by hand.
func inventorySentence(names []string) string {
	const width = 79 // counts the ", " separator that follows each name but the last
	var b strings.Builder
	line := "Tools: "
	for i, name := range names {
		piece := name
		if i < len(names)-1 {
			piece += ", "
		} else {
			piece += "."
		}
		if len(line)+len(piece) > width && line != "Tools: " {
			b.WriteString(strings.TrimSpace(line))
			b.WriteString("\n")
			line = ""
		}
		line += piece
	}
	b.WriteString(line)
	b.WriteString("\n")
	return b.String()
}

// numberWord spells n the way the head spells its availability count
// ("All <word> are always available"). The count is generated from the
// tool array, so a count past the end of the table renders "?" rather than a
// word — visible in the golden head the moment it happens, which is what
// forces the table to grow.
func numberWord(n int) string {
	words := [...]string{"", "one", "two", "three", "four", "five", "six", "seven", "eight",
		"nine", "ten", "eleven", "twelve", "thirteen", "fourteen", "fifteen",
		"sixteen", "seventeen", "eighteen", "nineteen", "twenty",
		"twenty-one", "twenty-two", "twenty-three", "twenty-four", "twenty-five"}
	if n < 1 || n >= len(words) {
		return "?"
	}
	return words[n]
}

// availabilityTail is the fixed part of the availability paragraph, after
// the count word generated from the array ("All " + count + tail).
const availabilityTail = ` are always available; a permission policy may refuse a particular call at
execution time. A refusal comes back as a tool result naming the rule that
blocked it — read it and route around the restriction rather than repeating
the same call.

`

// toolFragment is one entry of the head's Rules list. It renders when every
// tool in needs is in the session's tool array, no tool in unless is, and
// (when set) the provider's seesImages capability matches. needs, unless
// and seesImages are all optional; a fragment with none of them set always
// renders. The table order is the frozen head's rule order — the bytes the
// goldens pin — so the assembled head is deterministic for a given tool
// set, and a session's head is a pure function of its array and capability
// rather than of the order fragments were edited in.
type toolFragment struct {
	// needs lists the tools that must all be in the session's tool array
	// for the fragment to render. Empty means the fragment does not depend
	// on the tool set.
	needs []string
	// unless lists tools whose presence suppresses the fragment: every one
	// of them must be absent for it to render. The scratch rule is worded
	// differently for a session that has the screenshot tool and one that
	// does not — it must not name a tool the session was never sent — and
	// unless is the other half of that pair.
	unless []string
	// seesImages, when set, additionally requires the provider capability
	// to match: true renders only for a provider that reads images
	// natively, false only for one that does not. The vision sentence is
	// capability-shaped, not tool-shaped — "You can see images" is true of
	// a model that reads images natively because of the provider
	// (seesImages in runner.go), and no tool array says so — so it is
	// selected here rather than by which tools happen to be present.
	seesImages *bool
	// text is the fragment's contribution, byte for byte the frozen head's.
	text string
}

// renders reports whether the fragment belongs in a head assembled for this
// tool set and provider capability.
func (f toolFragment) renders(have map[string]bool, seesImages bool) bool {
	for _, name := range f.needs {
		if !have[name] {
			return false
		}
	}
	for _, name := range f.unless {
		if have[name] {
			return false
		}
	}
	if f.seesImages != nil && *f.seesImages != seesImages {
		return false
	}
	return true
}

// capability returns a pointer to b for a fragment's seesImages condition.
func capability(b bool) *bool { return &b }

// The Rules list, in the frozen head's order. The tool-shaped entries each
// name the tools they are about and render only when all of them are in the
// session's array. The plan rules are one group — the plan machinery is
// all-or-nothing, so any one of the four plan tools missing removes the
// whole section — split into four fragments so the one bullet that mentions
// bash ("a long run of bash or edit calls") can pick its wording from what
// is present, the same way the scratch rule does: a session without bash
// gets the same bullet without the bash mention, and one with it gets the
// full text. The workspace rule is not about any tool and always renders.
// The scratch rule always renders too, but its wording depends on whether
// the session has the screenshot tool: the head must not name a tool the
// session was never sent, so a session without screenshot gets the wording
// that does not mention it. The vision rule renders as "You cannot see
// images ..." when the session is offered the capture-and-look tools, and
// as the one true sentence for a provider that reads images natively — a
// property of the provider, not of the tool array.
var toolFragments = []toolFragment{
	{
		needs: []string{"read", "write", "edit"},
		text: `- Call read on a file before overwriting it with write or changing it with
  edit. edit requires an exact, unique match of old_string against the file's
  real bytes; the line-number prefix read shows you is for your reference only
  and must never appear inside old_string.
`,
	},
	{
		needs: []string{"apply_patch"},
		text: `- apply_patch carries a whole patch in input, from *** Begin Patch to
  *** End Patch, and applies every hunk or none: one hunk that does not match
  refuses the call and leaves every file unchanged. Every file it changes must
  have been read this session first.
`,
	},
	{
		needs: []string{"read", "grep", "glob", "bash"},
		text: `- Send independent tool calls together in one message. Several read calls, a
  grep beside a glob, bash commands that do not depend on each other —
  batched, they run concurrently and cost one round trip instead of five. Two
  calls that write the same file are the one exception: they are applied in
  the order you sent them, never merged, so the second is working from bytes
  the first has already replaced. Make the change in a single edit where you
  can, and where you cannot, send the second only after seeing the first land.
`,
	},
	{
		needs: []string{"grep", "glob"},
		text:  "- Prefer grep and glob to orient before reading whole files.\n",
	},
	{
		needs: []string{"bash"},
		text: `- The shell is bash on whatever machine you were started on. GNU grep, rg,
  curl, ps and the git, Go, Node and Python toolchains are usually present.
  Check that a tool is there before you lean on it.
`,
	},
	{
		needs: []string{"bash", "job_output", "job_kill", "job_list"},
		text: `- A dev server, a watcher, or anything else meant to keep running rather
  than finish belongs in bash with run_in_background set, not backgrounded
  with & inside an ordinary call. Read what it has produced with job_output,
  which returns only what is new since the last time you read it, and stop
  it with job_kill when you are done with it rather than leaving it running.
  job_list names every job the session has started, with its command and
  whether it is still running. When a background job exits it starts a new
  turn telling you, so end your turn to wait for a build or a gate instead of
  polling it.
`,
	},
	{
		needs: []string{"task_create", "task_get", "task_list", "task_update"},
		text: `- A task that takes three or more steps gets a plan. Call task_create once, at
  the start, with one entry per step. Every entry needs all three of: subject,
  a short title like "Run the test suite"; description, what the step
  involves; activeForm, the subject in the present continuous, like "Running
  the test suite", which a human watching the run sees while that step is in
  progress. A one- or two-step task needs no plan — do the work.
`,
	},
	{
		needs: []string{"task_create", "task_get", "task_list", "task_update", "bash"},
		text: `- Set a task to in_progress with task_update before starting it, and to
  completed with task_update as soon as it is done. Keep exactly one task
  in_progress. Send each update at the moment the step changes state, even
  mid-stream through a long run of bash or edit calls, rather than saving the
  updates for the end of the run.
`,
	},
	{
		needs:  []string{"task_create", "task_get", "task_list", "task_update"},
		unless: []string{"bash"},
		text: `- Set a task to in_progress with task_update before starting it, and to
  completed with task_update as soon as it is done. Keep exactly one task
  in_progress. Send each update at the moment the step changes state, even
  mid-stream through a long run of edit calls, rather than saving the
  updates for the end of the run.
`,
	},
	{
		needs: []string{"task_create", "task_get", "task_list", "task_update"},
		text: `- One task_update call names one taskId and sets the one or two fields that
  changed. Do not re-send the whole plan.
- Mark a task completed only when it actually worked. If a step's command
  failed or its fix did not hold, leave that task in_progress and call
  task_create for what is still outstanding. A plan of completed tasks that
  did not work produces a false summary at the end of the run.
- Call task_list to re-read the plan when you have lost track of it, and
  task_get with a taskId to re-read one task's description. Read the plan back
  after a long stretch of work rather than guessing what is left.
`,
	},
	{
		needs: []string{"task", "web_fetch"},
		text: `- Delegate self-contained side work to task when it would otherwise clutter
  this conversation, and use web_fetch to read documentation or a URL you
  were given.
`,
	},
	{
		needs: []string{"complete"},
		text: `- Tool calls cannot be forced. When the task is done, call complete
  yourself with a summary and, if asked for one, a structured result. If
  you stop without finishing, call complete with status "gave_up" and say
  why in summary.
`,
	},
	{
		text: `- Work only within the workspace path given in the opening message. Paths
  outside it are rejected.
`,
	},
	{
		needs: []string{"screenshot"},
		text: `- Ad hoc files that are not part of the run's deliverable — a screen capture, a
  scratch note, a temporary download — belong in a scratch/ directory at the
  workspace root, sibling to the repository clone(s); never /tmp (shared with
  everything else on this machine, and not preserved), and never inside a
  cloned repository (risks being swept into a commit). screenshot writes there
  and nowhere else.
`,
	},
	{
		unless: []string{"screenshot"},
		text: `- Ad hoc files that are not part of the run's deliverable — a screen capture,
  a scratch note, a temporary download — belong in a scratch/ directory at
  the workspace root, sibling to the repository clone(s); never /tmp (shared
  with everything else on this machine, and not preserved), and never inside
  a cloned repository (risks being swept into a commit). Write screenshots
  there and nowhere else.
`,
	},
	{
		needs: []string{"screenshot", "glance"},
		text: `- You cannot see images. When a change is visual, screenshot the page and
  ask glance what it shows, naming the spec you were working to — that pair
  is your only way to find out what you actually built, and guessing from the
  markup is how a broken layout gets reported as done.
- glance answers what something is; ground and detect answer where it is, as
  pixel boxes. Ask for a box whenever the answer turns on a position, a size,
  an alignment or a count: a description of a gap reads the same whether it
  is 4px or 40px. Boxes are close rather than exact — crop and compare with
  them, and read a value you have to be sure of out of the CSS.
- Compare images in one glance call, never two. Separate calls cannot see
  each other's image, so comparing their answers compares two guesses.
- An image costs a fixed token budget however large it is, so a full-page
  capture spends it on the parts you did not ask about. When the question is
  about one control, capture that control — or pass a box from ground to
  crop, or to glance's region, and spend the budget there.
`,
	},
	{
		needs: []string{"transcribe"},
		text: `- To read the text off a page taller than a screen, use transcribe rather
  than glance: it cuts the image up and reads each piece at full resolution,
  where one look at the whole thing spends the same fixed budget and comes
  back with the top of the page and a summary of the rest. Read the seam
  report it returns before you trust the text at a boundary.
`,
	},
	{
		seesImages: capability(true),
		text: `- You can see images: read returns the image when the path is a PNG, JPEG, or
  WebP file.
`,
	},
}

// renderSystemPromptFor assembles the head for one session's tool set and
// provider capability: the fixed preamble, the inventory generated from the
// array, and the Rules list assembled from the fragment table. The bytes
// are deterministic — a pure function of names and seesImages — so every
// session with the same tool set shares the same head, the prompt cache's
// prefix (docs/CACHE.md).
func renderSystemPromptFor(names []string, seesImages bool) string {
	have := make(map[string]bool, len(names))
	for _, name := range names {
		have[name] = true
	}
	var b strings.Builder
	b.WriteString(promptPreamble)
	b.WriteString(inventorySentence(names))
	b.WriteString("All ")
	b.WriteString(numberWord(len(names)))
	b.WriteString(availabilityTail)
	b.WriteString("Rules:\n")
	for _, f := range toolFragments {
		if f.renders(have, seesImages) {
			b.WriteString(f.text)
		}
	}
	// Every fragment ends with a newline so that adding one to the end of
	// the table cannot run into the previous rule's last line — which is
	// what happened when the fragments that could be last omitted theirs.
	// The head itself ends without one, so the last fragment's is trimmed
	// here rather than left off there.
	return strings.TrimRight(b.String(), "\n")
}

// RenderSystemPrompt returns the frozen prompt for the base tool array. It
// is what a caller with no provider in hand renders — the tests that pin the
// head's bytes, and any direct CLI caller — and it is not what any provider's
// session is sent, which goes through RenderSystemPromptFor.
// Sessions store their render directly on creation and never call it again
// for the life of that session (docs/CACHE.md).
func RenderSystemPrompt() string {
	return renderSystemPromptFor(toolNamesInOrder(tools.Definitions()), false)
}

// RenderSystemPromptFor returns the frozen system prompt for the provider
// serving model, with a named variant's edits made. An empty variant name is
// the provider's shipped prompt, byte for byte (internal/promptvariant):
// DeepSeek renders the head for its hosted array, Kimi renders the head for
// its own, whose inventory, count word and rules all follow from the array
// itself (internal/tools/definitions.go, docs/KIMI-INTEGRATION.md §4.4),
// Gemini renders the same head Kimi does, since both resolve to
// tools.definitionsVisionCapable and both have seesImages() true
// (docs/GEMINI-INTEGRATION.md §5.7), and a variant that subtracts tools
// (tools.DefinitionsForVariant) renders the head for its own smaller array,
// with no replacements entry needed to keep the inventory truthful.
func RenderSystemPromptFor(model, variant string) (string, error) {
	base := renderSystemPromptFor(toolNamesInOrder(tools.DefinitionsForVariant(model, variant)), seesImages(model))
	return promptvariant.Apply(variant, base)
}

// RenderMCPBlock builds the opening message's MCP section from array — the
// session's resolved tool array, which carries a configured MCP server's
// tools appended after the built-in ones when any are configured
// (tools.WithMCP). Empty when array carries none, so a session with no MCP
// servers configured renders the same opening message it always has
// (docs/MCP.md, "What the model is told": nothing here touches the system
// prompt, and the opening message stays byte-identical for a run with no
// MCP tools). The naming stays a naming — which servers, and how many tools
// each contributes — rather than a catalogue of every tool: grouped by
// server with tools.MCPServerOf, server names sorted so the rendering is
// deterministic regardless of the array's own tool order.
//
// instructions is what each server said about itself at initialize, keyed
// by server name (store.MCPServer.Instructions). A server that sent any has
// its text quoted verbatim under its own heading, in the same sorted order,
// and only when that server actually contributed tools to array — prose
// about tools the session cannot call is prose the model has no use for.
// A nil or empty map renders exactly the block this function produced
// before instructions were plumbed through at all.
//
// Verbatim is the whole point: this text is written by the server's authors
// for a model to read, and it is where the operational knowledge lives that
// a tool schema has no room for — the official Blender server's explains
// the datablock model, that operators clobber the selection, and that an
// unflushed bmesh silently loses every edit. Summarising it here would be
// this harness second-guessing the one party that knows.
func RenderMCPBlock(array []wire.Tool, instructions map[string]string) string {
	counts := map[string]int{}
	var servers []string
	for _, t := range array {
		server, ok := tools.MCPServerOf(t.Function.Name)
		if !ok {
			continue
		}
		if counts[server] == 0 {
			servers = append(servers, server)
		}
		counts[server]++
	}
	if len(servers) == 0 {
		return ""
	}
	sort.Strings(servers)

	var b strings.Builder
	b.WriteString("MCP servers: this session also has tools from configured MCP servers, named\n")
	b.WriteString("mcp__<server>__<tool>, which reach systems outside the workspace.\n")
	for _, server := range servers {
		n := counts[server]
		suffix := "s"
		if n == 1 {
			suffix = ""
		}
		fmt.Fprintf(&b, "- %s (%d tool%s)\n", server, n, suffix)
	}
	for _, server := range servers {
		text := strings.TrimSpace(instructions[server])
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "\nWhat the %s server says about using it:\n\n%s\n", server, text)
	}
	return strings.TrimRight(b.String(), "\n")
}

// RenderOpeningMessage builds the first user message: everything specific
// to this run, which is why it lives here and not in the system prompt
// (docs/DESIGN.md §3.2). resultSchema, when non-empty, is shown here too —
// putting it in `complete`'s tool definition instead would vary the tool
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
// into under scratch/attachments/. The model cannot
// guess they exist — nothing in the task text says so — so they are named
// here, ahead of the task, with the path a tool call can use; the common
// use is passing one to `glance` as the mockup the page should be judged
// against.
//
// mcpBlock, when non-empty, is RenderMCPBlock's output — naming the
// session's configured MCP servers, beside the CLAUDE.md excerpts and the
// skills catalogue rather than in the system prompt (docs/MCP.md, "What
// the model is told": the head stays byte-identical to a session with no
// MCP tools). Empty leaves the message byte-identical to a run before this
// parameter existed, the same contract claudeMDBlock and skillCatalogue
// already keep.
func RenderOpeningMessage(workspace, task string, resultSchema json.RawMessage, claudeMDBlock, skillCatalogue, mcpBlock string, attachments []string) string {
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
	if mcpBlock != "" {
		b.WriteString(mcpBlock)
		b.WriteString("\n")
	}
	b.WriteString(RenderAttachmentBlock("task", attachments))
	fmt.Fprintf(&b, "Task:\n%s\n", task)
	if len(resultSchema) > 0 {
		b.WriteString("\nWhen you call complete, its result argument must validate against this JSON Schema:\n")
		b.WriteString(string(resultSchema))
		b.WriteString("\nThe schema describes the value of result, not complete's own arguments. " +
			"complete takes exactly three: summary, result, status. Every field named above " +
			"goes inside result:\n")
		b.WriteString(completeExample(resultSchema))
		b.WriteString("\n")
	}
	return b.String()
}

// RenderAttachmentBlock names the image files an attachment-carrying message
// was materialised into under scratch/attachments/. The
// model cannot guess they exist — nothing in the message text says so — so
// they are named ahead of the words, with the path a tool call can use; the
// common use is passing one to `glance` as the mockup the page should be
// judged against.
//
// subject is the noun the block calls the thing the images came with: "task"
// for the opening message, "message" for a steer or a continuation somebody
// typed into the composer. Three callers share it so the model reads the
// same sentence wherever an image arrives from, and so the phrasing the
// opening message has always used stays exactly one string in the tree.
//
// No attachments renders the empty string, which leaves every caller's
// message byte-identical to what it produced before this block existed.
func RenderAttachmentBlock(subject string, attachments []string) string {
	if len(attachments) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Image files attached to this %s, materialised into scratch/attachments/:\n", subject)
	for _, name := range attachments {
		fmt.Fprintf(&b, "- scratch/attachments/%s\n", name)
	}
	b.WriteString("You can pass one of these paths to glance — the image is already in the workspace, so the spec you were working to is something you can show rather than describe.\n\n")
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
		return `complete(summary="…", status="done", result={…})`
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
		return `complete(summary="…", status="done", result={…})`
	}
	inner := make([]string, len(fields))
	for i, f := range fields {
		inner[i] = fmt.Sprintf("%q: …", f)
	}
	return fmt.Sprintf(`complete(summary="…", status="done", result={%s})`, strings.Join(inner, ", "))
}

// RenderCompactionSummarySystemPromptFor seeds a session forked by
// compaction for the provider serving model: the system prompt of the
// session being continued — variant-aware, so a compacted variant session's
// head still names the array it is actually sent — plus a summary of the
// parent session, placed in the stable head where it can itself become a
// cache checkpoint rather than just more body text in a user message
// (docs/CACHE.md).
func RenderCompactionSummarySystemPromptFor(model, variant, summary string) string {
	base := renderSystemPromptFor(toolNamesInOrder(tools.DefinitionsForVariant(model, variant)), seesImages(model))
	return base + compactionSummaryIntro + summary
}

// compactionSummaryIntro is the text between a compacted session's base
// system prompt and its summary. priorCheckpoint finds the summary by it.
const compactionSummaryIntro = "\n\n## Continuing from a prior session\n\n" +
	"That session ran long enough to need compaction. Here is a summary of what happened before this point:\n\n"

// priorCheckpoint returns the summary a compacted session was seeded with,
// or "" for a session that was not forked by compaction.
func priorCheckpoint(systemPrompt string) string {
	_, summary, ok := strings.Cut(systemPrompt, compactionSummaryIntro)
	if !ok {
		return ""
	}
	return summary
}

// RenderCompactionOpeningMessage is the opening user message of a session
// forked by compaction. Compaction is a session boundary, not an edit
// (docs/TOOLS.md), so this is a fresh conversation whose only link to the
// parent is the summary already placed in the system prompt.
func RenderCompactionOpeningMessage(workspace string) string {
	return fmt.Sprintf("Workspace: %s\n\nContinue the task described in the system prompt's summary.", workspace)
}
