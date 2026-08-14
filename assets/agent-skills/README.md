# Skills our own sessions get

Skills for the agent sessions this harness runs. Everything in a subdirectory
here is embedded into the binary (`assets/embed.go`) and written into every
prepared workspace's `skills/` directory at session setup
(`internal/skills.Install`), where discovery lists it in the session's skill
catalogue beside anything a cloned repository ships in its own
`.claude/skills`.

This is the opposite audience to [`../skills`](../skills/README.md), which
holds Claude Code skills for driving this harness from the outside and is
never loaded by a run.

## Layout

    agent-skills/<name>/SKILL.md          the skill, with YAML frontmatter
    agent-skills/<name>/references/...    anything else it needs

`<name>` becomes the directory name in the workspace, so it must be a plain
directory name. A subdirectory without a `SKILL.md` is not a skill and is not
copied; this README is not either.

## Nothing is here yet, deliberately

The first candidate was a `vision-tools` skill explaining how the four vision
tools fit together. It went into the system prompt instead, and the reasoning
generalises: **guidance that changes the first call of its kind belongs in the
head, not here.**

Two reasons, and the cost one is the opposite of what it looks like. A skill
only helps if the model reads it, and the guidance that stops a bad first
vision call is needed before the model would think to go looking. And the head
is byte-identical across sessions, so it hits the provider's cross-session
prompt cache (docs/CACHE.md), while this directory's descriptions ride in the
opening user message — which carries the workspace path and the task, is
unique per session, and is therefore paid at the cache-MISS rate every single
run. The system prompt is the cheap place for always-true guidance; a skill
catalogue entry costs more and buys only a pointer.

What is left for this directory is what the head cannot carry: long-form
procedure, worth its Read only when a session is actually doing that kind of
work, and irrelevant enough to most runs that putting it in the head would tax
every one of them.

## What to put here, and what it costs

Every skill's one-line `description` rides in the opening message of **every
session on every provider**, whether or not the run has anything to do with
it (`internal/skills`, `MaxDescriptionLen`). A skill that is only relevant to
one repository belongs in that repository's `.claude/skills`, where only runs
that clone it pay for it. What belongs here is what any session might need:
conventions for the workspace itself, or a capability the harness gives every
run.

The catalogue is capped at 50 skills and each description at 500 characters,
counting both sources together — so a repository's own skills are what get
dropped when this directory grows.
