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
