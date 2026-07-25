#!/usr/bin/env bash
# Run the real BGE-M3 embedder NATIVELY on macOS, on the Mac's GPU.
#
# Why not in a container: macOS does not pass Metal through to the podman VM,
# so an in-container torch is CPU-bound on however many cores the VM was given
# — on this machine that meant BGE-M3 grinding at ~13 records/minute and then
# being OOM-killed. Running the identical service on the host reaches the GPU
# through torch's MPS backend.
#
# This is the same precedent as cmd/engram-extract-shim, which also runs on the
# host and is reached from the stack via host.docker.internal (see the
# -extract-url comment in ../docker-compose.yml).
#
# Nothing about the model changes: same BAAI/bge-m3, same 1024 dimensions, same
# /embed + /health wire contract. Only the device differs, so local retrieval
# stays faithful to what deploy/aws provisions.
#
# Usage:
#   ./run-host.sh              # foreground
#   ./run-host.sh & disown     # background
#
# Then bring the stack up with the host-embed override:
#   podman compose -p local -f docker-compose.yml -f docker-compose.host-embed.yml up -d
set -euo pipefail

cd "$(dirname "$0")"

VENV=".venv"
if [ ! -x "$VENV/bin/python" ]; then
  echo "run-host: creating $VENV (torch + sentence-transformers, ~1.5GB)" >&2
  # 3.12 matches the container image, so a bug that reproduces in one
  # reproduces in the other.
  uv venv --python 3.12 "$VENV"
  VIRTUAL_ENV="$VENV" uv pip install "torch>=2.6,<3" "sentence-transformers>=3.3,<6"
fi

# mps is the whole point of running out here; if torch can't see the GPU we want
# to know immediately rather than discover it later as unexplained slowness.
"$VENV/bin/python" - <<'PY'
import sys, torch
if not torch.backends.mps.is_available():
    sys.exit("run-host: torch reports no MPS device — refusing to start, "
             "since falling back to cpu here would be slower than the container")
PY

export EMBED_DEVICE="${EMBED_DEVICE:-mps}"
export EMBED_MODEL="${EMBED_MODEL:-BAAI/bge-m3}"
export EMBED_PORT="${EMBED_PORT:-8081}"
# The container caps batches at 32 to survive a 4GB memory limit. On 64GB of
# unified memory that cap is pure overhead per round-trip, so the host default
# is larger; still overridable for a memory-constrained Mac.
export EMBED_MAX_BATCH="${EMBED_MAX_BATCH:-128}"

echo "run-host: $EMBED_MODEL on $EMBED_DEVICE, :$EMBED_PORT, batch $EMBED_MAX_BATCH" >&2
exec "$VENV/bin/python" -u server.py
