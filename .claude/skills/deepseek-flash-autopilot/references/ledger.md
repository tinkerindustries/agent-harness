# The ledger

One file, beside the plan document, named `<plan-basename>-ledger.md` and left
untracked. It is the live state of the run: rewritten after every phase, before
the next one launches.

Write it as though the reader has none of your context, because after a
compaction that reader is you. "Fixed the signature" means nothing an hour
later; "P2 changed `Load` to return `(*Config, error)` rather than
`(Config, error)` because the caller needs to distinguish absent from empty"
survives.

Three things about it are deliberate:

- **The live plan is authoritative, not the plan document.** Phase briefs are
  composed from here. The original plan is never edited, so it stays usable as
  the baseline you diff intent against at the end.
- **Every unilateral decision lands under assumptions.** There was nobody to ask,
  so the substitute is a record of what you decided and why, surfaced in the
  final pull request where a reviewer can check it.
- **The budget line survives compaction.** Relaunch counts held only in the
  conversation are counts that reset themselves silently.
- **The foundations are written once and quoted many times.** Where the run
  creates something new, this is where the conventions and the pinned versions
  live, and every brief afterwards copies from here. A convention held only in
  your head is one the phase after a compaction will not repeat.

## Template

```markdown
# <feature> — autopilot ledger

Started: <date>
Integration branch: deepseek/<feature>
Plan: <path>
Scope: <path> (frozen — not edited during this run)
Verification worktree: <path>

## Live plan

- [x] P1 <slug> — <what it leaves behind> — merged, PR #12
- [x] P2 <slug> — <what it leaves behind> — merged, PR #14
- [ ] P3 <slug> — <what it leaves behind>   ← next
- [ ] P4 <slug> — <what it leaves behind>

## Budget

Relaunches used: 1 of 3
- P2, attempt 1: timed out at sub-turn 400 with nothing pushed. Relaunched with
  the fixture generation cut from the brief.

## Foundations

Only where the run creates something new. Settled at preflight, quoted into
every brief, and not revisited by a phase.

- Errors: wrapped with `fmt.Errorf("...: %w", err)`, sentinel values only at
  package boundaries — matches `internal/queue`.
- Config: flags, with the `internal/config` loader added in P2. No env vars.
- Logging: `log/slog`, structured, one handler set up at startup.
- Tests: `go test` alongside the code, table-driven, no new test framework.
- Deps added this run, versions resolved from the registry on <date>:
  - `github.com/foo/bar v1.9.2` (P2) — YAML parsing. v2 exists but is a
    pre-release; briefs pin v1.
- Repo conventions this inherits rather than restates: build, CI, gofmt/vet.

## Amendments

- After P1: `Load` returns `(*Config, error)`, not `(Config, error)` as the plan
  had it — the poller needs to tell absent from zero-valued. P3's brief updated
  to match.
- After P2: validation moved out of P3 and into P2, which had already written
  most of it. P3 is correspondingly smaller.

## Assumptions

Decisions made without a user, and what would change if any is wrong.

- The plan does not say what a malformed config should do. Assumed: fail at
  startup, non-zero exit, naming the field — consistent with how the flag parser
  already behaves. If the intent was to warn and fall back, P2 needs redoing.

## Load-bearing additions

Work not in the plan, built because the deliverable does not function without it.

- P2 added `internal/config.defaults()`. The plan assumed the flag defaults were
  reachable from the config package; they were unexported in `cmd/harness`, so
  with no fallback source a missing file could not fall back at all.

## Adjacent — recorded, not built

- `internal/queue` reads its retry ceiling from a package-level var and would
  read better through config. Out of scope: queue is fenced, and the system
  works without it.
- The startup path has three near-identical error-wrapping blocks. Tidier
  merged; the system runs fine as is.

## Phase log

### P1 <slug> — merged, PR #12

Launched: <time> · request `mcp-...` · finished `ok`/`done` in 11m
Diff: 6 files, +240/-18. Check: base correct, no fence crossings, `go build`
and `go vet` clean on the integration branch after merge.
Left behind: `internal/config.Load(path) (*Config, error)`, parsing and
validating; nothing consumes it yet.
Deferred by the agent: none.
Notes: added tests only, changed none.

### P2 <slug> — merged, PR #14 (attempt 2)

Launched: <time> · request `mcp-...` · finished `ok`/`done` in 14m
Attempt 1 timed out with nothing pushed; brief narrowed and relaunched.
Diff: 4 files, +130/-40. Check escalated to the full suite — the diff changed
`poller_test.go`. `scripts/test.sh` passes; the changed assertion was the
interval source, which is legitimately what this phase moved.
Left behind: the poller reads its interval from `Load`, flags still work when no
file is present.
```

Keep the phase log entries short. Their job is to let a later phase's brief say
what is already there in terms of what the diff actually did, and to let the
final pull request body be assembled without re-reading six merged PRs.
