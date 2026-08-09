# syntax=docker/dockerfile:1

# internal/webassets/dist is committed, but go:embed bakes in whatever sits
# there at compile time. Rebuilding it here keeps the image's UI matching
# web/src instead of whatever was last committed.
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
# ca-certificates for TLS to api.deepseek.com; the Bash tool runs commands
# through /bin/sh, and agent sessions expect git on the path.
RUN apk add --no-cache ca-certificates git

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

COPY --from=build /out/harness /usr/local/bin/harness
# The price table's default path is relative to the working directory, which
# holds workspaces here. Point at an absolute copy so the image runs the same
# way outside compose.
COPY configs/prices.json /etc/harness/prices.json
ENV DEEPSEEK_PRICE_TABLE=/etc/harness/prices.json \
    DEEPSEEK_DATA_DIR=/data
# Runs as root: agent sessions write into the mounted workspace, and a
# fixed uid would collide with the host's ownership on a bind mount.
WORKDIR /workspaces
ENTRYPOINT ["harness"]
CMD ["serve"]
