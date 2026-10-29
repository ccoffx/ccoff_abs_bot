#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
export CGO_ENABLED=1 GOFLAGS=-mod=readonly
if ! command -v go >/dev/null 2>&1 && [ -x .tools/go/bin/go ]; then
  export PATH="$PWD/.tools/go/bin:$PATH"
fi
if command -v go >/dev/null 2>&1; then
  go test ./...
  go test -race ./...
  go vet ./...
else
  docker run --rm -v "$PWD:/src" -w /src -e CGO_ENABLED -e GOFLAGS golang:1.26.10 sh -ec 'go test ./...; go test -race ./...; go vet ./...'
fi
python3 scripts/test_deploy.py
python3 scripts/test_release.py
