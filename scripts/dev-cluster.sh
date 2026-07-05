#!/usr/bin/env bash
# Starts the pinned dev OpenSearch (D14: 3.1 exactly) as a local container.
# Uses docker when present, else podman (drop-in CLI-compatible).
set -euo pipefail

IMAGE="docker.io/opensearchproject/opensearch:3.1.0" # pinned — D14
NAME="engram-dev-os"
PORT="${ENGRAM_OPENSEARCH_PORT:-9200}"

if command -v docker >/dev/null 2>&1; then
  RUNTIME=docker
elif command -v podman >/dev/null 2>&1; then
  RUNTIME=podman
else
  echo "error: neither docker nor podman found; install one to run the dev cluster" >&2
  exit 1
fi

if "$RUNTIME" ps --format '{{.Names}}' | grep -qx "$NAME"; then
  echo "$NAME already running"
else
  "$RUNTIME" rm -f "$NAME" >/dev/null 2>&1 || true
  "$RUNTIME" run -d --name "$NAME" -p "${PORT}:9200" \
    -e discovery.type=single-node \
    -e DISABLE_SECURITY_PLUGIN=true \
    -e DISABLE_INSTALL_DEMO_CONFIG=true \
    -e 'OPENSEARCH_JAVA_OPTS=-Xms512m -Xmx1g' \
    -e path.repo=/usr/share/opensearch/snapshots \
    "$IMAGE"
fi

echo -n "waiting for cluster"
for _ in $(seq 1 60); do
  if curl -sf "http://localhost:${PORT}/" >/dev/null 2>&1; then
    echo " — up"
    curl -s "http://localhost:${PORT}/" | grep '"number"'
    exit 0
  fi
  echo -n .
  sleep 2
done
echo " — timed out waiting for OpenSearch on :${PORT}" >&2
exit 1
