# Generating candidate non-goals

Non-goals cannot be recalled, only recognised. This is the generator: work each
category against each phase of the plan, write down what a capable agent
standing in that code would plausibly build beyond its brief, then put the
survivors to the user as yes-or-no.

The test for whether a candidate is worth asking about is not "is it a good
idea" — most of them are, which is exactly the problem. It is **would a
competent engineer, mid-phase, with no one to ask, have a defensible argument
for doing this?** If yes, it needs an answer in the document. If nobody would
ever have done it, it is noise and it makes the non-goals section look padded.

## The categories

**Generalisation.** The plan says one case; the code makes it nearly free to
handle all of them. One provider becomes a provider interface, one file format
becomes a registry, one retry policy becomes a strategy pattern. This is the
most common single source of unplanned work, because it genuinely looks like
good engineering from inside the phase.

**The adjacent members of the set.** The plan names three settings, three
endpoints, three fields — and there are eleven of them sitting in the same
struct. An agent moving three and leaving eight looks, from where it stands,
like it left the job half done.

**The obvious next feature.** Config file → hot reload. Auth → sessions. A
cache → invalidation. An event log → replay. Whatever the second thing anyone
asks for after the first thing exists, someone will build it while in there.

**The tidy-up.** Refactoring, deduplication, renaming for consistency,
extracting the helper, deleting the code that looks dead. Especially dangerous
in late phases, where the branch has accumulated several agents' worth of code
and cleaning it up feels like the responsible finish.

**The second consumer.** The plan says the server; there is also a CLI, a
worker, a test harness, an MCP surface, a second binary. Which of them get the
change, and which stay as they are, is almost never stated and almost always
assumed differently by everyone.

**Compatibility and migration.** Deprecation shims, a migration path for
existing data, environment variable overrides alongside the new mechanism,
keeping the old flag working, a version negotiation. Each is a defensible
addition and each can double a phase.

**Tests and CI.** Adding a CI job, an integration harness, a fixture generator,
coverage for code adjacent to the change, converting existing tests to a
different style. Note the specific one worth calling out separately: **changing
existing tests rather than adding to them** is how a failing suite gets made to
pass, and it deserves its own line in the document.

**Docs, logging, metrics.** README changes beyond the feature, adding metrics
because the new path is uninstrumented, structured logging because the log lines
nearby are inconsistent, architecture docs updated to match. Cheap individually,
and they blur what the diff is about.

**Performance.** An agent that notices an N+1, a linear scan, or a missing index
while implementing something else. Almost always out of scope and almost always
tempting, because it comes with a plausible benchmark.

**Error handling and validation beyond the brief.** Hardening adjacent paths,
adding validation to inputs the change did not introduce, wrapping errors that
were already unwrapped. Sensible, unbounded, and not what was asked for.

**Dependency changes.** Adding a library that makes the phase easier, upgrading
one that is out of date, replacing a hand-rolled helper with a package. Worth an
explicit line in almost every scope document, because the cost lands on
everybody afterwards rather than on the run.

**Infrastructure and configuration.** Dockerfile edits, compose changes, new
ports, new volumes, CI runner changes. In a repository whose agents hold a real
docker socket, this is worth being specific about.

## Working a phase

Take one phase, hold the categories against it, and write candidates as
concrete sentences. From a phase reading "parse config.yaml into a Config
struct":

- Generalisation: support TOML and JSON as well, behind a format sniffer.
- Adjacent set: move all eleven settings, not the three named.
- Next feature: watch the file and reload on change.
- Tidy-up: replace the flag parsing in `main.go` while restructuring startup.
- Second consumer: have the MCP server read the same file.
- Compatibility: accept env var overrides for each setting.
- Tests: add a CI job that validates the shipped example config.
- Docs: document every setting in the README.
- Dependencies: pull in a schema-validation library rather than hand-rolling.

Nine candidates from one phase, every one defensible, and each one a run's worth
of work that nobody scheduled. Six of them will be non-goals, two will already
be answered by the plan, and one will turn out to be something the user actually
wanted and had not said — which is the other reason this pass earns its time.
