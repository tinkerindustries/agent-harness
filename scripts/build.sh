#!/bin/sh
# The build, as one command, failing at the first thing that is wrong.
#
# It exists because the pieces could each look fine while the result was
# stale. `npm run build` is `tsc -b && vite build`: when the typecheck failed,
# vite never ran, the previous bundle stayed on disk, the image baked it, and
# the container served a version of the app that no longer matched the source.
# Nothing in that chain reported an error.
#
# So the last stage checks the thing that actually matters — that the running
# container is serving the bundle this build just produced — rather than that
# each step exited zero.
#
# Usage:
#   scripts/build.sh              everything, including the dev container
#   scripts/build.sh --no-docker  through the Go binary only
#
# The Go suite is scripts/test.sh; this runs the frontend tests because they
# take under a second and one of them is the only guard against a silently
# dropped CSS rule.
set -eu

cd "$(dirname "$0")/.."

# The compose files mount the workspace root at the same absolute path inside
# the container as it has here, so that a session which runs docker against
# the shared host socket names paths the host can resolve (docker-compose.yml,
# docs/WORKTREES.md "Path parity"). Their default for it is ${PWD}, which is
# the shell's directory rather than the project's; export it from the root
# this script just moved to, so the value never depends on where the script
# was invoked from.
# .env is where compose reads it from, so honour it here too: an exported
# variable wins over .env in compose's interpolation, and without this line a
# root pinned there would be silently replaced by $PWD/workspaces on every
# build — the one path this script is supposed to keep stable.
if [ -z "${HARNESS_WORKSPACES:-}" ] && [ -f .env ]; then
  HARNESS_WORKSPACES="$(sed -n 's/^HARNESS_WORKSPACES=//p' .env | tail -1)"
fi
export HARNESS_WORKSPACES="${HARNESS_WORKSPACES:-$PWD/workspaces}"

DOCKER=1
[ "${1:-}" = "--no-docker" ] && DOCKER=0

stage() { printf '\n=== %s\n' "$1"; }
die() { printf '\nbuild failed: %s\n' "$1" >&2; exit 1; }

stage "gofmt"
unformatted="$(gofmt -l cmd internal)"
[ -z "$unformatted" ] || die "not gofmt-clean:$(printf '\n%s' "$unformatted")"
echo "clean"

stage "go vet"
go vet ./cmd/... ./internal/...
echo "clean"

stage "frontend dependencies"

# `npm run build` runs tsc and vite out of web/node_modules, and a fresh
# clone has neither. The failure that causes is a poor one to read: the build
# script is `npm run clean && tsc -b && vite build`, so clean-dist.mjs empties
# the output directory first and the typecheck then dies with `sh: tsc:
# command not found` — a missing-tool error where the missing piece is the
# whole dependency tree, and an emptied dist left behind either way.
#
# `npm ci` rather than `npm install`: it installs exactly what the lockfile
# says and never writes one back, which is what a build wants. It also
# deletes node_modules first, so it repairs a half-installed tree rather than
# layering onto it.
#
# Only when the tree is missing or stale, because ci is a full reinstall and
# this script runs on every build. `npm ci` writes
# node_modules/.package-lock.json as a record of what it installed, so that
# file existing and being no older than the lockfile is the check for whether
# the install still matches what the lockfile asks for.
if [ ! -f web/node_modules/.package-lock.json ] ||
	[ web/package-lock.json -nt web/node_modules/.package-lock.json ]; then
	npm --prefix web ci
else
	echo "up to date with package-lock.json"
fi

stage "frontend build (clean, typecheck, bundle)"
npm --prefix web run build

# npm run build empties the output first, so a typecheck failure leaves no
# assets rather than the previous ones. Prove they came back.
assets="internal/webassets/dist/assets"
[ -d "$assets" ] || die "$assets does not exist: the bundle did not build"
css="$(ls "$assets" | grep '^index-.*\.css$' | head -1)" || true
[ -n "${css:-}" ] || die "no index-*.css in $assets: the bundle did not build"
echo "bundled $css"

stage "frontend tests"
npm --prefix web run test

stage "go build"
go build -o bin/harness ./cmd/harness
echo "bin/harness"

if [ "$DOCKER" -eq 0 ]; then
	printf '\nok (skipped the container)\n'
	exit 0
fi

stage "unity image"

# The image `unity` forwards to. It is built here, before the harness
# container, because the shim baked into that container is useless without it
# and a session's first `unity` call is a poor place to discover that.
#
# A separate image rather than a layer: the Unity CLI needs glibc 2.34+ and
# refuses musl, so it cannot live on the Alpine harness image at all
# (Dockerfile.unity has the full reasoning, docs/UNITY.md the reference).
#
# Not part of the compose project deliberately — nothing runs it as a service.
# It is a host-level image that `docker run --rm` reaches per invocation, and
# both the dev stack and deepseek-harness-prod share one host daemon, so a
# single build here serves both.
docker build -f Dockerfile.unity -t "${HARNESS_UNITY_IMAGE:-harness-unity:latest}" .

# Prove the binary loads rather than that the build exited zero. This is the
# check that would have caught the musl problem on day one: a `unity` whose
# loader is missing still builds a perfectly good image.
docker run --rm "${HARNESS_UNITY_IMAGE:-harness-unity:latest}" unity --version >/dev/null \
	|| die "the unity image built but \`unity --version\` does not run in it"
echo "harness-unity ok"

stage "container"

# Inside a container, `docker` talks to a daemon that does not share this
# filesystem — the host's, through the mounted socket — so a bind mount only
# resolves where the path is identical on both sides. Prove that before
# compose tries it: compose's own failure names a path and a File Sharing
# preference pane, and neither points at the reason (docs/WORKTREES.md, "Path
# parity"). A session in the harness is the only caller that reaches this.
if [ -f /.dockerenv ]; then
	if docker image inspect alpine:3 >/dev/null 2>&1 || docker pull -q alpine:3 >/dev/null 2>&1; then
		if ! probe="$(docker run --rm -v "$PWD:$PWD" -w "$PWD" alpine:3 test -f go.mod 2>&1)"; then
			die "the docker daemon cannot see this checkout at $PWD${probe:+ ($probe)}.
The container stage runs compose against a daemon that resolves bind mounts on
the host, so it can only run where this path is the host's path too. See
docs/WORKTREES.md, \"Path parity\"."
		fi
	else
		printf 'warning: could not run the mount preflight (no alpine:3); letting compose speak for itself\n' >&2
	fi
fi

docker compose up -d --build

port="$(docker compose port harness 8080 2>/dev/null | sed 's/.*://')"
[ -n "$port" ] || die "the harness service published no port"
base="http://127.0.0.1:$port"

# A published port is published on the *host's* loopback. Inside a container
# that is somebody else's loopback, so a session running this script has to
# reach the service it just started on the compose network instead — by the
# container's own address, on the container port rather than the published
# one. Everything the stage then checks is unchanged; only the address is.
if [ -f /.dockerenv ]; then
	cid="$(docker compose ps -q harness)"
	[ -n "$cid" ] || die "the harness service has no container"
	ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "$cid")"
	[ -n "$ip" ] || die "the harness container has no address on its network"
	base="http://$ip:8080"
	echo "in a container: reaching the service at $base rather than the published $port"
fi

# Wait for it to answer at all before asking what it serves.
i=0
until curl -sf "$base/api/queue" >/dev/null 2>&1; do
	i=$((i + 1))
	[ "$i" -lt 60 ] || die "$base did not become healthy within 60s"
	sleep 1
done

# The check the whole script is for: the container is serving the bundle this
# build produced, not one from an earlier run.
served="$(curl -sf "$base/" | grep -o 'assets/index-[A-Za-z0-9_-]*\.css' | head -1 | sed 's|assets/||')"
[ -n "$served" ] || die "the served index.html references no stylesheet"
[ "$served" = "$css" ] || die "the container serves $served but this build produced $css"
curl -sf "$base/assets/$css" >/dev/null || die "the served stylesheet 404s"

printf '\nok — %s is serving %s\n' "$base" "$css"
