#!/usr/bin/env bash
# Snapshot the live local memory store (engram-e2e-os, :9201) and mirror the
# snapshot repository to the host.
#
# The repository (`local`, registered on first run) lives on the
# engram-os-snapshots volume inside the podman VM. That survives a container
# recreate but not the loss of the VM itself, so every run also copies the
# whole repository out to $ENGRAM_BACKUP_DIR (default ~/engram-backups/os-snapshots)
# on the macOS host. Snapshot repository files are written once and never
# modified in place, so copying between snapshots is consistent.
#
# Keeps the newest $KEEP snapshots (default 14) and deletes older ones.
# Restore: docs/runbooks/05-restore-from-snapshot.md (restore under a new
# name, verify, cut over; never in place).
#
# Usage: scripts/snapshot-local.sh            # take + prune + mirror
set -euo pipefail

OS_URL="${ENGRAM_OPENSEARCH_URL:-http://localhost:9201}"
REPO=local
KEEP="${KEEP:-14}"
BACKUP_DIR="${ENGRAM_BACKUP_DIR:-$HOME/engram-backups/os-snapshots}"
CONTAINER=engram-e2e-os

# Idempotent: re-registering the same fs location is a no-op.
curl -sf -XPUT "$OS_URL/_snapshot/$REPO" -H 'content-type: application/json' \
	-d '{"type":"fs","settings":{"location":"/usr/share/opensearch/snapshots"}}' >/dev/null

name="auto-$(date -u +%Y%m%dt%H%M%Sz)"
result="$(curl -sf -XPUT "$OS_URL/_snapshot/$REPO/$name?wait_for_completion=true" \
	-H 'content-type: application/json' \
	-d '{"indices":"engram-*,knowledge-*","include_global_state":false}')"
state="$(printf '%s' "$result" | python3 -c 'import json,sys; print(json.load(sys.stdin)["snapshot"]["state"])')"
if [[ "$state" != SUCCESS ]]; then
	echo "snapshot $name finished $state, not SUCCESS:" >&2
	printf '%s\n' "$result" >&2
	exit 1
fi
echo "snapshot $name: $state"

# Prune: snapshots come back oldest first.
all=()
while IFS= read -r id; do all+=("$id"); done < <(curl -sf "$OS_URL/_cat/snapshots/$REPO?h=id&s=start_epoch")
excess=$(( ${#all[@]} - KEEP ))
for (( i = 0; i < excess; i++ )); do
	curl -sf -XDELETE "$OS_URL/_snapshot/$REPO/${all[$i]}" >/dev/null
	echo "pruned ${all[$i]}"
done

mkdir -p "$BACKUP_DIR"
staging="$(mktemp -d "$BACKUP_DIR/.staging.XXXXXX")"
podman cp "$CONTAINER:/usr/share/opensearch/snapshots/." "$staging/"
rm -rf "$BACKUP_DIR/repo"
mv "$staging" "$BACKUP_DIR/repo"
echo "mirrored repository to $BACKUP_DIR/repo ($(du -sh "$BACKUP_DIR/repo" | cut -f1))"
