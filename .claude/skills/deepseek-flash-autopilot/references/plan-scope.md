# The plan-scope document

The plan says what gets built and in what order. The scope document says what
counts as in bounds, and it exists because an unattended chain of runs has no
other brake. It is written by a human, before the run, and it is not edited once
the run starts — by the orchestrator or by any phase agent.

Two properties make it work, and both come from it being frozen and external.
Every judgement call during the run gets checked against a fixed text rather
than against a memory that has been quietly reshaped by five phases of context.
And a rule you did not write yourself is one you cannot reason your way out of
at the point where it becomes inconvenient, which is exactly the point where it
matters.

## What it must contain

Three sections are load-bearing. The autopilot refuses to start without them,
because without them there is nothing to check against.

### What this delivers

One paragraph, present tense, describing the system working. This is the
referent for the load-bearing test — the question "would the delivered thing
still work without this?" is meaningless unless something says what the
delivered thing is.

Write the end state, not the activity. "The poller reads its interval from
config.yaml and picks up changes without a restart" is a referent. "Improve
configuration handling" is not — it cannot be failed, so nothing can be measured
against it.

### Non-goals

The things that are explicitly not being built, especially the ones a reasonable
engineer would otherwise assume come along for the ride. These are absolute: the
orchestrator treats a named non-goal as outranking its own reasoning, and a plan
that appears to require one is a plan that gets stopped rather than a non-goal
that gets overridden.

This is the highest-value section and the one most often left thin. Every
sentence here is a run that does not get spent on something you did not want.

### Fences

Concrete paths, packages, and behaviours that must not change, each with a
reason. Concrete is the operative word: the orchestrator checks a pull request's
file list against this section, so `internal/queue/**` is a fence and "don't
break the queue" is a sentiment.

Include behavioural fences where a file-level one would not catch it — a wire
format, an exported signature, a database schema, a CLI flag someone's scripts
depend on.

## What it should also contain

Not required to start, but the run is better with them.

### In scope

The surfaces that may be created or changed. The complement of the fences, and
useful mostly where the boundary is not obvious from the plan.

### Stack, where you care

If the plan stands up something new — a service, an app, a package in a language
the repo does not already use — the orchestrator settles the cross-cutting
conventions and the libraries itself at preflight, and records what it chose. Any
of those you have an opinion about belongs here instead, where it is binding.
"Same logging and error handling as `internal/queue`", "no ORM", "no new web
framework; the repo already has one" each cost a line and save an argument the
orchestrator would otherwise have with itself. Leave out the ones you do not
mind either way — this section is for preferences you would be annoyed to lose,
not a stack specification.

### Definition of done

The checks that must pass for the whole piece of work, as commands. The
orchestrator runs these on the integration branch before opening the pull
request to `main`, and whether they pass decides draft or ready.

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

## Fences

- `internal/queue/**` — retry logic is being rewritten in another branch; a
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

Note what the non-goals section is doing there. Hot reload, the other settings,
and env overrides are all things a capable agent mid-phase would have a good
argument for adding. Written down, each one is a decision already made; left
unwritten, each one is an argument the orchestrator has to win six times.
