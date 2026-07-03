#!/usr/bin/env bash
# Regenerates api/engrampb from api/proto via buf (DW-0.7). Everything is a
# version-pinned `go run` — no host installs; CI runs this and fails on diff.
set -euo pipefail
cd "$(dirname "$0")/.."

BUF_VERSION=v1.55.1

go run "github.com/bufbuild/buf/cmd/buf@${BUF_VERSION}" lint
go run "github.com/bufbuild/buf/cmd/buf@${BUF_VERSION}" generate
echo "codegen: ok"
