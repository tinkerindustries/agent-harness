#!/bin/sh
# Runs the Go suite against the broker in docker-compose.test.yml. Without
# it the integration tests skip; pointed at the deployment's broker they
# fight the running pool for messages.
#
# Usage: scripts/test.sh [go test flags...]   runs every package.
# For a subset, start the broker with this script once, then call go test
# directly. Stop the broker with:
#   docker compose -f docker-compose.test.yml down
set -eu

cd "$(dirname "$0")/.."

docker compose -f docker-compose.test.yml up -d --wait

exec go test ./... "$@"
