# The plan-scope document

Written by a human before the run, frozen once it starts, and not edited by the
orchestrator or by any phase agent. It is the authority on what may and may not
be built.

Two properties make it work, and both come from it being fixed and external.
Every judgement during the run is checked against a stable text rather than
against a memory that five phases of context have quietly reshaped. And a rule
somebody else wrote is one that cannot be reasoned away at the moment it becomes
inconvenient — which is the only moment it matters.

## Required sections

The runner refuses to start without these three. Without them there is nothing
to check against.

### What this delivers

One paragraph, present tense, describing the system working. The referent for
the runner's central test — *would the delivered thing still work without this?*
— which is meaningless unless something says what the delivered thing is.

End state, not activity. It has to be failable by a running system.

### Non-goals

The things explicitly not being built, especially the ones a reasonable engineer
would assume come along for the ride. These are absolute: the runner treats a
named non-goal as outranking its own reasoning, and a plan that appears to
require one gets stopped rather than the non-goal overridden.

The highest-value section, and the one most often left thin. Every line here is
a run that does not get spent on something nobody wanted.

### Fences

Concrete paths, packages, and behaviours that must not change, each with its
reason. Concrete is the operative word — the runner checks a pull request's file
list against this section, so `internal/queue/**` is a fence and "don't break
the queue" checks nothing.

Include behavioural fences where a file-level one would miss it: a wire format,
an exported signature, a database schema, a CLI flag someone's scripts pass.

## Optional but worth having

**In scope** — the surfaces that may be created or changed. The complement of
the fences, useful where the boundary is not obvious from the plan.

**Definition of done** — the checks for the whole piece of work, as commands.
Run on the integration branch before the final pull request; whether they pass
decides draft or ready.

---

## Example

```markdown
# Plan scope — config file support

## What this delivers

The harness reads its poll interval, retry ceiling, and log level from a
config.yaml beside the binary. A missing file falls back to the flag defaults
that exist today, so an existing deployment keeps working untouched. A malformed
file fails at startup with a message naming the field, rather than starting with
silent zero values.

## Non-goals

- Hot reload. The file is read once at startup. A change needs a restart.
- Any other setting. Only these three move to config; everything else stays on
  flags, including the ones that obviously "should" move too.
- Environment variable overrides. Flags and config only.
- A config file for the MCP server. This is the harness binary alone.
- New dependencies. Validation is hand-rolled against the standard library YAML
  package already vendored.
- Changing existing tests. Add to them; a test that has to change to accommodate
  this is a signal to stop, not to edit.

## Fences

- `internal/queue/**` — retry logic is being rewritten on another branch; a
  change here conflicts on merge.
- `cmd/harness/serve.go` HTTP route table — unrelated, and reviewed separately.
- The `-poll-interval`, `-retry-max`, and `-log-level` flags keep their current
  names and behaviour when no config file is present. Deployments pass them.
- Database schema. No migration is in scope.

## In scope

- A new `internal/config` package.
- The startup path in `cmd/harness/main.go`, to load and validate.
- The three call sites that read those values today.
- Tests alongside each of the above.

## Definition of done

- `scripts/test.sh` passes.
- `gofmt -l cmd internal` is empty and `go vet ./cmd/... ./internal/...` is clean.
- `harness serve` with no config.yaml behaves as it does on main.
- `harness serve` with a config.yaml missing a required field exits non-zero and
  names the field.
```

Note what the non-goals are doing there. Hot reload, the other settings, env
overrides, the dependency, and the test edits are all things a capable agent
mid-phase would have a good argument for. Written down, each is a decision
already made. Left out, each is an argument that has to be won six separate
times by someone who is not in the room.
