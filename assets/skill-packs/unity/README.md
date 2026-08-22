# The Unity skill pack

Unity Technologies' own Agent Skills, vendored. A session gets them only when
its request asks for the `unity` pack by name — nothing here reaches a run
that did not opt in, which is the whole reason this directory sits under
`skill-packs/` rather than `agent-skills/`
([`../../agent-skills/README.md`](../../agent-skills/README.md) explains what
the always-on directory costs, and why almost nothing belongs in it).

## What it costs when it is on

Measured, not estimated: the seventeen descriptions render to a **7,416-byte
catalogue block, roughly 1,850 tokens, and it rides in every request of the
run** — not once, every sub-turn. A forty-sub-turn run carrying this pack for
no reason spends real money restating Shader Graph and UI Toolkit to a model
working on something else. That is the whole argument for the default being
off, and the number to quote if anyone proposes making a pack always-on.

## Provenance

Mirrored from <https://github.com/Unity-Technologies/skills>, commit
`87fac23d66a1f44f5e06c2935eccce0b40b9715a` (2026-08-21). Upstream installs
these with `npx skills add Unity-Technologies/skills`; that writes them into a
developer's own skills directory, which is the wrong shape for a harness that
must decide per run whether a session sees them at all, so they are copied in
instead.

Refresh by re-cloning at a new commit and copying the kept subset below over
this directory, preserving `LICENSE.md`. Do not hand-edit a `SKILL.md`: the
next refresh overwrites it, and a local fix would be silently lost. Something
that must be said differently for this harness belongs in a skill of our own.

## Licence

`LICENSE.md` is upstream's, kept verbatim: the **Unity Companion License**,
which grants use "for Unity-dependent projects". It is not a permissive
licence, and it is the reason the file travels with the skills rather than
being summarised here. Keep it in place when refreshing.

## What was kept, and what was not

Upstream ships 22 skills. Sixteen are here, plus one of ours. The six that
are not:

| Dropped | Why |
| --- | --- |
| `build-live-game` | Needs Unity Cloud Build and a live Unity Services project |
| `implement-in-app-purchases` | Needs a configured store and a Unity Services project |
| `levelplay-unity-integration` | Needs a LevelPlay mediation account |
| `setup-multiplayer-services` | Needs Unity Gaming Services provisioning |
| `setup-vivox-voice-chat` | Needs a Vivox account |
| `physics-3d-collision` | **Its frontmatter is not valid YAML** — see below |

The first five each drive a Unity Cloud service a session has no credentials
for, so the skill would be advertised and then dead-end. They are also the
four largest directories upstream, so dropping them halves the pack.

`physics-3d-collision` is a different case and worth knowing about, because
nothing would have reported it. Its `description` is a plain unquoted scalar
containing `": "` — `... MonoBehaviour-based Unity projects. Primary scope:
OnCollisionEnter ...` — which YAML reads as a nested mapping and rejects.
`internal/skills` parses frontmatter with a real YAML parser and drops a skill
it cannot read, so the skill installed fine, occupied 33 KB of every opted-in
workspace, and was never mentioned to a single session. `assets`'s
`TestEveryPackedSkillReachesTheCatalogue` now fails the build on that, so a
refresh will say so rather than repeat it. If upstream quotes the string,
bring the directory back.

What is kept is the work a session can actually do against a checkout: the
`unity-cli` skill, the UI family (`ui`, `ui-uitk`, `ui-ugui`, `ui-imgui`),
rendering (`shader-graph-create-custom-node`, `urp-postprocessing`,
`validate-urp-render-graph-renderer-feature`), and the rest of the
project-level set. `unity-harness-notes` is ours, not upstream's — see below.

## The Editor caveat

`unity-cli` documents the whole CLI, including `unity build`, `unity test`,
`unity command` and `unity list` — every one of which needs a running Unity
Editor. **No session can reach one**, for two independent reasons that
[`../../../docs/UNITY.md`](../../../docs/UNITY.md) sets out: there is no Linux
arm64 Editor at all, and the CLI discovers Editors by scanning local processes
over a hardcoded `127.0.0.1`, which a container's own PID namespace and
loopback make invisible.

The skill is vendored unedited anyway, per the no-hand-edits rule above. The
correction is delivered instead by `unity-harness-notes`, a skill of ours in
this same pack, which the catalogue lists alongside it.
