#!/bin/sh
# Runs the Go suite against the broker in docker-compose.test.yml. Without
# it the integration tests skip; pointed at the deployment's broker they
# fight the running pool for messages. The broker comes down again when the
# suite finishes, however it finishes.
#
# The compose broker publishes onto the host's loopback, which a container
# that shares the docker socket — every agent session — does not share.
# When that port is unreachable from where the script runs, it falls back
# to a local `nats-server` (baked into the image, see the Dockerfile) on
# the same port with JetStream on, so the same command works on a host and
# in a session container with nothing passed and nothing configured.
# Compose stays the default: a host run still exercises the broker image
# CI uses.
#
# Usage: scripts/test.sh [go test flags...]   runs every package.
# For a subset, start the broker yourself and call go test directly:
#   docker compose -f docker-compose.test.yml up -d --wait
#   go test ./internal/queue/...
#   docker compose -f docker-compose.test.yml down
set -eu

cd "$(dirname "$0")/.."

# Looks a key up the way the tests do (config.LoadDotEnv): a real
# environment variable always wins, then the first definition in .env.
# docker compose's own parser keeps the *last* duplicate instead — which is
# why COMPOSE_PROJECT_NAME below reads with tail -n1 — but these keys never
# repeat in a worktree's .env (internal/worktree strips its managed keys
# outside the block it writes), so first-wins matches the reader that
# decides what URL the suite dials.
env_value() {
	key=$1
	if [ -n "$(printenv "$key")" ]; then
		printenv "$key"
		return
	fi
	[ -f .env ] || return 0
	sed -n "s/^${key}=//p" .env | head -n1
}

# The URL the suite will dial, resolved exactly as the tests resolve it
# (testNATSURL in internal/queue, internal/worker and internal/mcp):
# HARNESS_TEST_NATS_URL wins, otherwise HARNESS_TEST_NATS_PORT, otherwise
# the 4422 default. `harness worktree init` writes both keys into .env, so
# a worktree's allocated port keeps working whichever path this script
# takes.
nats_url=$(env_value HARNESS_TEST_NATS_URL)
if [ -z "$nats_url" ]; then
	nats_port=$(env_value HARNESS_TEST_NATS_PORT)
	nats_url="nats://127.0.0.1:${nats_port:-4422}"
fi

# The host:port from that URL, for the reachability probe. Only the plain
# nats://127.0.0.1:PORT shape is produced here and by worktree init; the
# stripping just tolerates a path, extra URLs or userinfo without pretending
# to be a NATS URL parser.
nats_hostport=${nats_url#nats://}
nats_hostport=${nats_hostport%%/*}
nats_hostport=${nats_hostport%%,*}
nats_hostport=${nats_hostport##*@}

# The test broker's own compose project name. docker-compose.test.yml pins
# "deepseek-harness-test" (see its own name: field), which is fine for a
# single checkout but collides the moment two worktrees run this script at
# once. .env's COMPOSE_PROJECT_NAME — written per-worktree by `harness
# worktree init` — names the *dev* stack; deriving the test project from it
# keeps the two stacks apart from each other and from any other worktree's,
# without touching docker-compose.test.yml. Passing -p explicitly always
# wins over the file's name:, so the file's own default still works for
# anyone who runs `docker compose -f docker-compose.test.yml` directly.
project=deepseek-harness-test
if [ -f .env ]; then
	dev_project=$(sed -n 's/^COMPOSE_PROJECT_NAME=//p' .env | tail -n1)
	[ -n "$dev_project" ] && project="${dev_project}-test"
fi

# What this run started beyond the compose broker: empty until the fallback
# path actually starts nats-server, so teardown's compose down stays the
# whole job on a normal host run. probe_dir is initialized too — teardown
# references it, and set -u must not trip if a signal lands between the
# traps below and its creation.
nats_pid=
nats_store=
probe_dir=

# Clears its own EXIT trap so a signal path and the trap cannot both fire it.
# Tears down whatever this run started, however it finishes: the compose
# broker, and — only on the fallback path — the local nats-server, its
# store, and the probe source.
teardown() {
	trap - EXIT
	if [ -n "$nats_pid" ]; then
		kill "$nats_pid" 2>/dev/null || true
		wait "$nats_pid" 2>/dev/null || true
		rm -rf "$nats_store"
	fi
	rm -rf "$probe_dir"
	docker compose -f docker-compose.test.yml -p "$project" down
}

docker compose -f docker-compose.test.yml -p "$project" up -d --wait

trap teardown EXIT
trap 'teardown; exit 130' INT
trap 'teardown; exit 143' TERM

# Probe source, in a temp dir so concurrent runs cannot collide. Go is the
# one dependency this script is guaranteed to have — its whole job is
# running go test — while nc is not on every host it runs on; the probe
# compiles once and is cached, so it costs milliseconds. Teardown removes
# the dir, and the EXIT trap is already set, so an early exit still cleans
# it up.
probe_dir=$(mktemp -d "${TMPDIR:-/tmp}/testsh-probe.XXXXXX")
probe_file="$probe_dir/probe.go"
cat > "$probe_file" <<'EOF'
package main

import (
	"fmt"
	"net"
	"os"
	"time"
)

func main() {
	conn, err := net.DialTimeout("tcp", os.Args[1], 2*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	conn.Close()
}
EOF
# Asks the same question the suite's nats.go connect asks: does anything
# accept TCP connections at the resolved host:port?
probe_reachable() {
	go run "$probe_file" "$1" >/dev/null 2>&1
}

# The suite's port, reachable or not? On a host the compose broker answers
# here and nothing more happens. In a container sharing the docker socket
# the broker runs on the host's loopback, which this network namespace does
# not share — start nats-server on the same port instead, JetStream on,
# stream state in a temp store so a run cannot inherit messages a previous
# run left unacked (the same contract docker-compose.test.yml's tmpfs gives
# the compose broker).
if ! probe_reachable "$nats_hostport"; then
	if ! command -v nats-server >/dev/null 2>&1; then
		echo "scripts/test.sh: compose broker unreachable at ${nats_url} and no nats-server on PATH" >&2
		echo "scripts/test.sh: install the version pinned in the Dockerfile (ARG NATS_SERVER_VERSION)" >&2
		exit 1
	fi
	nats_store=$(mktemp -d "${TMPDIR:-/tmp}/testsh-nats.XXXXXX")
	nats-server -js -a "${nats_hostport%:*}" -p "${nats_hostport##*:}" -sd "$nats_store" >/dev/null 2>&1 &
	nats_pid=$!
	attempt=0
	while ! probe_reachable "$nats_hostport"; do
		attempt=$((attempt + 1))
		if [ "$attempt" -ge 100 ]; then
			echo "scripts/test.sh: nats-server did not come up on ${nats_hostport}" >&2
			exit 1
		fi
		sleep 0.1
	done
fi

# Every package in this module except the agent workspaces.
#
# The harness writes each session's workspace into workspaces/ (dev) and
# workspaces-prod/ (prod), both inside this checkout, so whatever Go a
# session happens to leave there is part of this module as far as the go
# tool is concerned. One prod session's scratch/ holds two files that each
# declare main, which makes `go test ./...` fail to build a package that is
# not ours, was never ours, and cannot be deleted — workspaces-prod/ belongs
# to the running production stack (see CLAUDE.md). That failure is worse
# than noise: it makes the suite exit non-zero every single run, so a real
# failure has nothing left to signal with.
#
# Filtering the listed packages rather than naming ./cmd/... ./internal/...
# is deliberate. A hardcoded pair of roots silently stops covering a new
# top-level package, and tests that quietly do not run are a worse bug than
# the one being fixed here.
packages=$(go list ./... | grep -v "/workspaces") || exit 1

status=0
go test $packages "$@" || status=$?

teardown
exit "$status"
