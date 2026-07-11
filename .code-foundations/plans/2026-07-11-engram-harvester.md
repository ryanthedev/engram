# Plan: Engram Harvester (Plan 2 of 2)

**Created:** 2026-07-11
**Status:** ready
**Complexity:** complex
---
## Context

Plan 1 shipped engram's knowledge platform (collection registry, `KnowledgeStore`, `KnowledgeRetriever`, six `knowledge_*` gRPC/MCP ops, per-collection RBAC) — but nothing fills the collections. This plan builds the **Layer-2 harvester**: the crawl-side tool that reads a git-versioned sources manifest, fetches from typed sources, and feeds engram via `knowledge_ingest` / `knowledge_delete`.

Source of intent: `.code-foundations/research/2026-07-09-engram-knowledge-collection.md` (confirmed). Built against the frozen Plan 1 API (`.code-foundations/plans/2026-07-10-engram-knowledge-platform.md`, complete/merged): the harvester is a Go gRPC client of `internal/engramclient` (`Dial`, `KnowledgeIngest`, `KnowledgeDelete`, `KnowledgeCollections`, `CreateCollection`), consuming `mcp.KnowledgeDoc{ID,Title,Text,SourceVersion,Fields}`.

## Constraints
- **Dedicated Go tool in the engram repo** — `cmd/engram-harvester` + `internal/harvester/**`; talks to engram over gRPC via `internal/engramclient` with a **harvester-role token**. engram-server never crawls (crawl/index boundary holds).
- **No PDFs / full text ever** — arXiv *metadata* only; enforced in the arXiv sources.
- **Provenance on every doc** — stamp `source_version` (commit SHA / dump date / crawl ts); `harvested_at` is server-set (never client-trusted).
- **Mark-and-sweep for full harvests** — stamp one `harvest_id` per run, then `KnowledgeDelete` the not-current rows for that collection+source (those whose `harvest_id != currentHarvestID`, per Plan 1's predicate). A partial/failed run must sweep NOTHING (fail-safe). **Incremental git & OAI-PMH carry explicit per-file/per-record deletes**, not a full sweep; OAI-PMH honors `status="deleted"`.
- **New source type = a new `Source` behind one interface** (OCP); never touches engram or the manifest loader.
- **Reuse engram house style** — raw stdlib `net/http`, `map[string]any`, `go.yaml.in/yaml/v2` (as `internal/knowledge/seed.go`), stdlib `testing` (no testify), sentinel errors returned unwrapped so `errors.Is` survives.
- **Validate every caller-supplied name before it enters a REST/gRPC path or a subprocess argv** (engram path-traversal choke-point lesson + command-injection): collection/source names, repo `owner/repo`, crawl host. Defense-in-depth even though engram re-validates server-side.
- **Execution mix (user directive):** the three typed-source phases (3, 4, 5) are scoped, frozen-contract "implement one `Source`" units — built via **sdd** (`agy` implement → `codex` review). The seam/orchestration/integration phases (1, 2, 6) run in the main `/code-foundations:build` flow.

## Chosen Approach
**A — Stateless one-shot CLI + local state file, cron-driven.** `engram-harvester --once` runs and exits; an external scheduler (cron/systemd timer) drives cadence. The only durable state is a local JSON file holding per-repo git last-SHA; OAI-PMH uses a small look-back window so cursor loss just re-fetches a cheap, idempotent overlap (upsert-by-id dedups). Typed sources plug in behind a `Source` interface via a `SourceFactory` registry (OCP). **Rationale:** simplest lifecycle, matches the `cmd/*` one-shot idiom and cron/CI scheduling, no daemon to babysit, near-zero durable state; a lost cursor self-heals via a full re-harvest + mark-and-sweep. **Fallback:** Approach C (persist cursors in an engram-backed `_harvest_state` collection) if harvests later run from ephemeral CI needing durable, shareable state.

## Rejected Approaches
- **B — Long-running daemon + internal scheduler:** adds a persistent process, lifecycle/observability surface, and still host-local state — overkill for a nightly job; a crashed daemon fails silently where a missed cron run is visible via the staleness signal.
- **C — Engram-backed state collection:** durable/shareable and dogfoods the platform, but adds an extra collection + network round-trips for state in v1 for a durability benefit a single cron host doesn't need. Kept as the fallback.

---
## Implementation Phases

### Phase 1: Harvester core — manifest, seams, engram client
**Model:** fable
**Skills:** aposd-designing-deep-modules, cc-defensive-programming
**Gate:** Full
**Security-sensitive:** yes
**Depends on:** none | **Unlocks:** Phase 2, 3, 4, 5, 6
**File scope:** `internal/harvester/manifest.go, internal/harvester/source.go, internal/harvester/engram.go, internal/harvester/manifest_test.go, internal/harvester/source_test.go, internal/harvester/engram_test.go`

**Goal:** Define the Layer-2 manifest schema + loader and freeze the seams every downstream phase implements against — the `Source`/`Sink`/`StateStore` interfaces (incl. the typed delete-mode and dependency injection), the `SourceFactory` registry, and an `EngramClient` wrapper over `engramclient`.

**Scope:**
- IN: `Manifest{Collections:[{Name, Sources:[SourceConfig{Type string, Raw map[string]any}]}]}`; `LoadManifest([]byte)(Manifest,error)` (YAML via `go.yaml.in/yaml/v2`); `Validate(ctx, EngramClient)error` (each collection Name matches a live Layer-1 registration via `KnowledgeCollections`; every source `Type` is registered; names path-safe); `Source interface{ Type()string; Mode()HarvestMode; Harvest(ctx, Sink)error }` with `HarvestMode ∈ {FullHarvest, Incremental}` — the Runner reads `Mode()` to decide sweep vs explicit-delete, so **the delete-mode contract is frozen HERE, not P2**; `Sink interface{ Add(mcp.KnowledgeDoc)error; Delete(id string)error; Flush(ctx)error }`; `StateStore interface{ LastSHA(repo string)(string,bool); SetLastSHA(repo,sha string)error }`; `SourceFactory` registry `Register(type string, func(SourceConfig, Deps)(Source,error))` + `Build(SourceConfig, Deps)`, where `Deps{State StateStore}` injects the runtime deps a source needs (P4 uses `State`; P3/P5 ignore it) — this keeps the frozen factory signature sufficient without a post-freeze widening; `EngramClient` seam wrapping `engramclient.Dial(addr,token)`.
- OUT: any concrete source (P3-5), the batching/sweep engine (P2), the local-file `StateStore` impl (P2), CLI (P6).

**Constraints:** Manifest is external input — validate at the barricade (unknown source type → error naming valid types; collection not registered → error; name failing `^[a-z0-9][a-z0-9_-]*$` → reject before any REST/gRPC call). Type-specific config validation is pushed DOWN into each source's factory, not the loader (OCP — loader stays generic). Deep, minimal seams (design-it-twice): `Sink` hides batching + provenance; `EngramClient` hides gRPC; `Deps` hides which sources need state.
**Edge cases:** empty/malformed YAML; duplicate collection name; source Type unknown; collection name absent from engram; `Sink.Delete` on a non-existent id (no-op, not error).
**Produces:** `harvester.Manifest` + `LoadManifest`/`Validate`; `Source`/`Sink`/`StateStore` interfaces; `HarvestMode` enum; `SourceFactory{Register,Build}` + `Deps{State StateStore}`; `EngramClient` seam. **Contract (frozen here):** `Source{Type()string; Mode()HarvestMode; Harvest(ctx,Sink)error}`, `Sink{Add(mcp.KnowledgeDoc)error; Delete(string)error; Flush(ctx)error}`, `StateStore{LastSHA(string)(string,bool); SetLastSHA(string,string)error}`, factory `func(SourceConfig, Deps)(Source,error)`, `Deps{State StateStore}`, and `SourceConfig{Type,Raw}`. P3-5 implement `Source` + self-register a factory; P2 implements `Sink`+`StateStore`+the Runner that reads `Mode()`; P6 consumes all.

**Approach notes:** `SourceConfig.Raw map[string]any` (not per-type structs in the loader) is the OCP seam — adding a source type never edits the loader. `Deps` injection + `Mode()` are the deliberate choice that keeps the frozen `Source`/factory contract sufficient for P4's state need and P2's sweep decision. User chose a stateless CLI (Approach A), so no daemon/scheduler types.

**Done when:**
- [ ] DW-1.1: a valid manifest round-trips through `LoadManifest`; `Validate` passes when every collection is registered in engram and every source Type is known.
- [ ] DW-1.2: `Validate` rejects (a) an unregistered collection, (b) an unknown source Type — each error naming what was expected; (c) a name failing the path-safe pattern, before any REST/gRPC call.
- [ ] DW-1.3: `SourceFactory.Build` constructs a registered source from its `SourceConfig`+`Deps` and errors on an unregistered Type; `Source` (incl. `Mode()`)/`Sink`/`StateStore`/`EngramClient` interfaces compile and are driven by fakes in tests.

**Difficulty:** HIGH
**Uncertainty:** whether `EngramClient` needs `CreateCollection` in v1 (collection bootstrap) or assumes collections pre-exist — resolve in DETAIL by checking whether the manifest should provision. Default: assume pre-existing (admin creates via Plan 1 RPC); harvester only validates.

### Phase 2: Harvest engine — orchestration, mark-and-sweep, state
**Model:** fable
**Skills:** aposd-designing-deep-modules, cc-defensive-programming
**Gate:** Full
**Security-sensitive:** yes
**Depends on:** Phase 1 | **Unlocks:** Phase 3, 4, 5, 6
**File scope:** `internal/harvester/runner.go, internal/harvester/sink.go, internal/harvester/state.go, internal/harvester/runner_test.go, internal/harvester/sink_test.go, internal/harvester/state_test.go`

**Goal:** The run engine that drives one `Source` end-to-end: assign a `harvest_id`, batch docs through a `Sink` into bounded `KnowledgeIngest` calls with provenance, then run the correct deletion mode (from `source.Mode()`) fail-safely, backed by a local-file `StateStore`.

**Scope:**
- IN: `Runner.Run(ctx, collection string, source Source)(Report,error)` — mints one `harvest_id` per run (RFC3339 timestamp + source type, unique per run), drives `source.Harvest(sink)`, and on clean completion of a `FullHarvest` source performs the not-current sweep via `KnowledgeDelete(collection, sourceType, harvestID)`; a batching `Sink` impl (bounded `_bulk`-size batches to `KnowledgeIngest`, stamps `source_version`, surfaces per-item failures — no silent success); `fileStateStore` (JSON file: `LastSHA`/`SetLastSHA`, atomic temp+rename write, missing/corrupt file → empty state not error); staleness read helper over `KnowledgeCollections`. The Runner consumes the frozen `HarvestMode` from P1; `Incremental` sources never sweep.
- OUT: concrete sources (P3-5); CLI wiring (P6).

**Constraints:** **Fail-safe sweep** — the not-current delete runs ONLY after a `FullHarvest` `source.Harvest` returns nil AND every ingest/`Flush` succeeded; ANY error (from `Harvest`, a partial `_bulk`, or `Flush`) aborts before the sweep, so a partial run never deletes live rows (correctness over robustness — this is a delete path). `harvested_at` is server-set; never send it. Batch size bounded + politeness (lowered pressure during backfill) per research doc. Partial `_bulk` errors are reported, not swallowed (aposd Silent-Failure red flag). State-file writes atomic (temp+rename) to survive a crash mid-write.
**Edge cases:** zero docs harvested on a `FullHarvest` → skip the sweep entirely (never delete a collection to zero); `KnowledgeIngest` partial failure → suppress sweep; context cancellation mid-batch (flush-then-abort, no sweep); corrupt/absent state file; `Delete` of an id not present (no-op).
**Produces:** `Runner.Run(ctx, collection, source)(Report,error)`; `Sink` impl (batching + provenance); `fileStateStore`. **Contract:** `Run` signature + `Report{Indexed,Deleted int, HarvestID string}` consumed by P6; `Sink`/`StateStore` impls consumed by P3-5 at runtime (injected via P1's `Deps`). The delete-mode contract itself is frozen in P1 (`HarvestMode`), not here.
**Rollback:** the sweep hard-deletes rows via `KnowledgeDelete` — irreversible in place; compensating action is a full re-harvest of that collection+source (upsert-by-id restores current docs). Guarded by fail-safe (no sweep on ANY error) + the empty-harvest guard (DW-2.5).

**Approach notes:** Stateless one-shot (Approach A) — `fileStateStore` path is a CLI flag; no engram-backed state in v1. Sweep predicate mirrors Plan 1: `KnowledgeDelete(collection, source, currentHarvestID)` deletes rows whose `harvest_id != currentHarvestID`.

**Done when:**
- [ ] DW-2.1: `Run` mints one `harvest_id`, batches N docs through `Sink` into `KnowledgeIngest` (bounded batch size) stamping `source_version`, and returns `Report{Indexed=N}`; zero embedding involvement (harvester never touches the embedder).
- [ ] DW-2.2: on clean completion of a `FullHarvest` source, `Run` calls `KnowledgeDelete(collection, source, harvestID)` (not-current sweep); on ANY error — `Harvest` error, partial `_bulk`, or `Flush` failure — it returns the error and issues NO delete (fail-safe, asserted); an `Incremental` source never sweeps.
- [ ] DW-2.3: a `_bulk`/ingest partial failure is surfaced in the error/report, not reported as full success (and therefore suppresses the sweep per DW-2.2).
- [ ] DW-2.4: `fileStateStore` round-trips `LastSHA`/`SetLastSHA` across process restarts; a missing or corrupt file yields empty state (no crash); writes are atomic.
- [ ] DW-2.5: an empty (zero-doc) successful `FullHarvest` does NOT sweep the collection to zero (guard fires unconditionally).
- [ ] DW-2.6: `harvest_id` follows the documented format (RFC3339 ts + source type) and two consecutive `Run`s mint distinct ids (uniqueness — the not-current sweep predicate is load-bearing on it).

**Difficulty:** HIGH
**Uncertainty:** none material — the sweep predicate is confirmed as `KnowledgeDelete(collection, source, currentHarvestID)` in `engramclient.knowledge.go` (deletes rows whose `harvest_id != currentHarvestID`); the Runner passes `currentHarvestID`.

### Phase 3: arXiv sources — Kaggle backfill + OAI-PMH incremental
**Model:** fable
**Skills:** cc-defensive-programming, cc-control-flow-quality
**Gate:** Full
**Security-sensitive:** yes
**Depends on:** Phase 1, 2 | **Unlocks:** Phase 6
**File scope:** `internal/harvester/sources/arxiv_kaggle.go, internal/harvester/sources/arxiv_oaipmh.go, internal/harvester/sources/arxiv_record.go, internal/harvester/sources/arxiv_kaggle_test.go, internal/harvester/sources/arxiv_oaipmh_test.go`

**Goal:** The two v1 shipping sources: `arxiv-kaggle` (one-time full backfill from the local gzipped metadata dump) and `arxiv-oaipmh` (nightly incremental), both mapping arXiv metadata → `mcp.KnowledgeDoc` with a shared record mapper, no PDFs.

**Scope:**
- IN: `arxiv-kaggle` — stream-parse the local `*.json.gz` dump (line-delimited JSON, constant memory), filter `categories` to `cs.*`, emit one doc per paper (id=`arxiv_id`, text=abstract, fields=categories/published_date/update_date/doi/journal-ref/comments), full-harvest (drives P2 mark-and-sweep). `arxiv-oaipmh` — `GET oaipmh.arxiv.org` `verb=ListRecords`, `set=cs`, `from=<now−lookback>`, `metadataPrefix=arXivRaw`, follow `resumptionToken` to completion, honor `header@status="deleted"` (→ `Sink.Delete(id)`), incremental (no full sweep). Shared `arxiv_record.go` mapper + `cs.*` filter. Both self-register a `SourceFactory`.
- OUT: engram write mechanics (P2 Sink); scheduling (P6); PDF/full-text (forbidden).

**Constraints:** **No PDF/full-text** — store only metadata fields; assert no PDF fetch. XML parsing is untrusted deserialization → **disable external entity/DTD resolution (XXE)**. Bound the resumption-token loop (detect a repeated/empty token → stop; cap iterations) to prevent an infinite loop on a misbehaving endpoint. Malformed single record → skip+log and continue (robustness for the batch); malformed dump structure → abort (correctness — don't ingest garbage that a full sweep then deletes-around). OAI-PMH politeness: honor `503 Retry-After`, serial requests. Look-back window makes a lost/absent cursor a cheap idempotent re-fetch (upsert-by-id dedups).
**Edge cases:** empty `ListRecords` (no new papers) → zero docs, no error, no delete; `resumptionToken` expiry mid-run → surface error (no sweep, P2 handles); `403/503` from arXiv → retry/backoff then fail cleanly; a paper with no `cs.*` category filtered out; gzip truncation → abort.
**Produces:** two `Source` impls registered as `arxiv-kaggle` / `arxiv-oaipmh`; shared arXiv record→`KnowledgeDoc` mapper; `arxiv-kaggle` reports `Mode()=FullHarvest`, `arxiv-oaipmh` reports `Incremental`. **Contract:** consumed by P6 via blank-import registration; no new exported types beyond factory registration.
**Execution:** sdd — `agy` implements against the frozen P1/P2 seams; `codex-high` reviews (security-sensitive: XXE, resumption-loop bound, no-PDF). Main thread owns the diff + commit.

**Approach notes:** Look-back window (not a persisted OAI cursor) is the user-chosen Approach-A simplification — cursor loss self-heals via idempotent overlap. `metadataPrefix=arXivRaw` gives full metadata incl. all versions; confirm field availability in DETAIL against the live endpoint.

**Done when:**
- [ ] DW-3.1: `arxiv-kaggle` streams a sample gzipped dump, filters to `cs.*`, and emits one correctly-mapped `KnowledgeDoc` per paper (id/title/abstract/fields) with constant memory; issues zero PDF fetches.
- [ ] DW-3.2: `arxiv-oaipmh` parses a `ListRecords` response, follows `resumptionToken` across pages to completion, and maps records identically to Kaggle via the shared mapper.
- [ ] DW-3.3: an OAI `header status="deleted"` record produces `Sink.Delete(id)`, not an upsert.
- [ ] DW-3.4: XML parsing rejects/ignores external entities (XXE-safe); a repeated/empty resumption token terminates the loop (no infinite loop).
- [ ] DW-3.5: a single malformed record is skipped-and-logged; a malformed dump/gzip structure aborts the harvest (no partial sweep).
- [ ] DW-3.6: `arxiv-oaipmh` is polite — requests are serial, a `503 Retry-After` is honored (backoff then retry), and a mid-run `resumptionToken` expiry surfaces as an error (no partial sweep).

**Difficulty:** HIGH
**Uncertainty:** exact `oaipmh.arxiv.org` schema/field names under `arXivRaw` and its `status="deleted"` header shape — verify against the live endpoint in DETAIL/BUILD; fallback `metadataPrefix=oai_dc` (leaner fields) if `arXivRaw` is unavailable.

### Phase 4: github-repos source
**Model:** fable
**Skills:** cc-defensive-programming
**Gate:** Full
**Security-sensitive:** yes
**Depends on:** Phase 1, 2 | **Unlocks:** Phase 6
**File scope:** `internal/harvester/sources/github_repos.go, internal/harvester/sources/github_repos_test.go`

**Goal:** A `github-repos` source that ingests matched files (READMEs/docs) one doc per file, using the local `git` CLI and per-repo last-SHA diffing for cheap incremental refresh with explicit per-file deletes.

**Scope:**
- IN: for each `repos:[owner/repo]` with `files:[globs]` — shallow-clone/fetch via the `git` CLI (argv, never `sh -c`); on first run ingest all matched files (full), on later runs `git diff --name-status <lastSHA> HEAD` and ingest Added/Modified, `Sink.Delete` Deleted; id=`repo+path`, text=file body, `source_version`=commit SHA; persist new HEAD via `StateStore.SetLastSHA`. Self-register factory `github-repos`.
- OUT: GitHub API auth flows (public repos via git over https in v1); engram mechanics (P2).

**Constraints:** **Command-injection barricade** — validate every `owner/repo` against `^[\w.-]+/[\w.-]+$` and every glob before it reaches a `git` argv; pass args as an argv slice (`exec.CommandContext`), never a shell string; run in a controlled temp dir. Untrusted repo content is data, not code — never execute it. Bound file size read into memory. This is incremental (per-file deletes), NOT a full sweep.
**Edge cases:** repo unreachable/clone fails → error, no partial state write; `lastSHA` no longer in history (force-push) → fall back to full re-ingest; binary/oversized file → skip+log; empty diff → no-op; glob matching zero files → warn.
**Produces:** a `Source` impl registered as `github-repos` (reports `Mode()=Incremental`); receives its `StateStore` via P1's `Deps`. **Contract:** blank-import registration consumed by P6.
**Rollback:** emits per-file `Sink.Delete` (hard-deletes those ids); compensating action is a full re-ingest of the repo. No full sweep, so blast radius is the changed files only.
**Execution:** sdd — `agy` implements; `codex-high` reviews (security-sensitive: shell-out/command-injection). Main thread owns the diff + commit.

**Done when:**
- [ ] DW-4.1: first harvest of a repo ingests all glob-matched files, one doc per file (id=`repo+path`, `source_version`=HEAD SHA), and persists HEAD to the `StateStore`.
- [ ] DW-4.2: a second harvest after commits ingests only Added/Modified files and issues `Sink.Delete` for Deleted files (SHA-diff incremental), then updates the stored SHA.
- [ ] DW-4.3: a malicious `owner/repo` (e.g. containing `;`, `..`, or a flag-like `--upload-pack`) is rejected before any `git` invocation; git is always invoked via argv, never a shell string.
- [ ] DW-4.4: a stored SHA absent from history (force-push) falls back to a full re-ingest rather than erroring.

**Difficulty:** MEDIUM
**Uncertainty:** clone-everything vs sparse/partial checkout for large repos — default to shallow clone + glob filter; optimize only if a real repo is painful.

### Phase 5: web-crawl source
**Model:** fable
**Skills:** cc-defensive-programming, cc-control-flow-quality
**Gate:** Full
**Security-sensitive:** yes
**Depends on:** Phase 1, 2 | **Unlocks:** Phase 6
**File scope:** `internal/harvester/sources/web_crawl.go, internal/harvester/sources/web_crawl_test.go`

**Goal:** A `web-crawl` source that does a bounded, polite BFS crawl from seed URLs, extracts page text, and ingests one doc per page as a full harvest (drives mark-and-sweep).

**Scope:**
- IN: from `seeds:[url]` with `max_pages:N` — BFS same-host crawl bounded by `max_pages`, fetch via stdlib `net/http`, extract title + readable text from HTML, id=canonical URL, text=page text, `source_version`=crawl timestamp; full-harvest (P2 sweep removes pages that 404/disappear). Politeness: per-host delay, honor `robots.txt` disallow, cap depth. Self-register factory `web-crawl`.
- OUT: JS rendering / headless browser; auth'd sites; engram mechanics (P2).

**Constraints:** **SSRF barricade** — a crawl URL is untrusted; restrict to the seed's host (no cross-host hops in v1), and block requests to private/loopback/link-local IP ranges after DNS resolution. Hard `max_pages` + visited-set so the frontier loop always terminates (cc-control-flow-quality: bounded loop, no unbounded recursion). Bound per-page body size; timeout per request. Untrusted HTML is parsed as data. Full-harvest → mark-and-sweep, so a shrunk site prunes correctly.
**Edge cases:** redirect loop / cycle (visited-set dedup); non-HTML content-type → skip; page over size cap → truncate+log; seed unreachable → error (no partial sweep); `robots.txt` disallow-all → zero pages (no sweep-to-zero — P2 empty guard); relative/malformed links → resolve or skip.
**Produces:** a `Source` impl registered as `web-crawl` (reports `Mode()=FullHarvest`). **Contract:** blank-import registration consumed by P6.
**Rollback:** full-harvest source; its sweep is P2's guarded `KnowledgeDelete` — compensating action is a re-crawl. Empty-crawl guard (P2 DW-2.5) prevents a robots-blocked run from wiping the collection.
**Execution:** sdd — `agy` implements; `codex-high` reviews (security-sensitive: SSRF, unbounded-crawl). Main thread owns the diff + commit.

**Approach notes:** Same-host-only + private-IP block is the v1 SSRF stance; cross-host crawling is deliberately out.

**Done when:**
- [ ] DW-5.1: a crawl from a seed over a small fake site ingests one doc per reachable page (id=canonical URL, title+text extracted), bounded by `max_pages`.
- [ ] DW-5.2: the crawl stays on the seed host and refuses a URL resolving to a private/loopback/link-local address (SSRF block).
- [ ] DW-5.3: `max_pages` and the visited-set bound the crawl — a cyclic link graph terminates and never exceeds the cap.
- [ ] DW-5.4: `robots.txt` disallow is honored; a non-HTML content-type is skipped.

**Difficulty:** MEDIUM
**Uncertainty:** HTML→text extraction fidelity with stdlib `golang.org/x/net/html` (already an indirect dep) vs a new dep — default to `x/net/html`; no new dep unless extraction is unacceptable.

### Phase 6: CLI, wiring & end-to-end
**Model:** fable
**Skills:** cc-defensive-programming
**Gate:** Full
**Security-sensitive:** yes
**Depends on:** Phase 1, 2, 3, 4, 5 | **Unlocks:** —
**File scope:** `cmd/engram-harvester/main.go, internal/harvester/run.go, internal/harvester/run_test.go, docs/harvester.md, Makefile`

**Goal:** Wire manifest → runner → registered sources into the `engram-harvester` binary, driven by flags, and prove the whole thing end-to-end against a live cluster with scheduling docs.

**Scope:**
- IN: `cmd/engram-harvester/main.go` (flags `--manifest`, `--collection`, `--source`, `--once`, `--backfill`, `--addr`, `--state`, timeouts; token from `ENGRAM_HARVESTER_TOKEN` env, NOT argv); blank-import the source packages so factories register; a top-level `Run` that loads+validates the manifest, dials engram, and drives the `Runner` over selected collection/source(s); exit codes (0 ok / non-zero on any source failure); `docs/harvester.md` (manifest format, nightly cron/systemd-timer example, backfill runbook, no-PDF/politeness notes); a `Makefile` build target.
- OUT: new source types; engram-side changes.

**Constraints:** **Token is a secret** — read from env, never a flag/argv (avoid ps/​shell-history leakage), never logged. Validate flags at the CLI barricade (manifest exists, collection/source known) before dialing. A `--source` selection filters to matching manifest entries. Fail closed on auth error (engram returns `PermissionDenied` for a missing harvester role) with an actionable message.
**Edge cases:** missing/invalid token → clear auth error, non-zero exit; `--collection`/`--source` not in manifest → error listing valid names; partial multi-source run (one source fails) → report per-source, non-zero exit, others still committed (each source's ingest is independent); manifest validation failure → exit before any harvest.
**Produces:** the working `engram-harvester` binary + `docs/harvester.md`. **Contract:** user-facing CLI; terminal deliverable.
**Execution:** main `/code-foundations:build` flow (integration + live-cluster e2e + cross-cutting wiring, secret handling) — not sdd.

**Done when:**
- [ ] DW-6.1: `engram-harvester --manifest <f> --once` loads+validates the manifest, dials engram with the env token, and runs every collection/source (or the `--source`-filtered subset), returning correct exit codes.
- [ ] DW-6.2: the harvester token is read from `ENGRAM_HARVESTER_TOKEN`, never accepted via argv and never logged; a missing/invalid token yields a clear auth error + non-zero exit.
- [ ] DW-6.3: a bad `--collection`/`--source` (not in manifest) errors before dialing, naming valid options.
- [ ] DW-6.4: live end-to-end — against a running cluster, `knowledge_ingest` a small arXiv sample via the harvester, confirm it is searchable via `knowledge_search`, then a re-run + sweep leaves counts correct and staleness (`newest_harvested_at`) advances.
- [ ] DW-6.5: `docs/harvester.md` documents the manifest schema, a nightly cron/systemd example, and the backfill runbook.

**Difficulty:** MEDIUM
**Uncertainty:** which live cluster for e2e — use the dev/scratch `engram-dev-os` (:9200, throwaway), NEVER the `engram-e2e-os` (:9201) live personal memory store. Confirm a harvester-role token can be minted (Plan 2 auth notes: no CLI `--roles` mint flag yet — may need a token seeded into `auth-tokens.json`).

---
## Test Coverage
**Level:** 100% — every done-when item covered, with boundary + dirty tests; every code-touching phase carries ≥1 dirty test. (Matches Plan 1; security-sensitive phases 3/4/5/6 are dirty-heavy where error-path coverage matters most.)

## Test Plan

**Phase 1 — manifest + seams** (Unit)
- [ ] clean: valid manifest round-trips `LoadManifest`; `Validate` passes with all collections registered + source types known (DW-1.1)
- [ ] dirty: unregistered collection → error; unknown source Type → error naming valid types (DW-1.2)
- [ ] dirty: name failing `^[a-z0-9][a-z0-9_-]*$` rejected before any REST/gRPC call (DW-1.2)
- [ ] dirty: empty/malformed YAML → parse error; duplicate collection name rejected (edge)
- [ ] clean: `SourceFactory.Build` constructs a registered source; unregistered Type errors (DW-1.3)

**Phase 2 — engine** (Unit + Integration)
- [ ] clean: `Run` batches N docs → `KnowledgeIngest`, stamps `source_version`, `Report.Indexed=N`, zero embedder calls (DW-2.1)
- [ ] clean: clean `FullHarvest` → not-current `KnowledgeDelete` sweep fires; an `Incremental` source never sweeps (DW-2.2)
- [ ] dirty: `Harvest` error → error returned, NO delete issued (fail-safe assertion) (DW-2.2)
- [ ] dirty: a partial `_bulk`/`Flush` failure suppresses the sweep (no delete) and is surfaced, not reported as success (DW-2.2, DW-2.3)
- [ ] dirty: missing/corrupt state file → empty state, no crash (DW-2.4)
- [ ] boundary: `fileStateStore` round-trips across restart; atomic write (DW-2.4)
- [ ] dirty: empty successful `FullHarvest` does NOT sweep to zero (DW-2.5)
- [ ] clean: two consecutive `Run`s mint distinct, well-formed `harvest_id`s (DW-2.6)
- [ ] boundary: context cancel mid-batch → flush-then-abort, no sweep; `Sink.Delete` of an absent id is a no-op (edge)

**Phase 3 — arXiv sources** (Unit + Integration)
- [ ] clean: `arxiv-kaggle` streams gzipped sample, filters `cs.*`, one mapped doc/paper, constant memory, zero PDF fetches (DW-3.1)
- [ ] clean: `arxiv-oaipmh` follows `resumptionToken` across pages, shared mapper parity with Kaggle (DW-3.2)
- [ ] dirty: OAI `status="deleted"` → `Sink.Delete`, not upsert (DW-3.3)
- [ ] dirty: XML with an external entity is not resolved (XXE-safe) (DW-3.4)
- [ ] dirty: repeated/empty resumption token terminates the loop (no infinite loop) (DW-3.4)
- [ ] dirty: malformed single record skipped+logged; malformed gzip/dump aborts (no partial sweep) (DW-3.5)
- [ ] dirty: `503 Retry-After` honored (serial backoff+retry); `resumptionToken` expiry mid-run → error, no sweep (DW-3.6)
- [ ] boundary: empty `ListRecords` → zero docs, no error, no delete (edge)

**Phase 4 — github-repos** (Unit + Integration)
- [ ] clean: first harvest ingests all matched files (id=`repo+path`, SHA `source_version`), persists HEAD (DW-4.1)
- [ ] clean: second harvest ingests only Added/Modified, `Sink.Delete` for Deleted, updates SHA (DW-4.2)
- [ ] dirty: malicious `owner/repo` (`;`, `..`, `--upload-pack`) rejected before any `git`; argv-only invocation (DW-4.3)
- [ ] dirty: stored SHA absent from history (force-push) → full re-ingest fallback (DW-4.4)
- [ ] dirty: unreachable repo → error, no partial state write; binary/oversized file skipped (edge)
- [ ] boundary: glob matching zero files → warn (no docs, no error); empty diff on re-run → no-op (edge)

**Phase 5 — web-crawl** (Unit + Integration)
- [ ] clean: crawl over a fake site ingests one doc/page, title+text, bounded by `max_pages` (DW-5.1)
- [ ] dirty: off-host URL refused; URL resolving to private/loopback/link-local blocked (SSRF) (DW-5.2)
- [ ] dirty: cyclic link graph terminates, never exceeds `max_pages` (DW-5.3)
- [ ] dirty: `robots.txt` disallow honored; non-HTML content-type skipped (DW-5.4)
- [ ] dirty: seed unreachable → error, no partial sweep (edge)
- [ ] boundary: page over size cap truncated+logged; redirect loop deduped (edge)

**Phase 6 — CLI + e2e** (Integration + Manual)
- [ ] clean: `--manifest --once` loads/validates/dials/runs, correct exit codes (DW-6.1)
- [ ] dirty: token only from env, never argv, never logged; missing/invalid token → auth error + non-zero exit (DW-6.2)
- [ ] dirty: bad `--collection`/`--source` errors before dialing, names valid options (DW-6.3)
- [ ] manual: live e2e on `engram-dev-os` — harvest arXiv sample → `knowledge_search` finds it → re-run+sweep leaves counts correct, staleness advances (DW-6.4)
- [ ] clean: `docs/harvester.md` covers manifest schema + cron example + backfill runbook (DW-6.5)

---
## Assumptions
| Assumption | Confidence | Verify Before Phase | Fallback If Wrong |
|---|---|---|---|
| A harvester-role token can be minted for e2e (Plan 2 left no `--roles` CLI mint flag) | MED | Phase 6 | Seed a role-bearing token directly into `auth-tokens.json` for the dev cluster |
| `oaipmh.arxiv.org` serves `arXivRaw` with the expected fields + `status="deleted"` header | MED | Phase 3 | Fall back to `metadataPrefix=oai_dc` (leaner fields) |
| Collections pre-exist (admin creates via Plan 1 `knowledge_create_collection`); harvester only validates | HIGH | Phase 1 | Add `CreateCollection` to the `EngramClient` seam + a manifest `provision:true` opt-in |
| `sdd` `agy`/`codex` adapters are authenticated (list_adapters shows binaries present; auth unverified) | MED | Phase 3 | `check_adapter(agy)`/`check_adapter(codex-high)` before dispatch; fall back to code-foundations build if unauth |
| `golang.org/x/net/html` (indirect dep) suffices for HTML→text | HIGH | Phase 5 | Promote to a direct dep or add a minimal extractor |
| `KnowledgeDelete(collection, source, currentHarvestID)` implements the not-current sweep predicate (`harvest_id != current`) | HIGH | Phase 2 | Confirmed against `engramclient.knowledge.go`; none needed |

## Decision Log
| Decision | Alternatives Considered | Rationale | Phase |
|---|---|---|---|
| Stateless one-shot CLI + local state file, cron-driven (Approach A) | B daemon+scheduler; C engram-backed state | Simplest lifecycle; near-zero durable state; cursor loss self-heals; missed run visible via staleness | all |
| OAI-PMH look-back window instead of a persisted cursor | Persist a cursor per source | Upsert-by-id makes overlap idempotent; removes a state axis | 3 |
| `SourceConfig{Type,Raw map[string]any}` + factory registry | Per-type structs parsed in the loader | OCP — new source type never edits the loader (research doc's core stance) | 1 |
| Fail-safe sweep (delete only after clean `Harvest` AND all ingests/`Flush` succeed) + empty-harvest guard | Sweep unconditionally after run | A partial/empty run must never delete live rows (correctness on a delete path) | 2 |
| `git` CLI shell-out for github-repos | go-git library; GitHub REST API | No new dep; matches raw-stdlib idiom; SHA-diff is a one-liner | 4 |
| Same-host + private-IP-blocked crawl | Cross-host crawl; headless render | v1 SSRF stance; bounded blast radius | 5 |
| Phases 3/4/5 built via sdd (`agy`+`codex`), 1/2/6 via code-foundations build | All via code-foundations build | User directive; 3/4/5 are scoped frozen-contract "implement one Source" units ideal for sdd | all |

---
## Notes
- **House-style deviation (inherited from Plan 1):** the harvester drives engram's upsert-by-id + hard `KnowledgeDelete` writes — this is the documented knowledge-path exception to engram's append-only rule. Do NOT "fix" toward append-only.
- **Provenance produced here, consumed by staleness:** `source_version` + `harvest_id` are stamped per doc; `newest_harvested_at` on `knowledge_collections` is the stalled-run signal a nightly monitor watches.
- **Cluster hazard:** e2e uses the throwaway `engram-dev-os` (:9200). NEVER point the harvester at `engram-e2e-os` (:9201) — it doubles as the live personal memory store.
- **sdd execution is a build-time strategy, not a plan structure change:** the phase contracts are identical whether built via sdd or code-foundations build; `Execution:` lines record the intended dispatch, `check_adapter` gates it, and the main thread owns every diff + commit. On the sdd phases (3/4/5) the `Model: fable` field is the review-tier / fallback used if that phase is instead run through code-foundations build; `agy` is the implementer under sdd.
- **Leaner-v1 option (not taken):** github-repos (P4) + web-crawl (P5) are the extensibility proof; arXiv (P3) is the only must-ship corpus. They can be dropped for a papers-only v1 without touching P1/P2 — say so at approval if preferred.
- Open items are tracked in Assumptions (token minting, OAI schema) — each has a fallback; none blocks starting Phase 1.

---
## Execution Log
_To be filled during /code-foundations:build_
