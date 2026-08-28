# syntax=docker/dockerfile:1

# internal/webassets/dist holds only .gitkeep in git. go:embed bakes in
# whatever sits there at compile time, so the image builds the frontend here
# and copies it in before the Go build.
FROM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd/ ./cmd/
COPY internal/ ./internal/
# assets/ is a Go package, not just files: it embeds assets/agent-skills into
# the binary (assets/embed.go), so the build fails without it rather than
# producing a binary that ships no skills. A host `go build` sees the whole
# working tree and cannot catch a missing COPY here — only the container
# build can, which is the reason scripts/build.sh runs it.
COPY assets/ ./assets/
COPY --from=web /src/internal/webassets/dist ./internal/webassets/dist
# modernc.org/sqlite is pure Go, so the binary needs no libc at runtime.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -o /out/harness ./cmd/harness

# Toolchains for the runtime stage below, pulled from their official Alpine
# images rather than apk: 3.21's own packages are pinned to Node 22, Go 1.23,
# and Python 3.12, all behind the versions agent sessions get here. These
# stages are never built from, only copied out of.
FROM node:24-alpine AS node-toolchain
FROM golang:1.26-alpine AS go-toolchain
FROM python:3.14-alpine AS python-toolchain

FROM alpine:3.21
# ca-certificates for TLS to api.deepseek.com, and agent sessions expect git
# on the path.
RUN apk add --no-cache ca-certificates git

# Sessions reach for GNU flags that busybox's applets reject, and a rejection
# swallowed by the 2>/dev/null they tend to add reads as a genuine no-match
# (docs/reviews/sess-bb6c0ed564ddae573c3b1832cb3981f4.md). grep and procps
# replace the two applets that get hit, curl is absent from busybox entirely,
# and ripgrep stays for the searches that suit it. bash is what the Bash tool
# runs commands through when it is present (internal/tools/bash.go); without
# it the shell is busybox ash, which rejects the bash syntax models write.
RUN apk add --no-cache bash grep curl procps ripgrep

# A C toolchain for the sessions' own builds: cgo, node-gyp, and Python
# packages that ship no musl wheel all compile from source. linux-headers
# and pkgconf are common requirements of those same builds.
RUN apk add --no-cache build-base linux-headers pkgconf

# Node's binary is dynamically linked against libstdc++; everything else it
# needs lives under /usr/local/bin and /usr/local/lib already.
COPY --from=node-toolchain /usr/local/bin/node /usr/local/bin/node
COPY --from=node-toolchain /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN apk add --no-cache libstdc++ && \
    ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm && \
    ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx

# pnpm and yarn, because the workspace installs whatever manager a cloned
# repository's lockfile names (internal/workspace/deps.go) and a pnpm repo has
# nothing to run without them. Baked rather than fetched through corepack at
# run time, so a session does not wait on a download or fail without a network.
RUN npm install -g --no-fund --no-audit pnpm yarn

# The go and gofmt binaries are statically linked, so nothing else is needed.
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}"

# Python's interpreter dynamically links against openssl, sqlite, and a
# handful of other libraries; installing those from 3.21's own apk repo
# keeps them on the musl ABI this image already uses.
COPY --from=python-toolchain /usr/local/bin/python3.14 /usr/local/bin/python3.14
COPY --from=python-toolchain /usr/local/lib/libpython3.14.so.1.0 /usr/local/lib/libpython3.14.so.1.0
COPY --from=python-toolchain /usr/local/lib/python3.14 /usr/local/lib/python3.14
COPY --from=python-toolchain /usr/local/bin/pip3.14 /usr/local/bin/pip3.14
# Python.h and the rest of the headers, which building an extension module
# from source needs and the interpreter itself does not.
COPY --from=python-toolchain /usr/local/include/python3.14 /usr/local/include/python3.14
RUN apk add --no-cache gdbm libbz2 libffi libncursesw ncurses-terminfo-base \
        readline libssl3 sqlite-libs xz-libs zlib && \
    ln -s python3.14 /usr/local/bin/python3 && \
    ln -s python3 /usr/local/bin/python && \
    ln -s pip3.14 /usr/local/bin/pip3 && \
    ln -s pip3 /usr/local/bin/pip

# gh ships a statically linked Go binary, so the release tarball drops in
# directly instead of going through a build stage. TARGETARCH matches gh's
# own asset naming for amd64 and arm64.
ARG TARGETARCH
ARG GH_VERSION=2.97.0
RUN wget -qO- https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_${TARGETARCH}.tar.gz \
    | tar xz -C /usr/local/bin --strip-components=2 gh_${GH_VERSION}_linux_${TARGETARCH}/bin/gh

# uv, so a stdio MCP server registered as `uvx <package>` has something to
# launch (docs/MCP.md). Astral ship a static musl build, so the release
# tarball drops straight in the way gh's does above; uvx is a second binary
# in the same archive, not a shim that needs a Python on the path. Node-based
# servers are already covered by the npx installed above. uv's own arch names
# are not Docker's, hence the case: TARGETARCH is amd64/arm64 and uv ships
# x86_64/aarch64.
ARG UV_VERSION=0.12.5
RUN case "${TARGETARCH}" in \
      amd64) uv_arch=x86_64 ;; \
      arm64) uv_arch=aarch64 ;; \
      *) echo "unsupported TARGETARCH ${TARGETARCH}" >&2; exit 1 ;; \
    esac && \
    wget -qO- https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${uv_arch}-unknown-linux-musl.tar.gz \
    | tar xz -C /usr/local/bin --strip-components=1 \
        uv-${uv_arch}-unknown-linux-musl/uv uv-${uv_arch}-unknown-linux-musl/uvx

# Playwright drives Alpine's own Chromium. The browsers `playwright install`
# downloads are glibc-only and will not start on musl, so the symlinks below
# put the system Chromium where Playwright looks for its downloaded one —
# both the headed path and the headless shell it uses by default. That makes
# `playwright screenshot` and chromium.launch() work with no flags. The
# /opt/google/chrome path covers channel: "chrome" as well. Firefox and
# WebKit have no musl build and are not available here.
#
# @playwright/cli ships its own copy of Playwright (a newer snapshot than the
# stable one installed above), which resolves to its own browser build number
# and therefore its own cache paths. The second loop below derives the
# executable path from the playwright-core package that @playwright/cli
# actually resolves — never a hardcoded build number — and symlinks that set
# too, so the documented `playwright-cli` commands can launch the browser.
ARG PLAYWRIGHT_VERSION=1.62.1
ARG PLAYWRIGHT_CLI_VERSION=0.1.18
# NODE_PATH lets a script anywhere require("playwright") from the global
# install. A project's own node_modules still wins over it.
ENV PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 \
    NODE_PATH=/usr/local/lib/node_modules
RUN apk add --no-cache chromium nss freetype harfbuzz ttf-freefont font-noto-emoji ffmpeg && \
    npm install -g playwright@${PLAYWRIGHT_VERSION} && \
    npm install -g @playwright/cli@${PLAYWRIGHT_CLI_VERSION} && \
    chrome="$(node -e 'console.log(require("/usr/local/lib/node_modules/playwright").chromium.executablePath())')" && \
    headless="$(echo "$chrome" | sed 's#/chromium-#/chromium_headless_shell-#; s#/chrome$#/headless_shell#')" && \
    for p in "$chrome" "$headless"; do \
        mkdir -p "$(dirname "$p")" && \
        ln -sf /usr/bin/chromium "$p" && \
        touch "$(dirname "$(dirname "$p")")/INSTALLATION_COMPLETE"; \
    done && \
    cli_core="$(dirname "$(node -e 'console.log(require.resolve("playwright-core/package.json", { paths: ["/usr/local/lib/node_modules/@playwright/cli"] }))')")" && \
    cli_chrome="$(node -e 'console.log(require(process.argv[1]).chromium.executablePath())' "$cli_core")" && \
    cli_headless="$(echo "$cli_chrome" | sed 's#/chromium-#/chromium_headless_shell-#; s#/chrome$#/headless_shell#')" && \
    for p in "$cli_chrome" "$cli_headless"; do \
        mkdir -p "$(dirname "$p")" && \
        ln -sf /usr/bin/chromium "$p" && \
        touch "$(dirname "$(dirname "$p")")/INSTALLATION_COMPLETE"; \
    done && \
    ffmpeg_ver="$(node -e 'const path=require("path"); const coreDir=path.dirname(require.resolve("playwright-core/package.json", { paths: ["/usr/local/lib/node_modules/@playwright/cli"] })); console.log(require(path.join(coreDir, "browsers.json")).browsers.find(b => b.name === "ffmpeg").revision)')" && \
    ffmpeg_dir="/root/.cache/ms-playwright/ffmpeg-${ffmpeg_ver}" && \
    mkdir -p "$ffmpeg_dir" && \
    ln -sf /usr/bin/ffmpeg "$ffmpeg_dir/ffmpeg-linux" && \
    touch "$ffmpeg_dir/INSTALLATION_COMPLETE" && \
    mkdir -p /opt/google/chrome && ln -sf /usr/bin/chromium /opt/google/chrome/chrome

# Agent sessions run as root, and Chromium refuses to start as root while its
# sandbox is enabled. The raw `playwright` CLI adds --no-sandbox implicitly;
# the CLI's own launcher only does when the config asks for it, and its
# default channel config actually forces the sandbox on. A global config file
# is the tool's documented surface for this — a workspace's own
# .playwright/cli.config.json still takes precedence over it.
RUN mkdir -p /root/.playwright && \
    printf '%s\n' '{"browser":{"launchOptions":{"chromiumSandbox":false}}}' > /root/.playwright/cli.config.json

# The client for the docker socket docker-compose.yml mounts in. Paths given
# to `docker run -v` name the host's filesystem, not this container's.
RUN apk add --no-cache docker-cli docker-cli-compose docker-cli-buildx

# `unity`, forwarded to a sibling container by the shim (scripts/unity-shim.sh
# carries the reasoning; docs/UNITY.md is the reference).
#
# It is a shim rather than a binary because it cannot be a binary here: the
# Unity CLI is a C# Native AOT build requiring glibc 2.34+, and Unity's own
# installer refuses musl by name. Dropped onto this image it does not fail
# legibly — it fails with `no such file or directory` naming a file that is
# plainly there, because what is missing is the glibc loader. So the CLI lives
# in its own glibc image (Dockerfile.unity) and this forwards to it across the
# docker socket already mounted above.
#
# Deliberately placed after the docker-cli layer: the shim is inert without
# it, and a session calling `unity` on an image that had lost that layer
# should fail on the missing `docker` rather than somewhere stranger.
COPY scripts/unity-shim.sh /usr/local/bin/unity
RUN chmod +x /usr/local/bin/unity

# Blender, for the `*_for_cli` half of the official Blender MCP server's tool
# array (docs/MCP.md, "Blender"). Those tools do not talk to the operator's
# running Blender over the add-on's socket the way the rest do — they shell
# out to `blender --background` from the MCP server subprocess, and that
# subprocess runs *here*, inside this container. Without a binary on this
# PATH exactly half of that server's tools fail on every call, and the
# failure names Python, not the missing Blender.
#
# From edge, not the base's own release, and pinned exactly. Alpine 3.21
# carries Blender 4.3.0 and 3.22 carries 4.4.3, while the add-on side of the
# same MCP server is whatever Blender the operator is running — 5.2 LTS here.
# A .blend written by a 5.x Blender is not a file a 4.x one reads back
# faithfully, so a mismatched pair gives the CLI tools a subtly different
# scene than the interactive tools see, which is worse than not having them.
# edge/community carries the current 5.2.x patch, and edge/main is where its
# dependencies live.
#
# The exact `=` pin is the point of using edge at all: edge moves, and an
# unpinned `blender` would silently bake whichever version it had drifted to
# on the day of a rebuild. Pinned, a moved edge fails this build loudly and
# an operator bumps the ARG to match the Blender they actually run.
#
# What "match" means in practice is the LTS series, not the patch. edge keeps
# one 5.2.x at a time and drops the older one, so an exact patch match is
# available only until the next one lands — 5.2.0 was gone from edge while
# the host still ran it. A patch bump inside 5.2 keeps the .blend format
# identical, which is the thing this pin protects. A minor or major move is a
# different question and should be made against the Blender on the host.
#
# What it drags in: ~98 packages, about two dozen of them upgrades of imaging
# and codec libraries — OpenEXR, x265, libvpx, libjxl and the ffmpeg
# libraries. musl, chromium, node, python and the go toolchain are not
# touched, and the ffmpeg binary the playwright layer above symlinks for
# video capture stays the base's own 6.1.2 and still encodes.
#
# `--upgrade` and the explicit spirv-tools are not decoration, and dropping
# either reproduces a failure that looks nothing like its cause. edge's
# glslang-libs needs edge's SPIRV-Tools, but SPIRV-Tools ships *unversioned*
# sonames (libSPIRV-Tools.so, not .so.N), so apk reads the base's 3.21 copy
# as already satisfying the dependency and leaves it in place. The image then
# has a new glslang against an old SPIRV-Tools, and everything linking
# glslang — Blender and ffmpeg both — dies at load with `Error relocating
# /usr/lib/libglslang.so.16: symbol not found`. Naming the package and
# forcing the upgrade keeps the pair in lockstep.
ARG BLENDER_VERSION=5.2.1-r0
RUN apk add --no-cache --upgrade \
        --repository https://dl-cdn.alpinelinux.org/alpine/edge/community \
        --repository https://dl-cdn.alpinelinux.org/alpine/edge/main \
        "blender=${BLENDER_VERSION}" spirv-tools

# tini reaps orphans as PID 1. A session's Bash calls run under a shell in
# its own process group, and any of that shell's children still alive when it
# exits are reparented to PID 1. `harness serve` is a Go program and reaps
# only what it started itself, so without a reaper those orphans stay zombies
# and hold their PID slots for the life of the container (TESTING.md, "A
# container that cannot fork").
RUN apk add --no-cache tini

COPY --from=build /out/harness /usr/local/bin/harness
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
# The price table's default path is relative to the working directory. Point
# at an absolute copy so the image runs the same way outside compose, and so
# it does not depend on the working directory below.
COPY configs/prices.json /etc/harness/prices.json
ENV DEEPSEEK_PRICE_TABLE=/etc/harness/prices.json \
    DEEPSEEK_DATA_DIR=/data
# Runs as root: agent sessions write into the mounted workspace, and a
# fixed uid would collide with the host's ownership on a bind mount.
# Not the workspace root: compose mounts that at whatever absolute path it
# has on the host, so the image cannot know it (docker-compose.yml, "path
# parity"). Sessions are given their workspace directory explicitly.
WORKDIR /
ENTRYPOINT ["/sbin/tini", "--", "docker-entrypoint.sh"]
CMD ["serve"]
