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
`repo, path`; `web-crawl` emits `url`; `markdown-dir` emits `brain, category,
note_type, date, path, description` (every one but `brain` and `path` is
omitted when the note's frontmatter does not carry it).

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
        repos:
          - facebook/react-native-website
          - { repo: cloudflare/cloudflare-docs, branch: production }
          - { repo: some/monorepo, branch: main, subdir: docs/reference }
        files: ["docs/**/*.md", "src/content/docs/**/*.mdx"]

  - name: docs-sites                  # text_field: body ; keyword field: url
    sources:
      - { type: web-crawl, seeds: ["https://docs.astral.sh/uv/"], max_pages: 500 }

  - name: notes                       # must declare: brain, category, note_type,
    sources:                          # path, description (keyword-ish) and date (date)
      # One entry may list several note directories; each root is swept on its own.
      - type: markdown-dir
        roots:
          - ~/brains/self             # brain defaults to the base name ("self")
          - { path: /srv/notes/work, brain: work }
        exclude: [".trash/**", "recall.md", "README.md"]
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
   - `repos` (**required**) is an array whose entries may be either:
     - an `owner/repo` string, which shallow-clones the default branch and whole
       repository (the backward-compatible form); or
     - an object with `repo` (string, required), `branch` (string, optional), and
       `subdir` (repo-relative directory, optional). `branch` selects the cloned
       ref. `subdir` uses a partial sparse clone when supported and limits the
       walk to that subtree; emitted IDs and paths retain the subdirectory prefix.
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

5. **`markdown-dir`** (Full Harvest — sweeps)
   - Walks one or more LOCAL directories of markdown notes and emits one doc per
     file (`id = <brain>/<path relative to the root>`,
     `source_version = sha256:<first 16 hex of the file's content hash>` — there
     is no commit SHA for a local directory). YAML frontmatter is parsed into the
     collection's fields: `name` becomes the title (falling back to the relative
     path), `type` becomes `note_type`, and `date`/`description` are carried
     through; `category` is the note's containing top-level subdirectory. The
     frontmatter block is stripped from the indexed text. Symlinks and `.git` are
     skipped. Malformed frontmatter never fails the run; how it degrades depends
     on whether the block's extent is knowable:
     - **No block, or an unterminated one** (no leading `---`, or no closing
       `---`/`...`): there is nothing to strip, so the WHOLE file is the text and
       the relative path is the title. No fields are recovered.
     - **A correctly delimited block whose YAML does not parse**: the extent IS
       known, so the text is the content AFTER the block, exactly as on the
       successful path — the `---` fence and its YAML are never indexed as prose.
       Fields are then recovered by a lenient line scan instead of being lost:
       - Only `name`, `type`, `date` and `description` are recovered (the keys
         the collection actually uses); any other key is ignored.
       - A line counts only if it starts at column 0. An indented line is
         spillover from the previous key's mangled value, not a field.
       - The value is everything after the FIRST colon, trimmed, further colons
         included — `name: Raw research: Managed Agents` recovers whole.
       - One layer of MATCHING surrounding quotes is stripped. Unbalanced,
         mismatched or lone quote characters are kept verbatim rather than
         half-stripped or dropped.
       - A repeated key keeps the FIRST occurrence with a NON-EMPTY value (this
         diverges from YAML, which keeps the last: in a block already known to
         be broken, a repeat is usually debris from the first value rather than
         a real reassignment). An empty value never claims the key, so a bare
         `type:` above a real `type: gotcha` does not swallow the usable value.
       - A recovered `date` goes through the same normalization as a parsed one:
         emitted as a `YYYY-MM-DD` string, or omitted if it will not parse.
       A recovery is logged at INFO with the number of keys salvaged; a block
       from which nothing could be recovered is logged at WARN, as before.
   - `roots` (**required**) is an array whose entries may be either:
     - a directory-path string, whose `brain` defaults to the directory's base
       name; or
     - an object with `path` (string, required) and `brain` (string, optional).
     A leading `~` IS expanded (this is the only place in the harvester that does
     so); relative paths are resolved against the process working directory, so
     prefer absolute ones. A root that does not exist **fails the run** rather
     than harvesting nothing — an empty harvest would let the sweep delete every
     document that root had contributed.
   - `files` (array of globs, optional, default `["**/*.md"]`): supports `**`,
     matched against the root-relative path, same semantics as `github-repos`.
   - `exclude` (array of globs, optional, default none): matched against the
     root-relative path; a pattern ending in `/**` excludes that directory and
     everything beneath it at any depth (and prunes the walk). Note that
     `README.md` excludes only the root-level file — use `**/README.md` for all.
   - `max_file_bytes` (int, optional, default `1048576`): larger/binary files skipped.
   - Each root gets its OWN mark-and-sweep scope (`markdown-dir:<absolute path>`),
     so harvesting one brain never sweeps another brain's documents. Two entries
     pointing at the same directory collapse to one scope — list a directory once.

### Multi-repo & sweep scope (important)

A Full-Harvest source's mark-and-sweep mechanism deletes every row for
`(collection, source_type)` that the current run did **not** re-ingest. With the
per-repo sweep scoping feature (shipped in commit `ebfe8d2`), each repo configured
in a `github-repos` entry gets its **own mark-and-sweep scope**. This means
harvesting `repoA` in one `engram-harvester` run and `repoB` in a separate run
will **not** delete the other repo's documents — each repo is swept independently.

While you can configure multiple repos in a single `github-repos` entry (`repos: [repoA, repoB]`)
for efficiency, it is no longer a correctness requirement. Each repo maintains its
own scope based on its owner/repo identifier, making independent harvest runs safe.

## Running the Harvester

The harvester requires a valid gRPC authentication token. To protect sensitive credentials, the token **MUST** be supplied via the `ENGRAM_HARVESTER_TOKEN` environment variable. The harvester will reject running if the token is passed in flags/arguments, or if it is empty.

```bash
export ENGRAM_HARVESTER_TOKEN=egm_REPLACE_WITH_YOUR_HARVESTER_TOKEN
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
