#!/usr/bin/env bash
# Phase 2 backfill (.code-foundations/plans/2026-07-08-extraction-cli-shim.md):
# forces re-extraction of already-processed episodic events for one tenant.
#
# Bumping -extractor-version alone does NOT re-claim already-processed
# events: internal/store/outbox.go's ClaimBatch gate is `must_not
# exists(processed_at)` and has no notion of extractor_version at all. So
# this script clears processed_at (the outbox claim gate) for the given
# tenant; -extractor-version must ALSO be bumped separately on engramd
# (deploy/local/docker-compose.yml) so the re-claimed events don't
# short-circuit on their existing v1 LedgerComplete ledger entries
# (internal/worker/worker.go ProcessEvent's replay short-circuit).
#
# Snapshots the affected event ids to a file BEFORE mutating anything
# (rollback evidence — the plan requires this since the reset is the one
# non-additive step in this phase).
set -euo pipefail

OS_URL="${ENGRAM_E2E_OS_URL:-http://localhost:9201}"
INDEX="${ENGRAM_EPISODIC_INDEX:-engram-episodic-000001}"
TENANT="${1:?usage: backfill-reextract-rtd.sh <tenant_id> [snapshot_dir]}"
SNAPSHOT_DIR="${2:-.}"
SNAPSHOT_FILE="${SNAPSHOT_DIR}/backfill-${TENANT}-$(date -u +%Y%m%dT%H%M%SZ)-event-ids.txt"

echo "== snapshotting affected event ids (tenant=${TENANT}, index=${INDEX}) =="
curl -sf "${OS_URL}/${INDEX}/_search" -H 'Content-Type: application/json' -d '{
  "size": 1000,
  "_source": ["event_id","processed_at","attempts"],
  "query": {"bool": {"filter": [
    {"term": {"tenant_id": "'"${TENANT}"'"}},
    {"exists": {"field": "processed_at"}}
  ]}}
}' | python3 -c "
import json, sys
d = json.load(sys.stdin)
hits = d['hits']['hits']
total = d['hits']['total']['value']
if total != len(hits):
    print(f'error: total={total} but only {len(hits)} returned (raise size)', file=sys.stderr)
    sys.exit(1)
with open('${SNAPSHOT_FILE}', 'w') as f:
    for h in hits:
        f.write(h['_source']['event_id'] + '\n')
print(f'snapshotted {len(hits)} event ids -> ${SNAPSHOT_FILE}')
"

COUNT=$(wc -l < "${SNAPSHOT_FILE}" | tr -d ' ')
echo "== clearing processed_at for ${COUNT} tenant=${TENANT} episodic docs =="
RESP=$(curl -sf -X POST "${OS_URL}/${INDEX}/_update_by_query?refresh=true&conflicts=proceed" \
  -H 'Content-Type: application/json' -d '{
    "script": {
      "source": "ctx._source.remove(\"processed_at\")",
      "lang": "painless"
    },
    "query": {"bool": {"filter": [
      {"term": {"tenant_id": "'"${TENANT}"'"}},
      {"exists": {"field": "processed_at"}}
    ]}}
  }')
echo "${RESP}" | python3 -m json.tool
UPDATED=$(echo "${RESP}" | python3 -c "import json,sys; print(json.load(sys.stdin).get('updated', 0))")
echo "== done: ${UPDATED} docs re-queued (processed_at cleared) for tenant=${TENANT} =="
echo "== rollback: re-stamp processed_at for event ids in ${SNAPSHOT_FILE} if needed =="
