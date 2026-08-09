#!/bin/sh
# Runs the Go suite against the broker in docker-compose.test.yml. Without
# it the integration tests skip; pointed at the deployment's broker they
# fight the running pool for messages. The broker comes down again when the
# suite finishes, however it finishes.
#
# Usage: scripts/test.sh [go test flags...]   runs every package.
# For a subset, start the broker yourself and call go test directly:
#   docker compose -f docker-compose.test.yml up -d --wait
#   go test ./internal/queue/...
#   docker compose -f docker-compose.test.yml down
set -eu

cd "$(dirname "$0")/.."

# Clears its own EXIT trap so a signal path and the trap cannot both fire it.
teardown() {
	trap - EXIT
	docker compose -f docker-compose.test.yml down
}

docker compose -f docker-compose.test.yml up -d --wait

trap teardown EXIT
trap 'teardown; exit 130' INT
trap 'teardown; exit 143' TERM

status=0
go test ./... "$@" || status=$?

teardown
exit "$status"
