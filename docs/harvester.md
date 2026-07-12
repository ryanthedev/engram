# Engram Harvester — Layer-2 Crawler & Ingestion Tool

The `engram-harvester` is a Layer-2 document harvester designed to pull metadata and content from external document sources and feed it into Engram's knowledge platform using the `knowledge_*` gRPC APIs. 

Engram's server (`engram-server`) itself is entirely passive; it handles storage, search, and memory reconciliation, but **never crawls or initiates pulls**. The `engram-harvester` runs as a separate, one-shot process (e.g., triggered by cron or systemd timers) that contacts the gRPC API of `engram-server`.

## Manifest Schema

The harvester is configured using a YAML manifest file. A manifest maps a set of `collections` to their respective document `sources`.

Every collection `name` must already be registered in engram (via
`knowledge_create_collection`) with a mapping that covers the fields a source
emits — the knowledge index is `dynamic:strict`, so an unmapped field is
rejected at ingest. arXiv sources emit `categories, published_date,
update_date, doi, journal_ref, comments, authors`; `github-repos` emits
`repo, path`; `web-crawl` emits `url`.

### Example `sources.yaml`

```yaml
collections:
  - name: arxiv                       # text_field: abstract
    sources:
      # One-time backfill from the Kaggle metadata dump (download separately).
      - { type: arxiv-kaggle, path: "/data/arxiv-metadata-oai-snapshot.json.gz", filter: "cs.*" }
      # Nightly incremental via arXiv OAI-PMH (from = now - lookback).
      - { type: arxiv-oaipmh, set: "cs", lookback: "48h" }

  - name: docs                        # text_field: body ; keyword fields: repo, path
    sources:
      # NOTE: list EVERY repo for one collection in a SINGLE github-repos entry
      # (see "Multi-repo & sweep scope" below) — one run, one sweep.
      - type: github-repos
        repos: ["facebook/react-native-website", "cloudflare/cloudflare-docs"]
        files: ["docs/**/*.md", "src/content/docs/**/*.mdx"]

  - name: docs-sites                  # text_field: body ; keyword field: url
    sources:
      - { type: web-crawl, seeds: ["https://docs.astral.sh/uv/"], max_pages: 500 }
```

### Source Type Config Keys

1. **`arxiv-kaggle`** (Full Harvest — sweeps)
   - Streams a local gzipped arXiv metadata dump (constant memory). No PDFs.
   - `path` (string, **required**): path to the `.json.gz` metadata dump.
   - `filter` (string, optional, default `cs.*`): category prefix filter.
   - `dump_date` (string, optional): provenance date; defaults to the file mtime.

2. **`arxiv-oaipmh`** (Incremental — additive, does NOT sweep)
   - Pulls new/updated papers from the arXiv OAI-PMH endpoint. `status="deleted"`
     records are logged and skipped (the API has no per-doc-id delete — a
     withdrawn paper is not removed in real time). No PDFs.
   - `base_url` (string, optional, default `https://oaipmh.arxiv.org/oai`).
   - `set` (string, optional, default `cs`).
   - `metadata_prefix` (string, optional, default `arXiv`).
   - `lookback` (Go duration, optional, default `48h`): window is `from = now - lookback`
     (date-granular). The overlap is idempotent (upsert-by-id), so no cursor is kept.

3. **`github-repos`** (Full Harvest — sweeps)
   - Shallow-clones each repo via the `git` CLI and indexes matched files, one
     doc per file (`id = owner/repo/path`, `source_version = sha:<HEAD>`). Only
     `https`/`http` transports; symlinks are skipped.
   - `repos` (array of `owner/repo`, **required**).
   - `files` (array of globs, optional, default `["README.md"]`): supports `**`.
   - `base_url` (string, optional, default `https://github.com/`).
   - `max_file_bytes` (int, optional, default `1048576`): larger/binary files skipped.

4. **`web-crawl`** (Full Harvest — sweeps)
   - Bounded, same-host BFS crawl; extracts page text; honors `robots.txt`.
     SSRF-guarded (blocks private/loopback/link-local IPs at dial time).
   - `seeds` (array of URLs, **required**).
   - `max_pages` (int, optional, default `100`).
   - `max_page_bytes` (int, optional, default `1048576`).
   - `delay` (Go duration, optional, default `200ms`): per-host politeness.
   - `max_frontier` (int, optional, default `10 × max_pages`): hard cap on discovered URLs.
   - `user_agent` (string, optional).

### Multi-repo & sweep scope (important)

A Full-Harvest source's mark-and-sweep deletes every row for
`(collection, source_type)` that the current run did **not** re-ingest. All
rows written by a given source type share one sweep scope, so **every repo /
seed feeding one collection under one source type must be harvested in a single
manifest/run**. Harvesting `repoA` and `repoB` as two separate `engram-harvester`
invocations (each `source=github-repos`) makes the second run's sweep delete the
first run's documents. Put them in one `github-repos` entry (`repos: [repoA, repoB]`)
so a single run ingests both and the sweep keeps both. (Per-repo sweep scoping is
a possible future enhancement.)

## Running the Harvester

The harvester requires a valid gRPC authentication token. To protect sensitive credentials, the token **MUST** be supplied via the `ENGRAM_HARVESTER_TOKEN` environment variable. The harvester will reject running if the token is passed in flags/arguments, or if it is empty.

```bash
export ENGRAM_HARVESTER_TOKEN=egm_AvxGRzdGSCSeqRwsDyf6Z--aYAHmsoSHs_D539QmMYo
engram-harvester --manifest sources.yaml --addr localhost:7070
```

### Flags

- `--manifest` (path, required): Path to the `sources.yaml` manifest.
- `--collection` (repeatable or comma-separated): Optional collection filter (e.g. `--collection general-knowledge`).
- `--source` (repeatable or comma-separated): Optional source type filter (e.g. `--source arxiv-kaggle`).
- `--addr` (string, default `":7070"`): The address of the target `engram-server` gRPC service.
- `--batch-size` (int, default `500`): The document batching size when sending documents to the server.
- `--timeout` (duration, default `"6h"`): Overall run deadline timeout.
- `--once` / `--backfill` (boolean, no-ops): Documented aliases to show that the harvester is always a one-shot process.

## Scheduling & Automation

Since harvesting is a one-shot operation, regular updates should be automated using a scheduler.

### Nightly Cron Example

Run the harvester every night at 2:00 AM:

```cron
0 2 * * * ENGRAM_HARVESTER_TOKEN=egm_XXXX... /usr/local/bin/engram-harvester --manifest /etc/engram/sources.yaml --addr localhost:7070 >> /var/log/engram-harvester.log 2>&1
```

### Systemd Timer Example

Create a systemd service file `/etc/systemd/system/engram-harvester.service`:

```ini
[Unit]
Description=Engram Harvester Service
After=network.target

[Service]
Type=oneshot
Environment="ENGRAM_HARVESTER_TOKEN=egm_XXXX..."
ExecStart=/usr/local/bin/engram-harvester --manifest /etc/engram/sources.yaml --addr localhost:7070
User=engram
StandardOutput=journal
StandardError=journal
```

Create a systemd timer file `/etc/systemd/system/engram-harvester.timer`:

```ini
[Unit]
Description=Run Engram Harvester Nightly

[Timer]
OnCalendar=*-*-* 02:00:00
Persistent=true

[Install]
WantedBy=timers.target
```

Enable and start the timer:

```bash
systemctl daemon-reload
systemctl enable --now engram-harvester.timer
```

## Kaggle Backfill Runbook

When setting up Engram for the first time, a full backfill of historical arXiv documents is recommended using the Kaggle metadata dump (~1.7M total papers, ~904k filtered).

### Steps:
1. **Download Snapshot**: Download the arXiv Dataset from Kaggle (e.g. `arxiv-metadata-oai-snapshot.json.gz`).
2. **Update Manifest**: Configure the manifest to target the downloaded file.
3. **Execute Backfill**: Run the harvester using the `--backfill` alias for documentation clarity, restricting to the `arxiv-kaggle` source:
   ```bash
   ENGRAM_HARVESTER_TOKEN=egm_XXXX... engram-harvester \
     --manifest sources.yaml \
     --source arxiv-kaggle \
     --batch-size 1000 \
     --timeout 12h
   ```
4. **Monitor Progress**: The harvester logs periodic batch ingestion reports (slog format) to stderr.

## Correctness & Important Notes

- **Metadata Only (No PDFs)**: The harvester does not fetch or index PDF files or full-text articles. It parses and indexes metadata records only (abstracts, authors, titles, DOIs).
- **Mark-and-Sweep Deletion**:
  - Full-harvest sources (`arxiv-kaggle`, `github-repos`, `web-crawl`) utilize a **mark-and-sweep** mechanism.
  - Documents uploaded during a run are tagged with a unique `harvest_id`. At the end of a successful run, any documents in the target collection that belong to that source but do not match the current `harvest_id` are swept away.
  - If a full-harvest run fails mid-way, the sweep is bypassed, ensuring that partial failures do not wipe out valid historical records.
- **Incremental Additive-Only Behavior (`arxiv-oaipmh`)**:
  - The `arxiv-oaipmh` source is additive/incremental and **never deletes**.
  - Any arXiv record marked with `<status>deleted</status>` will be skipped and logged. This is a known v1 limitation because Engram's API does not expose a per-document-id delete endpoint.
- **Politeness & Rate Limits**:
  - The web crawler and the dynamic OAI-PMH interfaces enforce polite delay intervals (e.g. `delay: "1s"`) to avoid overloading upstream providers. Under politeness parameters, the large ~904k backfill may take several hours. Do not attempt to run multiple crawlers concurrently against the same host.
