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

FROM alpine:3.21
# ca-certificates for TLS to api.deepseek.com; the Bash tool runs commands
# through /bin/sh, and agent sessions expect git on the path.
RUN apk add --no-cache ca-certificates git
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
