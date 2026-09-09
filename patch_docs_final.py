jobs = []

# ---- internal/CLAUDE.md ---------------------------------------------------
p = 'internal/CLAUDE.md'
s = open(p, encoding='utf-8').read()
old = s[s.index('### `internal/responsesstdio`'):s.index('### `internal/attachment`')]
new = '''### `internal/responsesstdio`
The protocol `harness stdio-session` speaks: JSON-RPC 2.0 over stdin and
stdout, carrying the OpenAI Responses API's own vocabulary rather than one of
this repo's invention — its REST methods on `POST /responses` as JSON-RPC
methods, and that surface's semantic server-sent events as notifications
(docs/STDIO-PROTOCOL.md). Two translators around an unmodified
`session.Runner`: a create-response body becomes `RunOptions`, and the
session's committed events plus the hub's live text deltas become
`response.*` events. It streams text from the live frames and takes structure
— tool calls, results, usage, the run's end — from the log, which is why a
`reasoning` item and a `message` item can be open at once here and never are
on the HTTP surface's own stream.

The vocabulary is the same one `internal/deepseek` sends the provider
(docs/DEEPSEEK-RESPONSES.md), which is the point: a `function_call` item a
parent reads is the `function_call` item the provider was sent. Two output
item types are this package's own, and both follow from a response being a
whole agentic run rather than one model turn — `function_call_output`, which
on the HTTP surface a client sends back rather than receives, and a `message`
with `role: "user"`, which there only ever appears in input.

Also implements `tools.MCPProvider` for the two tool shapes a client may
declare, `function` (called back over the pipe) and `mcp_server` (dialled by
`internal/mcpclient` as any configured server is). `modelinfo.go` is the one
file here that knows a model has a provider at all: the handshake's per-model
details and the create's effort check are answered out of `internal/gemini`'s
or `internal/deepseek`'s tables, so the two providers' differing effort sets
reach a client as data rather than as a special case anywhere else.
`resume.go` is the seam between the two ways a create names a conversation: a
response id is minted in memory and dies with the process, so continuing
across a restart goes by session id out of the `-state-dir` store instead, and
everything the session's prompt prefix is built from — model, workspace,
permission mode, deny patterns, and the frozen tool array the create has to
re-declare with live connection metadata — is checked against the row rather
than taken from the create. Depends on: `internal/session`, `internal/store`,
`internal/hub`, `internal/tools`, `internal/mcpclient`, `internal/provider`,
and — in `modelinfo.go` alone, for the descriptive tables the handshake
publishes — `internal/gemini` and `internal/deepseek`.

'''
s = s.replace(old, new, 1)
open(p, 'w', encoding='utf-8', newline='').write(s)
print('internal/CLAUDE.md ok')

# ---- ARCHITECTURE.md ------------------------------------------------------
p = 'ARCHITECTURE.md'
s = open(p, encoding='utf-8').read()
pairs = [
("""The protocol is Google's own Interactions vocabulary rather than one of this
repo's invention: the methods are the four REST methods on
`POST /v1beta/interactions`, and the notifications are that surface's
server-sent events, so both sides of the process speak the same API.
[`docs/STDIO-PROTOCOL.md`](docs/STDIO-PROTOCOL.md) is the wire reference and
the record of where it departs from Google's HTTP surface and why.""",
 """The protocol is the OpenAI Responses API's own vocabulary rather than one of
this repo's invention: the methods are its REST methods on `POST /responses`,
and the notifications are that surface's semantic server-sent events.
[`docs/STDIO-PROTOCOL.md`](docs/STDIO-PROTOCOL.md) is the wire reference and
the record of where it departs from the HTTP surface and why.

**It is the same vocabulary underneath.** `internal/deepseek` posts to
DeepSeek's own `POST /responses`, so a `function_call` item the parent reads
is the `function_call` item the provider was sent and nothing between them
translates ([`docs/DEEPSEEK-RESPONSES.md`](docs/DEEPSEEK-RESPONSES.md)). The
Gemini models the process also hosts are translated into it by
`internal/gemini`, which is the one place a second vocabulary still lives."""),
("""    parent[parent application] <-->|JSON-RPC over stdio<br/>Google Interactions payloads| gs[internal/geministdio]""",
 """    parent[parent application] <-->|JSON-RPC over stdio<br/>Responses API payloads| gs[internal/responsesstdio]"""),
("""    session2 -->|wire.ChatIntent| dc[internal/deepseek]""",
 """    session2 -->|wire.ChatIntent| dc[internal/deepseek]
    dc -.->|POST /responses<br/>same vocabulary| dapi"""),
]
for old, new in pairs:
    if old not in s:
        print('ARCHITECTURE MISS:', old[:50].replace('\\n', ' '))
    s = s.replace(old, new, 1)
s = s.replace('internal/geministdio', 'internal/responsesstdio')
open(p, 'w', encoding='utf-8', newline='').write(s)
print('ARCHITECTURE.md ok')

# ---- CLAUDE.md ------------------------------------------------------------
p = 'CLAUDE.md'
s = open(p, encoding='utf-8').read()
pairs = [
("""  `internal/session` unchanged in a directory the parent owns. The name is the
  protocol's, not the model's: it hosts every Gemini model the harness routes
  and one DeepSeek model, `deepseek-v4-flash-vision-exp`, and refuses
  DeepSeek's other two because a model that cannot see is given vision tools
  that reach Google — so hosting one would need a second provider's key.""",
 """  `internal/session` unchanged in a directory the parent owns. It hosts every
  Gemini model the harness routes and one DeepSeek model,
  `deepseek-v4-flash-vision-exp`, and refuses DeepSeek's other two because a
  model that cannot see is given vision tools that reach Google — so hosting
  one would need a second provider's key."""),
("""  vocabulary rather than one of ours: the methods are the REST methods on
  `POST /v1beta/interactions` and the notifications are that surface's
  server-sent events, so `internal/gemini` speaks the vocabulary to Google and
  `internal/geministdio` speaks it to the parent.""",
 """  vocabulary rather than one of ours — **the OpenAI Responses API's**: the
  methods are its REST methods on `POST /responses` and the notifications are
  that surface's semantic server-sent events. `internal/responsesstdio`
  speaks it to the parent and `internal/deepseek` speaks it to DeepSeek, so
  one vocabulary runs the length of the process; `internal/gemini` translates
  for the Gemini models."""),
("""  [`docs/STDIO-PROTOCOL.md`](docs/STDIO-PROTOCOL.md) is the wire reference and
  is what a client is built from; read it before changing anything under
  `internal/geministdio`, because every field on it is a contract with a
  process this repo does not contain.""",
 """  [`docs/STDIO-PROTOCOL.md`](docs/STDIO-PROTOCOL.md) is the wire reference and
  is what a client is built from; read it before changing anything under
  `internal/responsesstdio`, because every field on it is a contract with a
  process this repo does not contain."""),
]
for old, new in pairs:
    if old not in s:
        print('CLAUDE MISS:', old[:50].replace('\\n', ' '))
    s = s.replace(old, new, 1)
s = s.replace('internal/geministdio', 'internal/responsesstdio')
open(p, 'w', encoding='utf-8', newline='').write(s)
print('CLAUDE.md ok')

# ---- docs/MCP.md and others ----------------------------------------------
for path in ['docs/MCP.md', 'docs/DEEPSEEK-RESPONSES.md', 'docs/DEEPSEEK-VISION.md',
             'docs/GEMINI-INTEGRATION.md', 'docs/OBSERVED.md']:
    try:
        s = open(path, encoding='utf-8').read()
    except FileNotFoundError:
        continue
    n = s.replace('internal/geministdio', 'internal/responsesstdio')
    if n != s:
        open(path, 'w', encoding='utf-8', newline='').write(n)
        print(path, 'ok')
