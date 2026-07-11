# Engram Harvester — Layer-2 Crawler & Ingestion Tool

The `engram-harvester` is a Layer-2 document harvester designed to pull metadata and content from external document sources and feed it into Engram's knowledge platform using the `knowledge_*` gRPC APIs. 

Engram's server (`engram-server`) itself is entirely passive; it handles storage, search, and memory reconciliation, but **never crawls or initiates pulls**. The `engram-harvester` runs as a separate, one-shot process (e.g., triggered by cron or systemd timers) that contacts the gRPC API of `engram-server`.

## Manifest Schema

The harvester is configured using a YAML manifest file. A manifest maps a set of `collections` to their respective document `sources`.

### Example `sources.yaml`

```yaml
collections:
  - name: general-knowledge
    sources:
      - type: arxiv-kaggle
        path: "/data/arxiv-metadata-oai-snapshot.json.gz"
        filter: "cs.*"
      - type: arxiv-oaipmh
        endpoint: "http://export.arxiv.org/oai2"
        set: "cs"
        from: "2026-07-01"
      - type: github-repos
        owner: "google"
        repo: "engram"
        branch: "main"
        includes:
          - "*.md"
          - "*.go"
      - type: web-crawl
        seeds:
          - "https://example.com"
        max_depth: 3
        max_pages: 1000
        delay: "1s"
```

### Source Type Config Keys

1. **`arxiv-kaggle`** (Full Harvest)
   - Parses local arXiv metadata dump files.
   - Config keys:
     - `path` (string, required): Absolute or relative path to the `.json.gz` or `.json` metadata file.
     - `filter` (string, optional): Regex matching category names (defaults to `cs.*`).

2. **`arxiv-oaipmh`** (Incremental)
   - Harvests metadata dynamically via the arXiv OAI-PMH HTTP interface.
   - Config keys:
     - `endpoint` (string, required): Base URL of the OAI-PMH service.
     - `set` (string, optional): The arXiv category/set (e.g. `cs`).
     - `from` (string, optional): Start date in `YYYY-MM-DD` format.

3. **`github-repos`** (Full Harvest)
   - Clones a remote repository and indexes text files.
   - Config keys:
     - `owner` (string, required): GitHub repository owner.
     - `repo` (string, required): Repository name.
     - `branch` (string, optional): Branch to index (defaults to the default branch).
     - `includes` (array of strings, optional): Glob patterns for files to include.

4. **`web-crawl`** (Full Harvest)
   - Crawls a target site's HTML pages.
   - Config keys:
     - `seeds` (array of strings, required): Entry point URLs.
     - `max_depth` (int, optional): Maximum link depth.
     - `max_pages` (int, optional): Maximum number of pages to download.
     - `delay` (duration, optional): Request spacing/delay (e.g. `1s`).

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
