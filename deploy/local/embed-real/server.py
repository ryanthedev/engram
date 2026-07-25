"""A real BGE-M3 embedding service speaking the TEI wire protocol.

Drop-in replacement for cmd/engram-embed-server (the deterministic hash-based
stand-in) at the same /embed + /health surface, so switching between them is a
compose flag and nothing else. Matches what deploy/aws provisions in
production: a co-located BGE-M3 container, 1024 dimensions, which is exactly
the dimension the engram-episodic/engram-semantic index mappings already
declare — so swapping the model needs no reindex.

Wire contract (internal/embed/http.go):
  POST /embed  {"inputs": ["text", ...]}  ->  [[f32 x 1024], ...] index-aligned
  GET  /health ->  200 once the model is resident

Stdlib HTTP on purpose: sentence-transformers already pulls torch, and a web
framework on top would add install surface for two routes.
"""

import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from sentence_transformers import SentenceTransformer

MODEL_ID = os.environ.get("EMBED_MODEL", "BAAI/bge-m3")
PORT = int(os.environ.get("EMBED_PORT", "8081"))
# Defaults to cpu because the container has no other option: macOS does not pass
# Metal through to the podman VM, so in-container torch is CPU-bound no matter
# what. Set EMBED_DEVICE=mps to run this same service natively on the host and
# reach the Mac's GPU — same model, same dimensions, same wire protocol, so the
# only thing that changes is throughput. See run-host.sh.
DEVICE = os.environ.get("EMBED_DEVICE", "cpu")
# Cap one request's batch so a large backfill page can't drive the container
# out of memory; the caller's batch is chunked to this and reassembled in
# order, so index alignment holds regardless of what arrives.
MAX_BATCH = int(os.environ.get("EMBED_MAX_BATCH", "32"))

_model = None
_model_lock = threading.Lock()
_ready = threading.Event()


def load_model():
    """Load the model once, in the background, so /health is meaningful.

    The first run downloads ~2GB into the HF cache volume; binding the port
    before that finishes lets the healthcheck report not-ready instead of the
    container looking dead.
    """
    global _model
    model = SentenceTransformer(MODEL_ID, device=DEVICE)
    with _model_lock:
        _model = model
    _ready.set()
    # sentence-transformers 6 renamed get_sentence_embedding_dimension to
    # get_embedding_dimension and warns on the old name. The pin spans both
    # (>=3.3,<6 in the image, whatever the host venv resolved), so ask for the
    # new name and fall back rather than pinning the whole service to one side
    # of the rename for a log line.
    dim = getattr(model, "get_embedding_dimension", None) or model.get_sentence_embedding_dimension
    print(f"embed: {MODEL_ID} resident on {DEVICE}, dim={dim()}", flush=True)


def embed(texts):
    """Embed texts, returning unit-normalized vectors index-aligned to input.

    normalize_embeddings=True matters: the kNN clause scores by cosine, and
    normalized vectors make that equivalent to a dot product — the same
    convention the fake embedder followed by construction.
    """
    out = []
    for i in range(0, len(texts), MAX_BATCH):
        chunk = texts[i : i + MAX_BATCH]
        vecs = _model.encode(chunk, normalize_embeddings=True, show_progress_bar=False)
        out.extend(v.tolist() for v in vecs)
    return out


class Handler(BaseHTTPRequestHandler):
    def _send(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path != "/health":
            self._send(404, {"error": "not found"})
            return
        if not _ready.is_set():
            # 503 until the weights are resident: the compose healthcheck gates
            # engramd on this, so engramd never starts against a model that
            # would fail its first embed call.
            self._send(503, {"status": "loading", "model": MODEL_ID, "device": DEVICE})
            return
        # Report the device: a service that silently fell back to cpu looks
        # identical to one on the GPU except for being ~20x slower, and "it's
        # running, it must be fine" is how the fake-vector problem survived.
        self._send(200, {"status": "ok", "model": MODEL_ID, "device": DEVICE})

    def do_POST(self):
        if self.path != "/embed":
            self._send(404, {"error": "not found"})
            return
        if not _ready.is_set():
            self._send(503, {"error": "model still loading"})
            return
        try:
            raw = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            texts = json.loads(raw).get("inputs")
            if not isinstance(texts, list):
                raise ValueError('"inputs" must be a list of strings')
            self._send(200, embed(texts))
        except Exception as err:  # noqa: BLE001 — surface any failure as 4xx/5xx JSON
            self._send(400, {"error": str(err)})

    def log_message(self, fmt, *args):
        pass  # one line per embed call would drown the backfill's own output


if __name__ == "__main__":
    threading.Thread(target=load_model, daemon=True).start()
    print(f"embed: serving on :{PORT}, loading {MODEL_ID}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
