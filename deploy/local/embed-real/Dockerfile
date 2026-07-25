# Real BGE-M3 embedding service (1024-dim), TEI wire protocol.
#
# The model is NOT baked into the image: it downloads on first start into the
# hf-cache volume the compose file mounts. That keeps the image small and the
# weights surviving image rebuilds — a rebuild of this Dockerfile must never
# re-download 2GB.
#
# CPU torch explicitly: the default index resolves CUDA wheels that are both
# enormous and useless on an arm64 Mac.
FROM python:3.12-slim

# torch>=2.6 is a hard floor, not a preference: transformers refuses to
# torch.load a .bin checkpoint on anything older (CVE-2025-32434), and BGE-M3
# ships one. Pinning 2.5.x fails at model load, not at build.
RUN pip install --no-cache-dir \
      --extra-index-url https://download.pytorch.org/whl/cpu \
      "torch>=2.6,<3" \
    && pip install --no-cache-dir "sentence-transformers>=3.3,<6"

ENV HF_HOME=/hf-cache \
    EMBED_MODEL=BAAI/bge-m3 \
    EMBED_PORT=8081 \
    # Threads are capped so the embedder cannot starve OpenSearch and engramd
    # of CPU on a single dev machine during a full-corpus backfill.
    OMP_NUM_THREADS=4

COPY server.py /app/server.py
WORKDIR /app
EXPOSE 8081
CMD ["python", "-u", "server.py"]
