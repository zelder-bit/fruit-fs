#!/usr/bin/env bash
# Runs the Go toolchain in a container, so Go doesn't need to be installed.
#   scripts/go.sh test -race ./...
#   scripts/go.sh run ./cmd/fruit-fs hash-password
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
exec docker run --rm -i -u "$(id -u):$(id -g)" -e HOME=/tmp \
  -e GOCACHE=/cache/build -e GOMODCACHE=/cache/mod -e GOFLAGS -e CGO_ENABLED \
  -v fruit-fs-gocache:/cache -v "$PWD":/src -w /src golang:1.26 go "$@"
