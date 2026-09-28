#!/bin/sh
# Builds bin/harness and runs one prompt through a live session, printing the
# frames it reads back. It calls a real model and costs real money.
#
# Usage: scripts/drive.sh [-dialect claude|responses|interactions] [-model M]
#                         [-env FILE] [-cwd DIR] [-mode readonly|full] PROMPT
# `go run ./cmd/drive -h` lists every flag.
set -eu

cd "$(dirname "$0")/.."

go build -o bin/harness ./cmd/harness
exec go run ./cmd/drive -harness bin/harness "$@"
