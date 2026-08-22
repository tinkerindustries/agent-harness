# The Blender skill pack

Opt-in, like every pack here: a session gets these only when its request named
`blender` ([`../unity/README.md`](../unity/README.md) explains the mechanism,
and [`../../agent-skills/README.md`](../../agent-skills/README.md) explains
what an always-on skill costs).

## This pack is ours, and that is not the same choice the Unity pack made

The Unity pack vendors Unity Technologies' own skills. There is no equivalent
here: the Blender Foundation publishes no agent-skills repository, and what
exists is third-party — `Dev-GOM/claude-code-marketplace`'s `blender-toolkit`,
`ra100/blender-skills`, and several marketplace listings — of varying
provenance and licence. None was vendored. Decide that deliberately before
adding one, rather than reaching for whichever is top of a search.

The second reason is that most of what such a pack would carry is already
arriving. The official Blender MCP server sends about five kilobytes of
instructions at initialize — the datablock model, active object versus
selection, the depsgraph update, the bmesh flush — and this harness puts them
in the opening message (`docs/MCP.md`, "Blender, the worked example"). It also
ships the Blender Python API reference and the user manual as plain-text RST
beside itself. A vendored skill repeating any of that would cost tokens to say
what the session has already been told, and would eventually contradict it.

So what is here is the remainder: what the MCP server cannot know, because it
is about this harness rather than about Blender.

## The one skill

`blender-harness-notes` — that the twenty-six tools reach two different
Blenders (fourteen over TCP to the operator's live session, twelve named
`*_for_cli` shelling out to `blender --background` in this container), that
the two image screenshot tools fail unless `size_limit_in_bytes` is passed,
which filesystem each half resolves a path on, and where the bundled docs are.

It is sourced from [`../../../docs/MCP.md`](../../../docs/MCP.md), which stays
the reference. When that section changes, change this with it — the doc is for
whoever maintains the harness, the skill is for a session in the middle of a
task, and they go stale independently.
