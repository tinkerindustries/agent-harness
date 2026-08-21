#!/bin/sh
# `unity` for agent sessions, forwarded to a sibling container.
#
# The Unity CLI is a C# Native AOT binary that needs glibc 2.34+, and this
# harness image is Alpine/musl, so `unity` cannot be a layer on it — the
# binary does not even fail legibly there, it fails with "no such file or
# directory" naming a file that exists, because what is missing is the glibc
# loader (Dockerfile.unity has the long version).
#
# What makes the sibling route work rather than merely possible is the
# workspace mount. docker-compose.yml binds the workspace root at the *same
# absolute path* on the host and in this container, and paths handed to
# `docker run -v` name the host's filesystem (ARCHITECTURE.md, "Gotchas";
# docs/WORKTREES.md, "Path parity"). So the same path string is valid in all
# three places — this container, the host daemon, and the sibling — and a
# project path can be passed straight through with no translation.
#
# Install as `unity` on PATH. Everything after the name is the CLI's own.
set -eu

IMAGE="${HARNESS_UNITY_IMAGE:-harness-unity:latest}"

# Without this check a missing image is not a missing image: `docker run`
# treats an unknown local name as something to pull, reaches Docker Hub,
# and fails with a repository-does-not-exist error naming a registry nobody
# meant to involve. Say the true thing instead, and name the command that
# fixes it — the image is built by scripts/build.sh, not by compose.
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
    echo "unity: the ${IMAGE} image is not on this host." >&2
    echo "unity: build it with scripts/build.sh (or: docker build -f Dockerfile.unity -t ${IMAGE} .)" >&2
    exit 127
fi

# The workspace root is the one directory guaranteed to exist at an identical
# path on both sides, so it is what gets mounted — not $PWD, which would work
# for a project directly under it but not for a `--project-path` pointing at a
# sibling session's tree. Compose sets this; the fallback keeps the shim
# usable if it is ever run somewhere that does not.
ROOT="${DEEPSEEK_WORKSPACE_ROOT:-/Users/Shared/harness-workspaces}"

# Sign-in state, the download cache and the update-check stamp all live under
# the CLI's config directory. With --rm and no volume every invocation would
# start signed out and re-download whatever it had already fetched, so
# `unity auth login` in one call would be invisible to the next. A named
# volume makes the CLI's state outlive the container the way it outlives a
# shell on a normal machine.
VOLUME="${HARNESS_UNITY_STATE_VOLUME:-harness-unity-state}"

# -i without -t: a session's Bash tool has no TTY, and asking for one fails
# outright. The image already sets UNITY_NON_INTERACTIVE, so the CLI does not
# go looking for a prompt it cannot get.
exec docker run --rm -i \
    -v "${ROOT}:${ROOT}" \
    -v "${VOLUME}:/root/.config/unityhub" \
    -w "$(pwd)" \
    -e UNITY_FORMAT \
    -e UNITY_PROJECT_PATH \
    -e UNITY_SERVICE_ACCOUNT_ID \
    -e UNITY_SERVICE_ACCOUNT_SECRET \
    "${IMAGE}" unity "$@"
