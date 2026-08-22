---
name: unity-harness-notes
description: Read this before running any `unity` command in this harness. Records what the Unity CLI can and cannot do from an agent session here — no Unity Editor is reachable, so `unity build`, `unity test`, `unity command` and `unity list` cannot work, whatever the unity-cli skill says — plus how the `unity` shim reaches its sibling container, why project paths pass through untranslated, and what to do when the CLI reports a missing image or an expired sign-in.
allowed-tools:
  - Bash
---

# Unity in this harness

The `unity` on your PATH is not the Unity CLI. It is a shell script that
forwards to a sibling container, because the real binary needs glibc 2.34+ and
this image is Alpine/musl. Everything after the name is the CLI's own, so
`unity releases --limit 2` works exactly as documented.

The `unity-cli` skill in this same pack is Unity's own, vendored unedited. It
documents the whole CLI. **This file is the part that is different here, and
it wins wherever the two disagree.**

## No Editor is reachable. None of these can work

    unity build      unity test       unity command
    unity list       unity open       unity run

Not "not configured yet" — unreachable, for two independent reasons:

1. **There is no Linux arm64 Unity Editor.** `unity releases --json` reports
   `"architecture": "x86_64"` for every Linux release, and asking for the
   other one is refused outright.
2. **A running Editor on the operator's machine is invisible to you.** The
   CLI finds Editors by scanning local processes and connects over a
   hardcoded `127.0.0.1`. This container has its own PID namespace and its
   own loopback.

So when a task needs an Editor, say so and stop. Do not install an Editor to
try — the download is 4.4 GB, it is the wrong architecture, and it will fail
after spending all of it. Do not fall back to `unity mcp`: without an Editor
that server returns an empty tool list.

Edit the project's files directly instead. A Unity project is a git checkout
like any other, and scenes, prefabs and `.asmdef` files are text you can read
and write with the ordinary tools. That is what the rest of this pack's
skills — `ui-uitk`, `shader-graph-create-custom-node`, `physics-3d-collision`
and the others — are for.

## What does work

`auth`, `releases`, `doctor`, `projects`, `templates`, `modules`, `config`,
`changelog`, and `mcp configure`. The `status` / `command` / `list` family
runs and reports cleanly with nothing connected, which is not the same as
working — read the output rather than assuming a zero exit meant success.

## Paths pass through untranslated

The workspace root is mounted at the same absolute path on the host and in
every container, so a `--project-path` you can `ls` is one the sibling can
open. Pass workspace paths as they are. Do not try to rewrite one into some
container-relative form; that is the thing that breaks it.

Paths *outside* the workspace root are not mounted and will not resolve.

## Two failures worth recognising

**`unity: the harness-unity:latest image is not on this host.`** The sibling
image was never built. You cannot fix this from inside a session — it is built
by `scripts/build.sh` on the host. Report it and move on.

**Sign-in state is shared and persistent.** It lives in a named volume, so
`unity auth login` in one session is visible to the next, and so is a logout.
Check `unity auth status` before assuming you are signed in, and do not log
out to "reset" anything — you would be signing out every other session too.
