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
