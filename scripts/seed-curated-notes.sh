#!/usr/bin/env bash
# Convenience wrapper around `engram-seed-knowledge`: creates (or tolerates
# the already-existing) `curated_notes` knowledge collection and ingests its
# fixed demo-doc set, mapped to real rtd memory entity ids. Idempotent --
# re-running upserts the same docs in place.
#
# Requires a token bearing the admin (or harvester) role. Mint one first:
#   engram token create --tenant T --user U --roles admin
#
# Env:
#   ENGRAM_ADDR   engramd address (default: localhost:7070, per the CLI's own default)
#   ENGRAM_TOKEN  bearer token (required -- an admin/harvester-role token)
#
# Usage:
#   ENGRAM_TOKEN=<admin-token> ./scripts/seed-curated-notes.sh [extra engram-seed-knowledge flags]
set -euo pipefail

if [[ -z "${ENGRAM_TOKEN:-}" ]]; then
  echo "error: ENGRAM_TOKEN is required (mint an admin token: engram token create --roles admin)" >&2
  exit 1
fi

echo "== seeding curated_notes (addr=${ENGRAM_ADDR:-localhost:7070}) =="
exec go run ./cmd/engram-seed-knowledge "$@"
