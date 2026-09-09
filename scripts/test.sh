#!/bin/sh
# Runs the Go suite. The suite needs no broker and no other service: every
# test uses a fresh SQLite store in a temp dir, so this script is the whole
# job.
#
# Usage: scripts/test.sh [go test flags...]   runs every package.
set -eu

cd "$(dirname "$0")/.."

go test ./... "$@"
