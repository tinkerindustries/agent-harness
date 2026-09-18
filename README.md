# agent-harness

A coding harness for running an agent loop against a model provider's own API.
DeepSeek is the default provider; Google's Gemini models run through the same
loop. The loop reads and writes files, runs commands, and iterates until a
task is done — targeting a provider's real behaviour rather than a
provider-agnostic abstraction.

It is one process hosting one coding session for a parent application, over
stdin and stdout. The parent spawns it, owns the working directory, supplies
the credentials, and reads the session's events back off the pipe. The
protocol is the OpenAI Responses API's vocabulary rather than one of ours:
`POST /responses` and that surface's semantic server-sent events
([docs/STDIO-PROTOCOL.md](docs/STDIO-PROTOCOL.md)).

## What you need

- **Go**, to build the binary. Nothing else is required to run a session.
- **`node` on `PATH`**, only for the `Screenshot` tool, which drives a
  headless browser through a Node script. Without it that one tool reports
  that it is unavailable and the rest of the session is unaffected.
- **An API key** for whichever provider you intend to run: DeepSeek from
  <https://platform.deepseek.com/api_keys> (prepaid, needs a balance), or
  Google's from <https://aistudio.google.com/apikey>. Neither is required and
  neither implies the other.

## Setup

```sh
git clone https://github.com/mrgeoffrich/agent-harness.git
cd agent-harness
scripts/build.sh
```

That leaves `bin/harness`. The credentials are the parent process's to
supply, in `GEMINI_API_KEY` / `GOOGLE_API_KEY` and `DEEPSEEK_API_KEY`. To
drive the process by hand instead, put them in a file and name it:

```sh
cp .env.example .env
# fill in the keys you have
bin/harness stdio-session -env .env
```

The process then waits on stdin for protocol frames and writes frames back on
stdout. Nothing else ever reaches stdout; logs go to stderr.

## Running a session

A parent sends a `create` naming the model, the prompt, the permission mode
and `harness.cwd` — the directory the session works in, which this process
never chooses for itself. `readonly` refuses any command that writes outside
that directory; under `full` a session can run any command. A deny list on the
request narrows either mode further.

The session's whole event stream comes back as notifications: reasoning and
content as they arrive, every tool call and its result, usage and cost per
request, and a terminal frame carrying the run's status.
[docs/STDIO-PROTOCOL.md](docs/STDIO-PROTOCOL.md) is the wire reference and is
what a client is built from.

### Flags

| Flag | What it does |
| --- | --- |
| `-state-dir` | Where this session's SQLite log and transcript mirror live. Without one, a per-pid directory under the user cache dir, removed on exit. |
| `-keep-state` | Leave that directory behind, for reading a finished session's transcript. |
| `-model` | The model a create body with no `model` runs on. |
| `-prices` | The price table, for the cost figures on `harness.usage`. Defaults to `configs/prices.json`. |
| `-env` | Read the API keys from this KEY=VALUE file when the environment does not carry them. |

### Models

Every Gemini model the harness routes, plus the one DeepSeek model it
routes, `deepseek-flash`, which reads images natively. A model that could
not see would be given vision tools that reach Google, so hosting one would
need a second provider's key
([docs/DEEPSEEK-VISION.md](docs/DEEPSEEK-VISION.md)).

## Where state lives

Under `-state-dir`: `session.db`, the event log the loop's state machine is,
and `sessions/`, the transcript mirror written alongside it. The database is
authoritative; the mirror is never rebuilt from it. Transcripts carry
workspace paths, file contents and command output, so treat them as
sensitive.

## Troubleshooting

**"no API key".** The process reads `GEMINI_API_KEY` or `GOOGLE_API_KEY` for
Google's models and `DEEPSEEK_API_KEY` for DeepSeek's, from the environment
it was spawned with. Pass `-env` to read a file instead.

**The parent sees nothing on stdout.** Check stderr: the process logs there,
including the reason it refused to start.

**A model is refused.** `harness stdio-session` hosts every Gemini model and
exactly one DeepSeek model; the error names which.

## Documentation

- [CLAUDE.md](CLAUDE.md) — the rules an agent working in this repo has to know
- [ARCHITECTURE.md](ARCHITECTURE.md) — how the packages relate and the
  invariants between them
- [`internal/CLAUDE.md`](internal/CLAUDE.md) — the codemap: one entry per Go
  package, what it is for and what it may depend on
- [TESTING.md](TESTING.md) — the suite and the smoke sequence
- [`docs/STDIO-PROTOCOL.md`](docs/STDIO-PROTOCOL.md) — the wire protocol, and
  what a client is built from
- [`docs/DESIGN.md`](docs/DESIGN.md) — the technical design and why each choice
  was made; the reference for anything on the request path
- [`docs/TOOLS.md`](docs/TOOLS.md), [`docs/MODELS.md`](docs/MODELS.md),
  [`docs/PROMPTING.md`](docs/PROMPTING.md), [`docs/CACHE.md`](docs/CACHE.md) —
  the tool set, model routing, prompt wording, and the prompt cache
- [`docs/OBSERVED.md`](docs/OBSERVED.md) — findings measured against the live
  API, which override the vendored docs where they disagree
- [`third_party/deepseek-docs/`](third_party/deepseek-docs/README.md) — a
  Markdown mirror of DeepSeek's own API documentation

## Licence

MIT. See [LICENSE](LICENSE). The vendored documentation under `third_party/`
belongs to its publishers and is not covered by this licence.
