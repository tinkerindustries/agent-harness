# The Unity CLI

How an agent session reaches Unity's own command-line tool: it types `unity`,
and the call is forwarded to a sibling container built from
[`Dockerfile.unity`](../Dockerfile.unity).

This is Unity's official CLI — the standalone `unity` binary Unity announced
in July 2026 for managing Editors, modules, projects, licensing and
automation. It is not the community `unity-mcp-cli` package, and the two are
easy to confuse because both put "unity" and "cli" in the name.

## The constraint everything here follows from

**The Unity CLI cannot run on the harness image, and never will.** It is a C#
Native AOT build that requires glibc 2.34 or newer, and Unity's installer
refuses musl outright — *"Alpine and other musl distributions are not
supported"*. The harness image is `alpine:3.21`, musl 1.2.5.

What makes this worth writing down rather than discovering twice is how it
fails. Dropped onto the Alpine image the binary does not report a missing
library; it reports

```
exec /usr/local/bin/unity: no such file or directory
```

naming a file that is plainly there, because what is missing is the glibc
dynamic loader the binary asks for. Confirmed both directions with the same
binary: `1.0.0-beta.5` on `debian:bookworm-slim`, the error above on
`alpine:3.21`.

So the CLI lives in its own glibc image and the harness image carries a shim.

## The shape of it

```
session's Bash tool
  │  unity releases --limit 2
  ▼
/usr/local/bin/unity            ← scripts/unity-shim.sh, baked into the image
  │  docker run --rm -i  (the host socket compose already mounts)
  ▼
harness-unity:latest            ← Dockerfile.unity, debian:bookworm-slim
  │
  ▼
unity 1.0.0-beta.5
```

**Path parity is what makes it work rather than merely run.**
`docker-compose.yml` binds the workspace root at the *same absolute path* on
the host and inside the container, and paths handed to `docker run -v` name
the host's filesystem (ARCHITECTURE.md, "Gotchas"; docs/WORKTREES.md, "Path
parity"). One path string is therefore valid in all three places — the
harness container, the host daemon, and the sibling — so a `--project-path`
passes straight through with no translation. The shim mounts the workspace
root rather than `$PWD`, so a path into another session's tree resolves too.

**CLI state outlives the container.** Sign-in state, the download cache and
the update-check stamp live under the CLI's config directory, and with `--rm`
and no volume every call would start signed out. A named volume
(`harness-unity-state`) is mounted at `/root/.config/unityhub`, so
`unity auth login` in one call is visible to the next.

**The image is built by `scripts/build.sh`, not by compose.** Nothing runs it
as a service; it is a host-level image reached per invocation. The dev stack
and `deepseek-harness-prod` share one host daemon, so one build serves both.
A missing image is caught by the shim with a message naming the fix — without
that check `docker run` treats the unknown name as something to pull and
fails against Docker Hub, naming a registry nobody meant to involve.

## What works, and what does not

Everything that does not need a Unity Editor works today: `auth`, `releases`,
`doctor`, `projects`, `templates`, `mcp configure`, and the
`status` / `command` / `list` family, which report cleanly with nothing
connected.

**No Editor is available to a session, and there are two independent reasons.**

The first is architecture. There is no Linux arm64 Editor at all —
`unity releases --json` reports `"architecture": "x86_64"` for every Linux
release, and asking for the other one is refused:

```
$ unity install 6000.0.82f1 --architecture arm64 --dry-run
Error: No editor version matched
```

The x86_64 dry run resolves normally, at a 4.4 GB download. So on an Apple
Silicon host, Editor-backed work — `unity build`, `unity test` — needs
`--platform linux/amd64` and therefore emulation, plus licensing. That is a
deliberate non-goal here rather than an oversight.

The second is that the operator's own Editor is unreachable from a container
even when it is running. The CLI finds Editors by scanning local processes
(`ScanRunningUnityEditorsAsync`, `DiscoverEditorInstancesAsync`) and connects
over a hardcoded `127.0.0.1` — the only host string in the binary — with no
flag to point it elsewhere. A container has its own PID namespace and its own
loopback, so a native macOS Editor is invisible to it. The CLI is designed to
sit on the same machine as the Editor.

An operator who wants to drive their own Editor should install the CLI on the
host (`brew install --cask unity-cli`) and use it there.

## Unity's MCP server is deliberately not wired up

`unity mcp` is an MCP server, and it is the same binary: `unity mcp configure`
writes `{"command": "unity", "args": ["mcp", "--project-path", …]}`. It would
slot into `internal/mcpclient` the way Blender's server does
([docs/MCP.md](MCP.md)).

It is not registered, and the reason is the second constraint above: an
Editor-less `unity mcp` answers `initialize` correctly and then returns
`{"tools":[]}`, because the tool list comes from commands the Pipeline package
registers inside a running Editor. Reaching a real Editor would mean running
the server on the host and bridging it in over HTTP — a host-side process
beside the Blender add-on's, not a row that stands on its own. Registering it
before that exists would put a permanently empty server in every session's
tool array.

## Versions

`Dockerfile.unity` pins `UNITY_CLI_VERSION` and a SHA-256 per architecture,
both from the CDN's `latest-beta.json`. The beta channel is not a preference:
it is the only one that exists, and the stable manifest (`latest.json`) 404s.
Bump the version and both sums together.
