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

COPY --from=build /out/harness /usr/local/bin/harness
COPY scripts/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
# The price table's default path is relative to the working directory, which
# holds workspaces here. Point at an absolute copy so the image runs the same
# way outside compose.
COPY configs/prices.json /etc/harness/prices.json
ENV DEEPSEEK_PRICE_TABLE=/etc/harness/prices.json \
    DEEPSEEK_DATA_DIR=/data
# Runs as root: agent sessions write into the mounted workspace, and a
# fixed uid would collide with the host's ownership on a bind mount.
WORKDIR /workspaces
ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["serve"]
