# Plans

One document per piece of planned work. Give each feature a slug. A feature's plan is
`<slug>.md`, its shared facts are in [memory/<slug>.md](memory/), and its reports are in
`reports/<slug>/`. The `turret-feature-plan` skill writes the first two, and
`turret-feature-orchestrate` runs the plan.

A feature memory holds what every session working on the feature needs: the schema it
introduces, the invariants that hold across phases, and links to the files that matter with
a sentence on why. It carries no code and no progress, and it stays small enough to load
without crowding out the work. The session orchestrating a feature is the only one that
writes it; the sessions doing the phases suggest additions in their reports.

A plan says what will be built, in what order, and what proves each step done. Once the
work lands, the plan stays as written. What was actually built is described where this
repository describes its current state: [../../ARCHITECTURE.md](../../ARCHITECTURE.md),
[../../internal/CLAUDE.md](../../internal/CLAUDE.md) and the references under
[../](../). Why it came out differently goes in [../DESIGN.md](../DESIGN.md). A finding
measured against a live provider API goes in [../OBSERVED.md](../OBSERVED.md).

A phase's proof names a command. `scripts/build.sh` is the default gate; it runs gofmt,
vet, the suite and every release build, and ends by printing `bin/harness`. A phase that
touches `internal/stdiosession` also runs
`go test ./internal/stdiosession -run TestGoldenFrames`.

A plan whose phases have all landed moves to [archive/](archive/), and its memory to
`archive/memory/`. Its reports stay where they are. Everything else in this folder is work
that is planned, in flight, or not started.

# Agent reports

Agents write their reports into `reports/<slug>/`, one file per run, named
`<yyyy-mm-dd>-<what-it-did>.md`, when their work finishes. A report is a record of one run
and is not updated afterwards.

Say what was built, what was verified and how, what was left undone, bugs found and not
fixed, and anything the next run needs to know. Say what mechanical issues slowed the work:
flaky or slow tests, a slow or flaky build. Include the tail of the gate's output as the
proof. Findings that outlive the run belong in the documents named above. A problem the run
could not fix goes in `docs/issues/open/<short-name>.md`; create the folder if it is not
there yet.
