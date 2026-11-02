# Phase 3: MCP resource templates

## What was built

`list_mcp_resource_templates` joins the MCP access tools as the fifth.

- `internal/tools/mcpresources.go`: `execMCPListResourceTemplates` answers from
  `MCPProvider.Resources`, the stored snapshot, and keeps only the entries whose `Template`
  flag is set. It shares one renderer with `list_mcp_resources`
  (`listMCPResources`, parameterised by a `templatesOnly` flag), so both listings print the
  same columns and both obey the output cap. The optional `server` argument filters exactly
  as it does in `list_mcp_resources`. No server is dialled and no `MCPProvider` method was
  added.
- Empty answers: `No configured MCP server advertises any resource templates.` when no
  server has one, and `The "<server>" MCP server advertises no resource templates.` when
  the filter names a server that has none. A server holding resources but no templates gets
  the templates wording rather than a repeat of its resources.
- `internal/tools/definitions.go`: the schema sits after `list_mcp_resources`, takes the
  optional `server` argument, and says a template's `{placeholders}` must be filled in
  before `read_mcp_resource` can read it. `MCPAccessToolCount` moved from 4 to 5 and the
  compile-time length check still holds.
- `internal/tools/registry.go`'s `toolFuncs`, `internal/tools/policy.go`'s `alwaysAllowed`
  (a listing reads the stored snapshot, so read-only mode allows it), and
  `internal/tools/descriptor.go`'s case (the server name goes into the descriptor, so a
  deny pattern naming a server covers the new tool) each gained the name.
- `internal/session/mcp_test.go`'s `mcpAccessToolNames` spells the access array out for the
  run/resume array comparisons. It carries five names now.
- `docs/MCP.md`: `five fixed tools`, `Five definitions`, and a new row in the resources
  table.
- Every comment that stated the count as four moved to five: `definitions.go`,
  `registry.go`, `policy.go`, `descriptor.go`, `mcpresources.go`,
  `internal/mcpclient/resources.go`, `definitions_test.go` and `session/mcp_test.go`.

No golden file moved. The access tools are inserted by `WithMCP` only when at least one
server is configured, and every golden test resolves its array with no MCP provider.

One judgement call: the new row in `docs/MCP.md`'s resources table is spelled
`list_mcp_resource_templates`, while the four rows above it still carry their pre-rename
`MCPListResources`-style names. Using the name the tool actually has was the choice; the
rest of the table is phase 4's to correct. A model reading that table can call the new tool
correctly either way.

Tests added or extended:

- `TestMCPListResourceTemplatesListsTemplatesOnly` covers templates only, and checks beside
  it that `list_mcp_resources` still returns both kinds.
- `TestMCPListResourceTemplatesSaysSoWhenThereAreNone` covers the empty snapshot and the
  resources-but-no-templates case.
- `TestMCPListResourceTemplatesFiltersByServer` covers the `server` argument and the
  filtered empty answer.
- `TestMCPAccessToolsWithoutAProvider` and `TestReadOnlyModeGatesTheTwoDiallingAccessTools`
  now cover the fifth name.
- `TestMCPAccessToolsAreInTheDispatchTable` in `internal/tools/mcp_test.go` checks that the
  slice length matches `MCPAccessToolCount`, that every access tool has a `toolFuncs`
  entry, and that the new name is present.

## What was verified, and how

The phase's proof, run with `-count=1` so nothing came from the test cache:

```
$ go test -count=1 ./internal/mcpclient ./internal/tools
ok  	github.com/mrgeoffrich/agent-harness/internal/mcpclient	0.791s
ok  	github.com/mrgeoffrich/agent-harness/internal/tools	9.713s
```

`gofmt -l internal cmd` and `go vet ./internal/tools ./internal/mcpclient ./internal/session`
printed nothing before the proof ran.

The whole tree was run as well, because the change moves an array that `internal/session`
compares position by position and the phase's own proof does not include that package:

```
$ go test -count=1 ./...
... every package ok ...
--- FAIL: TestJobOutputConcurrentPollsDoNotPanicOrDuplicate (0.05s)
    bashbackground_test.go:365: expected "line-1\n" somewhere across the concurrent polls, got a 12848-byte combined result
    bashbackground_test.go:365: expected "line-100\n" somewhere across the concurrent polls, got a 12848-byte combined result
    bashbackground_test.go:365: expected "line-200" somewhere across the concurrent polls, got a 12848-byte combined result
FAIL	github.com/mrgeoffrich/agent-harness/internal/tools	10.431s
FAIL
```

That is a pre-existing, load-dependent test flake, described below and recorded in
`docs/issues/open/flaky-job-output-concurrent-polls-test.md`. The package passed its own
proof run in the same session. The test passed 20 consecutive runs on its own after the
failure:

```
$ go test -count=20 -run 'TestJobOutputConcurrentPollsDoNotPanicOrDuplicate' ./internal/tools
ok  	github.com/mrgeoffrich/agent-harness/internal/tools	0.732s
```

`go test -count=1 ./internal/session` passed inside that same full run, which is the check
that the five-name array in `mcpAccessToolNames` matches what `WithMCP` builds.

## What was left undone

`docs/MCP.md`'s resources table still names the four older MCP access tools by their
pre-rename `MCPListResources`, `MCPReadResource`, `MCPListPrompts` and `MCPGetPrompt`
spellings, and the two prose references below the table use `MCPReadResource`,
`MCPListPrompts` and `MCPGetPrompt`. Phase 4 owns the documentation sweep and its phase text
does not name `docs/MCP.md`, so it may need telling.

Nothing from phase 2 or phase 4 is here. No `apply_patch`, and no `docs/TOOLS.md`,
`internal/CLAUDE.md`, `ARCHITECTURE.md` or `docs/DESIGN.md` change.

A live `resources/templates/list` call was not added, `MCPProvider` gained no method, and
the stored snapshot format is unchanged, all as the phase required.

## Bugs found and not fixed

`TestJobOutputConcurrentPollsDoNotPanicOrDuplicate` is flaky under the load of a full
`go test ./...`. It joins the content of 400 concurrent `job_output` results and one final
poll, then looks for `line-1\n`, `line-100\n` and `line-200` in that joined text. Each
result starts with `Status: <state>` and a blank line, and a reader can catch the output
buffer between two writes, so a payload may end mid-line and the next payload carries the
rest of it. The joined text then holds `line-1Status: running`, which contains neither
needle. The tool's behaviour is correct; the test's assumption about where the read
boundaries fall is not. Recorded in
`docs/issues/open/flaky-job-output-concurrent-polls-test.md`.

## Mechanical friction

The phase's proof command does not run `internal/session`, yet the change moves the MCP
access array that package asserts on position by position. A green proof would have hidden
a red package, so the whole tree had to be run by hand.

`go test ./... | tail -60` holds every line until the command exits, so the failure was only
visible at the end. Running the failing package alone next to the loaded run is what showed
the failure was load-dependent rather than a real regression.

## Memory suggestions

- `WithMCP` inserts the access tools ahead of a configured server's own, so adding one
  shifts every later index. `internal/session`'s MCP tests spell the access names out and
  compare the whole sent array element by element, so a new access tool fails those tests
  even though no golden file moves.
- `MCPAccessToolCount` and the `mcpAccessTools` slice length are pinned against each other
  at compile time. Adding a definition without moving the constant fails the build, and
  moving the constant without a definition fails it too.
- The probe already writes resource templates into the same stored resources list with a
  `Template` flag set, so a template listing is a filter over `MCPProvider.Resources` rather
  than a new method, a second probe, or a snapshot field.
- `docs/MCP.md`'s resources table names four MCP access tools by their pre-rename TitleCase
  spellings. Any phase that reads that table for tool names will find the old names there
  and the current names in the code.
