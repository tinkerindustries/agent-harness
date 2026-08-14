#!/bin/sh
# Drives layout-metrics.mjs inside the running harness container, which is
# where Playwright and Chromium already are. Nothing is installed on the host.
#
# Usage: scripts/layout-metrics.sh /evals/evr-abc123
#        scripts/layout-metrics.sh / --width=390 --root=.session-table
#        BASE_URL=http://host.docker.internal:8080 scripts/layout-metrics.sh /settings
#
# BASE_URL is where the page is fetched from and defaults to the container's own
# harness on :8080. Point it at another stack to read a page this checkout is
# not serving — from inside a container the host's published ports are on
# host.docker.internal, never 127.0.0.1, which is the container's own loopback.
#
# SERVICE names which compose service runs the browser; the stack must be up:
# docker compose up -d --build
set -eu

cd "$(dirname "$0")/.."
SERVICE="${SERVICE:-harness}"

container="$(docker compose ps -q "$SERVICE")"
if [ -z "$container" ]; then
	echo "layout-metrics: the $SERVICE service is not running; docker compose up -d --build" >&2
	exit 1
fi

docker cp scripts/layout-metrics.mjs "$container:/tmp/layout-metrics.mjs"
docker compose exec -T -e BASE_URL="${BASE_URL:-http://localhost:8080}" "$SERVICE" \
	node /tmp/layout-metrics.mjs "$@"
