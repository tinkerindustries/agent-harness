# Tool vocabulary alignment with the DeepSeek harness — feature memory

Every session working on this feature reads this file. Keep it to one screen. No code.

## Schema

None. The feature changes tool names, argument names and tool-array membership.
No wire type, store column or interface changes its shape, and `MCPProvider`
keeps the methods it has.

## Invariants

- Every built-in tool name is `lower_snake_case`. A tool that reaches an MCP
  server keeps the `mcp__<server>__<tool>` shape.
- The mapping table in the plan is the complete rename list, and phase 1 carries
  all of it. No later phase renames a tool.
- The provider tool arrays are frozen for a session's life. A rename or an added
  tool moves all three array goldens, and both prompt goldens whenever the
  array's membership or a fragment's wording moves.
- Membership is the array's decision; permission gates execution only. Every
  tool ships in every mode. `alwaysAllowed` in `internal/tools/policy.go` is the
  read-only list.
- `write`, `edit` and `apply_patch` are mutating and never in `alwaysAllowed`.
- `apply_patch` parses and applies every hunk in memory before writing any file.
  One hunk that does not apply leaves the workspace untouched.
- `definitionsVisionCapable` is DeepSeek's array minus the six vision tools;
  `definitionsClaude` is that array minus `web_fetch`. A tool added to DeepSeek
  reaches both unless it is named in a drop list.
- The five MCP access tools join an array only when at least one MCP server is
  configured (`WithMCP`), so changing them moves no golden file.
- MCP listings read the stored snapshot; reading dials. The template listing
  follows the same rule.

## Files that matter

| Path | Why an agent needs it |
| --- | --- |
| `internal/tools/definitions.go` | The three frozen arrays, the MCP access tools and `MCPAccessToolCount`; every name and schema lives here |
| `internal/tools/registry.go` | `toolFuncs` dispatch, `timeoutFor` name cases and the executor |
| `internal/tools/policy.go` | `alwaysAllowed` and the per-server MCP read-only gate |
| `internal/tools/descriptor.go` | The string a deny pattern matches and a denial shows; rename its cases |
| `internal/tools/mutation.go` | Same-file ordering for mutating tools; a new mutating tool belongs here too |
| `internal/tools/bashbackground.go` | `Shells` and background-shell state; the job wait and listing read it |
| `internal/session/prompt.go` | `toolOrder` and `toolFragments`; every fragment names tools in prose |
| `internal/session/tooldispatch.go`, `livestate.go` | Task tool name comparisons and the Bash live-state case |
| `internal/promptvariant/variants.go` | The `no-bash` drop list; the `no-patch` variant is added here |
| `internal/tools/testdata/tools_{claude,deepseek,kimi}.golden.json` | Frozen arrays; regenerate, never hand-edit |
| `internal/session/testdata/prompt_{deepseek,kimi}.golden.txt` | Frozen heads; regenerate, never hand-edit |
| `internal/tools/mcpresources.go` | The fixed MCP access tools and their executors |
| `internal/mcpclient/resources.go`, `refresh.go` | The resource/template snapshot and the live reads |
| `docs/TOOLS.md`, `docs/MCP.md` | The catalogue, per-tool rules and the MCP tool table |
| `docs/plans/deepseek-tool-vocabulary.md` | The mapping, phases, proofs and **Not building** |

## External facts

| Fact | Source |
| --- | --- |
| dsh names every built-in tool `lower_snake_case`: `read`, `write`, `edit`, `bash`, `glob`, `grep`, `job_output`, `job_list`, `job_kill`, `web_fetch`, `apply_patch`, `list_mcp_resource_templates`, `list_mcp_resources`, `read_mcp_resource` | <https://github.com/deepseek-ai/deepseek-harness>, `docs/tool-catalog.md` |
| dsh's `bash` takes `timeoutMs` and `workdir`; `job_output` takes `job_id` and `wait`; `job_kill` takes `job_id`; `apply_patch` takes `input` in the Codex grammar | same |
| Only `{"type": "custom", "name": "apply_patch"}` is accepted as a custom tool, and other custom names return 400. Built-in tool types, `web_search` included, are ignored | `third_party/deepseek-docs/guides/responses_api.md`, `api/create-response.md` |
| dsh ships `apply_patch` disabled behind an `applyPatch` flag; this harness ships it enabled | same catalogue, and the plan's decision |

The mapping table is the whole of the argument changes. dsh's `grep include`,
`job_output timeout_ms` and required `bash description` are not adopted.

## Vocabulary

- **dsh**: DeepSeek's own harness, `deepseek-ai/deepseek-harness`.
- **the mapping**: the Now/After table in the plan; the complete rename list.
- **goldens**: the committed JSON and TXT captures of the tool arrays and prompt heads.
