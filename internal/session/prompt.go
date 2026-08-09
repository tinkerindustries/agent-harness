package session

import (
	"encoding/json"
	"fmt"
	"strings"
)

// systemPrompt is fixed for the life of a harness build. Nothing here may
// vary per session or per request — no clock, cwd, git status, or file
// listing — because it sits at the head of every request and divergence
// there costs the whole prompt cache (docs/CACHE.md). Per-session and
// per-request context goes in the opening user message instead.
const systemPrompt = `You are a headless coding agent. You work inside one workspace directory for
the whole session and finish tasks by editing files and running commands,
not by describing what someone else should do.

Tools: Read, Write, Edit, Bash, Glob, Grep, List, TodoWrite, Task, WebFetch,
Complete. All eleven are always available; a permission policy may refuse a
particular call at execution time. A refusal comes back as a tool result
naming the rule that blocked it — read it and route around the restriction
rather than repeating the same call.

Rules:
- Read a file before Write-ing over it or Edit-ing it. Edit requires an
  exact, unique match of old_string against the file's real bytes; the
  line-number prefix Read shows you is for your reference only and must
  never appear inside old_string.
- Prefer Grep and Glob to orient before reading whole files.
- Use TodoWrite to track multi-step work. Keep it current: mark a step
  in_progress before starting it and completed right after, so anyone
  watching the plan can see real progress.
- Delegate self-contained side work to Task when it would otherwise clutter
  this conversation, and use WebFetch to read documentation or a URL you
  were given.
- Tool calls cannot be forced. When the task is done, call Complete
  yourself with a summary and, if asked for one, a structured result. If
  you stop without finishing, call Complete with status "gave_up" and say
  why in summary.
- Work only within the workspace path given in the opening message. Paths
  outside it are rejected.`

// RenderSystemPrompt returns the frozen system prompt text. Sessions store
// its output directly on creation and never call it again for the life of
// that session (docs/CACHE.md).
func RenderSystemPrompt() string {
	return systemPrompt
}

// RenderOpeningMessage builds the first user message: everything specific
// to this run, which is why it lives here and not in the system prompt
// (docs/DESIGN.md §3.2). resultSchema, when non-empty, is shown here too —
// putting it in Complete's tool definition instead would vary the tool
// array per request and cost the shared prefix (docs/TOOLS.md).
//
// skillCatalogue, when non-empty, lists the skills found in the workspace's
// repositories (internal/skills). It sits ahead of the task so the task text
// stays last. An empty catalogue leaves the message byte-identical to what a
// run without skills produces.
func RenderOpeningMessage(workspace, task string, resultSchema json.RawMessage, skillCatalogue string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace: %s\n\n", workspace)
	if skillCatalogue != "" {
		b.WriteString(skillCatalogue)
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "Task:\n%s\n", task)
	if len(resultSchema) > 0 {
		b.WriteString("\nWhen you call Complete, its result argument must validate against this JSON Schema:\n")
		b.WriteString(string(resultSchema))
		b.WriteString("\n")
	}
	return b.String()
}

// RenderCompactionSummarySystemPrompt seeds a session forked by compaction:
// the base system prompt plus a summary of the parent session, placed in
// the stable head where it can itself become a cache checkpoint rather than
// just more body text in a user message (docs/CACHE.md).
func RenderCompactionSummarySystemPrompt(summary string) string {
	return systemPrompt + "\n\n## Continuing from a prior session\n\n" +
		"That session ran long enough to need compaction. Here is a summary of what happened before this point:\n\n" + summary
}

// RenderCompactionOpeningMessage is the opening user message of a session
// forked by compaction. Compaction is a session boundary, not an edit
// (docs/TOOLS.md), so this is a fresh conversation whose only link to the
// parent is the summary already placed in the system prompt.
func RenderCompactionOpeningMessage(workspace string) string {
	return fmt.Sprintf("Workspace: %s\n\nContinue the task described in the system prompt's summary.", workspace)
}
