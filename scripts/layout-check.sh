#!/bin/sh
# Drives layout-check.mjs inside the running harness container, which is
# where Playwright and Chromium already are. Nothing is installed on the
# host.
#
# Usage: scripts/layout-check.sh          against the dev stack
#        SERVICE=harness scripts/layout-check.sh
#
# The stack must be up: docker compose up -d --build
set -eu

cd "$(dirname "$0")/.."
SERVICE="${SERVICE:-harness}"

container="$(docker compose ps -q "$SERVICE")"
if [ -z "$container" ]; then
	echo "layout-check: the $SERVICE service is not running; docker compose up -d --build" >&2
	exit 1
fi

docker cp scripts/layout-check.mjs "$container:/tmp/layout-check.mjs"
docker compose exec -T "$SERVICE" node /tmp/layout-check.mjs
