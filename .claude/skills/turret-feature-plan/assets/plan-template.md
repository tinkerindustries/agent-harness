# <Feature name>

- **Slug**: <kebab-case-slug>
- **Memory**: [memory/<slug>.md](memory/<slug>.md)

## What this builds

Two or three sentences on the finished feature, from the outside. What a person can do
once it lands that they cannot do now.

## Not building

The adjacent things this feature does not include, one line each, with a word on why. The
orchestrator holds each phase's pull request against this list.

-

## Phases

| Phase | Name | Depends on | Runs beside |
|---|---|---|---|
| 1 | | nothing | |
| 2 | | 1 | 3 |

### Phase 1 — <name>

**Depends on**: nothing

**What it builds**

The behaviour, and where it lives.

**How to approach it**

The direction you would take and why: what to build on, what pattern to follow, what to watch
out for. Not the files or the functions.

**Proof**

The command that passes or the observation on screen that closes this phase.

**Out of scope for this phase**

Where the neighbouring phase's work starts.

### Phase 2 — <name>

**Depends on**: 1

**What it builds**

**How to approach it**

**Proof**

**Out of scope for this phase**

## Open questions

Anything the plan could not settle, and who can settle it. Write "None" if there are none.
