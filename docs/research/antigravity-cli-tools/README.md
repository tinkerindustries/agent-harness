# Antigravity CLI: tools sent to the model

Google's terminal agent is `agy` (Antigravity CLI). This directory records which
tools it declares to the model on each request, captured from the request body
rather than inferred from documentation.

| File | Contents |
| --- | --- |
| `agy-tools-gemini-4-neon-high.json` | Tool declarations from a `gemini-4-neon-high` request |
| `agy-tools-gemini-3.8-flash-high.json` | Tool declarations from a `gemini-3.8-flash-high` request |
| `capture_server.py` | The local logger used to capture both |

Each JSON file holds the declarations exactly as sent: `name`, `description` and
the full parameter schema. A `_meta` block records the model, settings and
capture method. To count the tools, run
`jq '.functionDeclarations | length' <file>`.

## Where the tools come from

The harness inside `agy` is the same runtime that the `google-antigravity`
Python SDK ships
([antigravity-sdk-python](https://github.com/google-antigravity/antigravity-sdk-python)).
The SDK's `BuiltinTools` enum and `CapabilitiesConfig` control which tools the
model sees. `enabled_tools` is an allowlist and `disabled_tools` is a denylist.
A disabled tool is removed from the model's context. A policy that denies a tool
keeps it visible and rejects the call at run time.

The CLI sends a different set from the SDK enum. The CLI declares `read_file`,
`write_file`, `edit_file` and `manage_task`. The SDK enum names `view_file`,
`create_file`, `edit_file` and `run_command`, and adds `read_url_content`,
`generate_image` and `finish`. The CLI binary also carries older names
(`view_file`, `write_to_file`, `replace_file_content`, `command_status`,
`find_by_name`, `grep_search`, `list_dir`). Its changelog says the last three
left the default baseline.

## Tools in the captured requests

Parameter names ending in `*` are required.

| Category | Tool | Parameters |
| --- | --- | --- |
| Files | `read_file` | `Path*`, `StartLine`, `EndLine`, `ByteOffset` |
| Files | `write_file` | `Path*`, `Content*`, `Overwrite`, `Append`, `ArtifactMetadata` |
| Files | `edit_file` | `Path*`, `Target*`, `Replacement*`, `ArtifactMetadata` |
| Shell | `run_command` | `CommandLine*`, `WaitMsBeforeAsync*`, `toolSummary*`, `IsDaemon`, `BypassSandbox` (3.8 Flash only) |
| Shell | `manage_task` | `Action*` (`list`, `kill`, `status`, `send_input`), `TaskId`, `Input` |
| Shell | `schedule` | `Prompt*`, `DurationSeconds` or `CronExpression`, `TimerCondition`, `ExactTiming`, `IsDaemon`, `MaxIterations` |
| Web | `search_web` | `query*`, `domain` |
| Agents | `invoke_subagent` | `TypeName*`, `Role*`, `Prompt*`, `Model`, `Workspace` |
| Agents | `define_subagent` | `Name*`, `Description*`, `SystemPrompt*`, `EnableWriteTools`, `EnableMCPTools`, `EnableSubagentTools` |
| Agents | `manage_subagents` | `Action*` (`list`, `kill`, `kill_all`), `ConversationIds` |
| Agents | `send_message` | `Recipient*`, `Message*` |
| Agents | `run_workflow` | `Script` or `ScriptPath`, `Args`, `Cwd`, `Description`, `WaitMsBeforeAsync` |
| Agents | `swarm` | `action*`, `name`, `brief`, `model`, `fork`, `n`, `tasks`, `workspace`, `title`, `all` |
| User | `ask_question` | `questions*` |

The JSON files carry the enums, descriptions and nested object schemas. There is
no grep, find or list tool. The model searches through `run_command`.

## Neon against 3.8 Flash

The two requests declare the same tool names with identical descriptions. After
normalising encoding, the only content difference is that Neon's `run_command`
has no `BypassSandbox` parameter. Whether that comes from the model or from the
session is not established: the Neon capture used the Gemini API key route, and
the 3.8 Flash capture used the Google OAuth route.

The schema encoding differs. The Neon request sends `parametersJsonSchema`
(JSON Schema, with `$schema` and `additionalProperties: false`, integer
`minLength`). The 3.8 Flash request sends the older `parameters` object with
upper-case type names and string `minLength`. Both requests set
`thinkingLevel` to `HIGH` and `maxOutputTokens` to 65536.

A feature flag in the binary, `orchestrator-model-roles`, lists Neon, Carbon,
Argon and `claude-opus-5-5` as orchestrators, and Hydrogen and
`claude-sonnet-5-5` as executors. No tool difference between roles appears in
these captures.

## Limits of the captures

- Each file is the first model request of a print-mode run in a workspace outside
  the trusted list, with no MCP servers. Other setups can send more tools.
- Only `gemini-4-neon-high` and `gemini-3.8-flash-high` were captured. Carbon,
  Hydrogen and Argon were not.
- The system prompt and conversation from each request are not kept here.
- The `agy` build was one whose `--version` printed 1.2.0 while its embedded
  changelog ran to 1.5.13. Re-capture after an update.

## Reproducing a capture

Neon is only selectable in key mode. The Google OAuth login lists 3.8 Flash,
Claude and GPT-OSS models, with no Gemini 4 models.

1. Start the logger: `python3 -I capture_server.py /tmp/agy-capture`. With no
   upstream it logs each request and answers 500. Pass an upstream URL as the
   second argument to forward requests and log the responses as well.
2. Run `agy` against it with a dummy key. The key only goes to the local logger.

   ```
   GEMINI_API_KEY=dummy GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:18765 \
     agy --settings '{"modelProvider":"gemini"}' -p "hi" --model gemini-4-neon-high
   ```

3. Read the `streamGenerateContent` line in `exchanges.jsonl`. The tools are at
   `tools[0].functionDeclarations` in its `req` field.

For the OAuth route, set `CLOUD_CODE_URL` to the logger instead and give the
logger the upstream `https://cloudcode-pa.googleapis.com`. `agy` needs real
`loadCodeAssist` answers before it accepts a model, so the logger has to
forward. The generation request then also reaches Google.

`agy --settings` applies a settings object for one session without saving it.
`modelProvider: "gemini"` and `GOOGLE_GEMINI_BASE_URL` are documented in the
embedded changelog. `CLOUD_CODE_URL` is an environment variable the binary
reads, which this research found by testing.
