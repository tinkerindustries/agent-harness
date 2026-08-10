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

# Clears its own EXIT trap so a signal path and the trap cannot both fire it.
teardown() {
	trap - EXIT
	docker compose -f docker-compose.test.yml -p "$project" down
}

docker compose -f docker-compose.test.yml -p "$project" up -d --wait

trap teardown EXIT
trap 'teardown; exit 130' INT
trap 'teardown; exit 143' TERM

status=0
go test ./... "$@" || status=$?

teardown
exit "$status"
