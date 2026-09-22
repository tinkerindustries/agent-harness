# Testing

One suite, Go, run through `scripts/test.sh`. There is no CI — the suite runs
where you run it, which makes the smoke sequence at the bottom of this file
the only gate there is.

## Test layers

| Layer | Purpose here | Touches | Runner | Lives in |
| --- | --- | --- | --- | --- |
| Unit | Everything that is a pure function of its inputs: folds, diffs, SSE parsing, tool-call assembly, permission decisions, pricing | Nothing external; a temp dir and a local shell at most | `go test` | Beside the code, `internal/<pkg>/*_test.go` |
| Integration | The store's edges against a real SQLite file, and the stdio protocol end to end against a fake provider | A fresh SQLite file in a temp dir | `scripts/test.sh` | `internal/store`, `internal/stdiosession` |

Nothing in the suite calls `api.deepseek.com`. Findings that needed the live
API were measured by hand and written down in
[`docs/OBSERVED.md`](docs/OBSERVED.md) rather than turned into tests.

There is no end-to-end layer against a real model. A full run costs minutes
and real money, so the equivalent is the manual smoke check below.

## Running tests

| What | Command |
| --- | --- |
| Everything | `scripts/test.sh` |
| One package | `go test ./internal/session/...` |
| One test | `go test ./internal/session -run TestResumeContinuesSubTurnNumbering -v` |
| With the race detector | `scripts/test.sh -race` |

`scripts/test.sh` runs the whole suite — no broker, no other service, every
test on its own SQLite file in a temp dir. Flags after the script name pass
through to `go test`.

### Prerequisites

- **A POSIX shell** — the `Bash` tool tests run commands.
- No API key, no `.env`, no network.

## What to test where

- **`internal/deepseek`** — the parsing and assembly edges that a generic
  OpenAI-compatible client gets wrong: oversized SSE lines, `:` comments,
  `[DONE]`, tool-call `arguments` fragmenting mid-token, retry classification.
  Don't mock the whole API to assert the harness sends what it sends; the
  request-shape rules are asserted in `internal/session`'s prefix tests where
  they matter.
- **`internal/session`** — the invariants: prefix stability, tool-result
  ordering, resume replaying an event log to the same messages. These are the
  tests that fail when someone perturbs the cached head, and the reason to write
  a new one is that a change touched the request path.
- **`internal/stdiosession`** — the wire contract with a process this repo
  does not contain, in all three vocabularies. Every field on it is a
  promise, so the end-to-end tests drive a session over a pipe against a
  fake provider and read the frames back. `TestGoldenFrames` and
  `TestManagedAgentsGoldenFrames` go further and compare a whole scripted
  run's frames against a checked-in capture, byte for byte: the other tests
  assert facts about frames and would not notice a field that quietly
  changed name or stopped being emitted. Re-record either with
  `-run <name> -update-golden` when the matching protocol document sanctions
  the change, and put the diff in the commit. A test that wants the
  Interactions vocabulary calls `useDialect`; one that wants ManagedAgents
  calls `newManagedAgentsFixture`; the rest get the Responses one.
  `endtoend_test.go` and `claude_test.go` go further still, for DeepSeek and
  Claude respectively: a real provider client against a fake HTTP recorder at
  one end of the pipe and a real client at the other, so the outbound
  provider request and the inbound parent frames are both asserted from the
  same run rather than each dialect's shape being taken on faith from the
  other end. `managedagents_test.go` is the same shape again, against
  ManagedAgents' own methods: a create whose model calls a client-declared
  custom tool, the async round through `sessions.events` (including a stray
  event refused while a result is pending), steer, interrupt, get by turn id,
  delete and resume.
- **`internal/tools`** — argument validation, workspace confinement, and every
  permission decision in both modes. Policy tests also assert that the tool
  *definitions* are unchanged by mode, which is the cache invariant in test form.
- **`internal/fold`, `internal/store`** — pure transforms with obvious inputs and
  outputs; cheap, so cover the edges.

## Conventions

- Tests sit beside the code, `package foo` rather than `foo_test`, and reach
  into unexported identifiers freely.
- Table-driven where there is a table; a named `t.Run` per case.
- Standard library only. No assertion library, no mocking framework — collaborators
  are small interfaces satisfied by a struct declared in the test file.
- Comments on the non-obvious tests name the design section or measurement they
  are pinning. Keep that up: it is what tells the next person whether a failure
  means the code broke or the requirement changed.

## Known-awkward tests

**Time-dependent store behaviour is driven by an explicit `now` argument**,
never by sleeping, so the suite runs in seconds and does not flake under load.

## Coverage

No enforced threshold, and no coverage job. `go test -cover ./...` reports the
number if you want it.

## Smoke test after a change

`scripts/build.sh` runs all of this, stopping at the first failure. Run the
individual commands when you want one of them on its own.

1. `gofmt -l cmd internal` — silence means clean.
2. `go vet ./cmd/... ./internal/...`
3. `scripts/test.sh`
4. `go build -o bin/harness ./cmd/harness`

Then drive a real session, because nothing above talks to a model:

```
DEEPSEEK_API_KEY=sk-... bin/harness stdio-session -state-dir /tmp/smoke -keep-state
```

Send a `create` naming a small task and a `harness.cwd` you do not mind being
written to, and read the frames back. `-keep-state` leaves
`/tmp/smoke/session.db` and the transcript mirror behind, which is what to
read when the frames do not say enough.
