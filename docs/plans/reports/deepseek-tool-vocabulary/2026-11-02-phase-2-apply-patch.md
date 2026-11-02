# Phase 2: `apply_patch`

## What was built

`apply_patch` is a new tool in `internal/tools/patch.go`. It takes one string
argument, `input`, and reads the Codex patch grammar dsh uses: `*** Begin
Patch` to `*** End Patch`, `*** Add File:` with `+` lines, `*** Update File:`
with an optional `*** Move to:`, `*** Delete File:`, hunks introduced by `@@`
(the text after it anchors the hunk after the first matching line) and
carrying space, `+` and `-` lines, and `*** End of File` to pin the last hunk
to the end of the file. The tool description carries that grammar to the
model.

Every operation is staged against an in-memory copy of the file it names
before a byte is written. A hunk that does not match fails the whole call and
leaves the workspace exactly as it was, so a multi-file patch is all or
nothing. Writes are committed before deletes, so a write that fails leaves the
old files in place. A move carries the source's file mode to the destination,
because `os.WriteFile` only applies its mode when it creates a file.

Paths resolve through `ResolvePath` against the workspace root, the same check
`write` and `edit` go through. One path that escapes fails the whole patch.
The tool is mutating and is not in `alwaysAllowed`, so a read-only session is
refused it and a full one is allowed it. `descriptorFor` needed no case: a
patch can span files, so the string a deny pattern sees is the bare name, and
`-deny apply_patch` keeps a session off the tool entirely.

The prior-read decision: a file the patch updates or deletes must have been
read in this session, the same gate `write` and `edit` carry. The patch's own
context lines are not a substitute. A delete names no content at all, and an
insertion-only hunk carries none. A file the patch creates needs no read, and
so does a file an earlier operation in the same patch created. Two table cases
cover the gate, one for an unread update and one for an unread delete.

The scheduling decision: `MutationTarget` became `MutationTargets` and returns
a slice. `edit` and `write` return one path each. `apply_patch` returns every
path its patch names, a move's destination included. `executeToolCalls`
groups calls with a small union of path groups: a call joins every group its
files belong to, and two groups it bridges become one whose calls are sorted
back into the model's order. A patch spanning two files therefore runs as one
ordered group with an edit of either file, and two patches naming one file
never race. The tool result carries no `Result.Diff`. That field is Edit's
one-file line diff, and a patch has no one file to attach it to. The summary
names each file with the operation that happened to it.

`internal/promptvariant` gained `no-patch`, which drops `apply_patch` and
needs no replacement entry. The head's apply_patch rule is its own fragment
gated on the tool, so it leaves with the array. `internal/session/prompt.go`
lists `apply_patch` in `toolOrder` after `edit` and adds that fragment. The
editing rule about `read`, `write` and `edit` is untouched, so a `no-patch`
head still carries it.

The tool joined `definitionsDeepSeek` after `edit`, so it reaches the base
array, the vision-capable array and Claude's. DeepSeek's hosted array carries
it after the seven-name subtraction. The four tool goldens and the three
prompt goldens were regenerated from `tools.DefinitionsFor` and
`session.RenderSystemPromptFor` with a throwaway program under `scratch/`,
deleted before the commit. The array sizes the tests and comments restate
moved with it: base 24, vision-capable 18, Claude 17, DeepSeek hosted 11.

## What was verified, and how

The phase's proof:

```
$ go test ./internal/tools -run ApplyPatch -count=1
ok  	github.com/mrgeoffrich/agent-harness/internal/tools	0.340s
```

`TestApplyPatchAppliesTheGrammar` is the table the plan asked for: add (and an
add refused because the file exists), update with a bare `@@` and context
lines, update anchored by `@@` text, two hunks on one file, a file with no
trailing newline, a CRLF patch, a bare empty line read as an empty context
line, `*** End of File` both matching and refusing, delete (and a delete of a
missing file), move alone and move with a hunk (and a destination that
exists), a multi-file patch, a hunk that misses, a missed hunk in a later file
leaving the earlier one untouched, a path that escapes the workspace, a
malformed header, a missing `*** End Patch`, a missing `*** Begin Patch`, an
update with neither a hunk nor a move, an unread update and an unread delete.
`TestApplyPatchReportsEveryFileItChanged` pins the summary.
`TestApplyPatchIsInEveryProviderArray` pins membership for all five routed
models. `TestApplyPatchIsMutatingAndDeniedInReadOnlyMode` pins the permission
shape. `TestMutationTargetsReportsEveryFileAPatchRewrites` pins the scheduling
contract.

The variant and head are covered by `TestToolsDroppedBy`'s `no-patch` case and
`TestNoPatchVariantRendersAHeadWithoutThePatchRule`, which checks the
inventory, the count word, the missing rule and the surviving `write`/`edit`
rule.

Two session tests pin the ordering: `TestTwoPatchesOnOneFileRunInOrder` and
`TestAPatchIsOrderedAgainstEveryFileItNames`. They passed under `-race`.

`go test ./... -count=1` passed every package.

The build gate:

```
$ scripts/build.sh

=== gofmt
clean

=== go vet
clean

=== tests
?   	github.com/mrgeoffrich/agent-harness/cmd/drive	[no test files]
ok  	github.com/mrgeoffrich/agent-harness/cmd/harness	0.762s
...
ok  	github.com/mrgeoffrich/agent-harness/internal/session	12.169s
ok  	github.com/mrgeoffrich/agent-harness/internal/settings	2.174s
ok  	github.com/mrgeoffrich/agent-harness/internal/skills	2.293s
ok  	github.com/mrgeoffrich/agent-harness/internal/stdiosession	5.366s
ok  	github.com/mrgeoffrich/agent-harness/internal/store	2.813s
ok  	github.com/mrgeoffrich/agent-harness/internal/tools	12.187s
ok  	github.com/mrgeoffrich/agent-harness/internal/wire	2.684s

=== cross-compile
darwin/arm64
darwin/amd64
linux/amd64
linux/arm64
android/arm64
windows/amd64

=== go build
bin/harness
```

exit code 0.

## What was left undone

Hunk matching is exact. Codex's matcher has a fuzzy pass that tolerates
trailing whitespace and normalised punctuation. This one compares line text
byte for byte and fails with the hunk's old lines and a nearest-candidate line
number. The failure is predictable, and the model can retry with `edit` or a
corrected patch. A patch copied from a context that dropped trailing
whitespace is refused.

A unified-diff hunk header (`@@ -1,3 +1,3 @@`) is read as `@@` context text
and fails to anchor. The grammar the description gives uses `@@ <context>`,
which is what Codex and dsh write. Accepting git's header form as "no anchor"
is a later kindness if live models reach for it.

`apply_patch` runs under the ordinary 30-second tool timeout. It makes no
model call and does only local I/O, so it has no budget of its own and no
`tools.*` setting.

A binary file cannot be deleted through `apply_patch`. The read gate cannot be
satisfied because `read` refuses binary files. `bash rm` remains the way to
remove one.

An I/O failure part-way through the commit loop can leave earlier files
written. Every parse and every hunk apply happens before the first write, so
the all-or-nothing property the plan asks for holds for a hunk that does not
match. A full transactional commit across files is a different problem and was
not attempted.

`docs/TOOLS.md` and the other documents still describe the old catalogue and
the old array sizes. Phase 4 owns them. The deny-pattern section is still to
say that a stored `-deny apply_patch` is the whole of what can target the tool,
since its descriptor carries no path.

## Bugs found and not fixed

Nothing outstanding. Two hazards were designed around. A move would have
dropped an executable bit without an explicit `chmod`. A single shared path
target would have let a patch race an edit of one of its other files. Both are
fixed in this branch.

One near miss is worth recording. The array-size test kept each expected count
in two places, the comparison and the failure message. A mechanical
replacement updated the message and not the condition, and the test failed
with "base array has 24 tools, want 24". The suite caught it; review did not.

## Mechanical friction

There is no `-update-golden` flag for the tool arrays or the prompt heads.
Regenerating them means a throwaway program that calls the same functions the
tests call and writes the files. That is quick, and the next phase pays it
again.

The counts in comments and tests are duplicated across several packages:
`definitions_golden_test.go`, `policy_test.go`, `prompt_test.go`, the package
docs of `internal/tools`, and `prompt.go`. Phase 4's instruction to replace
restated counts with a pointer to `internal/tools/definitions.go` will pay for
itself the first time a tool is added again.

`go test ./...` takes about a minute on this machine. The build gate takes
longer because it cross-compiles six targets.

## Memory suggestions

- `MutationTargets` is the plural seam: a call reports every file it rewrites,
  and the runner merges path groups so a call naming several files is ordered
  against all of them. Phase 3's MCP tool is not mutating and does not touch
  it.
- `apply_patch` requires a prior read of every existing file it changes; a
  file it creates needs none. The gate is deliberately the same one `write`
  and `edit` carry, and it is why a binary file cannot be deleted through the
  tool.
- A tool-dropping variant needs no replacement entry when the head's rule
  about that tool is its own fragment gated on the tool. The `no-patch`
  variant relies on that: the head's apply_patch rule is a separate fragment
  from the `read`/`write`/`edit` rule.
- The tool arrays and prompt heads have no `-update-golden` flag. Regenerate
  them from `tools.DefinitionsFor` and `session.RenderSystemPromptFor` with a
  throwaway program, then delete it.
- Array sizes are restated in comments and test assertions across
  `internal/tools` and `internal/session`. Phase 4 replaces the comments with
  a pointer to `internal/tools/definitions.go`; the assertions stay as the one
  place a size change has to be acknowledged.
- `Result.Diff` remains Edit's one-file line diff. A multi-file tool reports
  its files in prose instead.
