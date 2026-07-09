# Plan: Engram → Obsidian Vault Exporter

**Created:** 2026-07-08
**Status:** in-progress
**Started:** 2026-07-08 23:52
**Current Phase:** 1
**Complexity:** medium

---

## Context

Engram's graph memory (deduped entities + predicate-typed edges) is only reachable via targeted
search — there is no way to walk it out as a whole. Build an `engram export <dir>` CLI that pulls a
user's tenant-scoped graph and renders it into a browsable Obsidian markdown vault (entities as
notes, edges as wikilinked bullets). Grounded in the confirmed research doc
`.code-foundations/research/2026-07-08-obsidian-vault-exporter.md` (decisions D1–D8).

## Constraints

- **Tenant-scoped, auth-honest** — reads only the caller's tenant, pinned from the verified identity
  (`authgrpc.IdentityFrom(ctx)`), never from the request; ACL-filtered per record, matching
  `Search`/`Audit` (`internal/server/server.go:131`,`:178`).
- **New scan primitive required** — no `search_after`/PIT/scroll exists anywhere in the repo; every
  read is size-capped. Reusing the `size:1000` cap on `Neighbors` would silently truncate a real vault.
- **Live-filtered per tier** — entities `expired_at==nil` (`Entity.Live()`), edges
  `invalid_at==nil && expired_at==nil` (`Edge.Live()`, `internal/graph/graph.go:121`). These are not
  the same test.
- **Semantic index not read** — edges already carry resolved `from`/`to`, `predicate`, `statement`;
  entities carry name/aliases. The vault reads graph tiers only.
- **Snapshot semantics** — clobber-and-regenerate; the tool owns the output dir (D6). Engram is
  append-only, no write-back.
- Proto regeneration via `make proto` (buf, pinned); `make proto-check` gates CI.

## Chosen Approach

**Paginated unary `Export` RPC + CLI-side rendering** — a new *unary* `Export(ExportRequest)
returns (ExportResponse)` RPC returns a bounded page of tenant-scoped graph records plus a cursor;
the CLI re-calls with the cursor until the graph is exhausted, then renders the Obsidian vault to
disk. Each page is bounded, so it avoids gRPC's 4 MB message cap, and — critically — a unary RPC
runs under the **existing** unary auth/telemetry interceptor chain
(`cmd/engram-server/main.go:265-267`), so no new security code is written.
**Fallback:** server-streaming `Export` — rejected as primary (see below) but revivable if
round-trip chattiness ever matters, at the cost of building a secured stream interceptor.

## Rejected Approaches

- **Server-streaming `Export`:** the repo registers only `ChainUnaryInterceptor` and has no
  `StreamServerInterceptor` anywhere — a streaming RPC would run with **no auth and no telemetry**.
  Making it safe means building and securing a new stream interceptor chain from scratch (the exact
  place data leaks hide). Not worth it when unary paging avoids the 4 MB cap just as well.
- **CLI reads OpenSearch directly:** bypasses auth/ACL/tenant-scoping — dishonest for a "vault your own memory" tool.
- **Server renders markdown:** puts presentation and filesystem concerns in the service; rejected for separation.

---

## Implementation Phases

### Phase 1: Graph scan primitive
**Model:** sonnet
**Skills:** aposd-designing-deep-modules
**Gate:** Full

**Goal:** Add cursor-paginated full-index iteration over the graph entity and edge indices to the
graph store, live- and tenant-filtered, as the read foundation the export endpoint builds on.

**Scope:**
- IN: new `Backend` methods `ScanEntities`/`ScanEdges`; `Store` wrappers; a cursor type; OpenSearch
  `search_after`+sort impl; `MemBackend` fake impl; unit tests.
- OUT: any server/RPC/CLI code; semantic-index reads; graph re-derivation.

**Constraints:** Deterministic total sort (e.g. by `id`) so `search_after` paginates completely and
stably. Live filter per tier (entity vs edge differ). Tenant filter via `term tenant_id`. Treat
`index_not_found` as empty, not error (`isIndexNotFound`, `opensearch.go:297`). No hardcoded size
cap that truncates the tier — page until exhausted.
**Edge cases:** empty tenant (no index yet) → clean empty result; page boundary exactly on batch
size; a cursor from an emptied index; entity present but all its edges expired.
**Depends on:** none | **Unlocks:** Phase 2
**File scope:** `internal/graph/**`
**Produces:** `graph.Backend` + `graph.Store` methods
`ScanEntities(ctx, tenantID string, cursor Cursor) (items []Entity, next Cursor, err error)` and
`ScanEdges(ctx, tenantID string, cursor Cursor) (items []Edge, next Cursor, err error)`; opaque
`Cursor` (nil/zero = start, zero `next` = exhausted). Live+tenant filtering applied inside.

**Approach notes:** `search_after` chosen over PIT/scroll — simplest, stateless, no server-side
cursor lifecycle; acceptable that concurrent writes during export may be missed (snapshot semantics).
**File hints:** `internal/graph/store.go` (Backend interface + MemBackend), `internal/graph/opensearch.go` (osJSON/osSearchHits helpers, live/tenant filters).

**Done when:**
- [ ] DW-1.1: `ScanEntities`/`ScanEdges` on `Backend`, `Store`, `MemBackend`, and OpenSearch backend, using `search_after` + total sort.
- [ ] DW-1.2: Full-tier iteration returns every live record across multiple pages (verified past one batch size), never truncating.
- [ ] DW-1.3: Only live records surface — entities `expired_at==nil`; edges `invalid_at==nil && expired_at==nil`.
- [ ] DW-1.4: Results scoped to the passed `tenantID`; empty/missing index yields empty, not error.

**Difficulty:** MEDIUM
**Uncertainty:** OpenSearch sort/`search_after` tie-break field choice; confirm `id` is a stable keyword-mapped field.

### Phase 2: Export RPC + server wiring
**Model:** fable
**Skills:** ca-architecture-boundaries, cc-defensive-programming
**Gate:** Full
**Security-sensitive:** yes

**Goal:** Expose the Phase 1 scan as a tenant-scoped, ACL-filtered *unary* `Export` RPC that returns
one bounded page + a cursor, wiring the graph store into the server behind a narrow seam and adding
the client method.

**Scope:**
- IN: proto `Export` RPC + request/response messages (request carries a cursor, response carries a
  record page + `next_cursor`); regenerate; `server.Export` handler; new `Exporter` seam interface +
  `svc` field; wire graph store in `cmd/engram-server/main.go`; `engramclient.Export`; handler unit tests.
- OUT: CLI; markdown rendering; changes to existing RPCs; any streaming/new-interceptor work.

**Constraints:** Unary RPC, so it runs under the existing unary auth/telemetry chain — no stream
interceptor needed. Pin `tenant_id` from `authgrpc.IdentityFrom(ctx)`, never the request (mirror
`Search`, `server.go:131`). Run each record through `s.ACL.CanRead`; fail-closed. Return
`codes.Unimplemented` if the `Exporter` seam is nil (mirror `server.go:166`); opaque error messages,
no existence oracle. Follow the consumer-defined-interface DI pattern (`StatusProbe`/`Auditor`,
`server.go:28-44`). Bound page size so a response stays under the 4 MB gRPC cap. Regenerate proto
with `make proto`; `make proto-check` must pass.
**Edge cases:** unauthenticated (existing interceptor → `Unauthenticated`); empty tenant → one empty
page with a terminal (zero) cursor; a page exactly filling the bound; a record failing `CanRead` is
skipped, not fatal; a stale/garbage cursor from the client.
**Depends on:** Phase 1 | **Unlocks:** Phase 3
**File scope:** `api/proto/**, api/engrampb/**, internal/server/**, internal/engramclient/**, cmd/engram-server/**`
**Produces:** `rpc Export(ExportRequest) returns (ExportResponse)` — `ExportRequest{ cursor }`,
`ExportResponse{ entities[], edges[], next_cursor }` (structured records, not markdown; empty
`next_cursor` = exhausted); `engramclient.Client.Export(ctx, cursor) (*ExportResponse, error)`.
Tenant/ACL enforced server-side.

**Approach notes:** unary paging over streaming — a unary RPC inherits the existing, proven auth
barricade, whereas streaming has no interceptor in this repo and would need one built and secured
from scratch. Server returns structured records (not rendered markdown); rendering is a CLI concern.
**File hints:** `api/proto/engram.proto` (add RPC), `internal/server/server.go` (handler + Exporter seam), `cmd/engram-server/main.go:270` (wire svc field), `internal/engramclient/client.go:92` (mirror Audit).

**Done when:**
- [ ] DW-2.1: `Export` RPC defined in proto and regenerated code committed; `make proto-check` clean.
- [ ] DW-2.2: Handler returns a bounded page of live entities+edges for the caller's tenant sourced via Phase 1 scan; `next_cursor` advances and empties on exhaustion.
- [ ] DW-2.3: Tenant pinned from verified identity; a token for tenant A never receives tenant B's records (test).
- [ ] DW-2.4: Records failing `ACL.CanRead` are omitted; nil `Exporter` seam → `Unimplemented`.
- [ ] DW-2.5: `engramclient.Export` returns a page + cursor; unauthenticated call rejected by the existing interceptor.

**Difficulty:** MEDIUM
**Uncertainty:** cursor encoding shape (opaque bytes vs typed) at the wire layer; page-size bound vs typical record size.

### Phase 3: CLI `export` + Obsidian vault rendering
**Model:** fable
**Skills:** cc-defensive-programming, cc-routine-and-class-design
**Gate:** Full
**Security-sensitive:** yes

**Goal:** Add an `engram export <dir>` subcommand that consumes the paginated `Export` pages (calling
with the cursor until exhausted) and writes an Obsidian-openable vault — entity notes with
frontmatter/aliases and edges as piped wikilink bullets.

**Scope:**
- IN: `export` subcommand (dial + token like `status`); positional `<dir>`; `--force`; cursor-paged
  consumption; entity-note rendering (frontmatter, aliases, `mention_count`); edge bullets as
  `- <predicate> [[file|Name]]`; deterministic homonym filenames + char sanitization; drop edges to
  non-exported/expired endpoints with a printed count; clobber-and-regenerate; unit + e2e tests.
- OUT: server/store changes; semantic content; write-back.

**Constraints:** Entity names are untrusted ingested content → **every written path must stay inside
`<dir>`**; reject/strip path separators and `..` so a name like `../../etc/x` cannot escape the vault
(the core security requirement of this phase). Filenames deterministic across runs — sanitize
FS/Obsidian-illegal chars; on display-name collision append a short prefix of the entity `id` (stable
sha256). Emit a link only when the target entity was exported (drop danglers; print "N edges, M
dropped"). Refuse a non-empty dir the tool didn't create unless `--force`; on `--force`, clean before
regenerating. Build the exported-entity id→filename map before rendering edges. This is the first CLI
subcommand to write files — create dirs, write per-note files atomically.
**Edge cases:** target dir missing → create; existing foreign files → refuse without `--force`;
empty vault (0 entities); entity name that sanitizes to empty; two entities colliding after
sanitization; edge whose endpoint wasn't exported; very large vault (paged, not buffered whole).
**Depends on:** Phase 2 | **Unlocks:** none
**File scope:** `internal/cli/**, e2e/**`
**Produces:** `engram export <dir> [--force] [--addr] [--token]` → exit 0 + a populated Obsidian
vault directory on success; non-zero exit + a refusal message when `<dir>` is a foreign non-empty dir
and `--force` is absent.
**Rollback:** clobber is guarded by `--force` on a tool-owned dir; refuse-by-default on foreign dirs is the mitigation — not a point of no return.

**Approach notes:** id→filename map assembled from the entity pages first, so edge bullets resolve
to real files and danglers are droppable deterministically.
**File hints:** `internal/cli/cli.go:49` (subcommand dispatch), `internal/cli/cli.go:206` (dialClient), `e2e/harness.go:163` (RunCLI for e2e vault assertion).

**Done when:**
- [ ] DW-3.1: `engram export ./vault` against a populated tenant writes one `.md` per exported entity with an H1, frontmatter (aliases, mention_count, provenance), and edge bullets.
- [ ] DW-3.2: Edges render as `[[file|Display]]` piped links resolving to real note files; no dangling links; dropped-edge count printed.
- [ ] DW-3.3: Homonym display-name collisions get deterministic id-suffixed filenames, stable across re-runs; illegal chars sanitized.
- [ ] DW-3.4: Re-running clobbers-and-regenerates; refuses a foreign non-empty dir unless `--force`.
- [ ] DW-3.5: e2e asserts every `[[file]]` link target resolves to a real note file on disk and each note's frontmatter parses as valid YAML (machine-checkable proxy for "opens cleanly in Obsidian").
- [ ] DW-3.6: No written file path escapes `<dir>` — an entity name containing `../` or path separators is confined inside the vault (traversal test).

**Difficulty:** MEDIUM
**Uncertainty:** exact frontmatter keys Obsidian surfaces cleanly; collision-suffix format readability.

---

## Test Coverage
**Level:** 100%

## Test Plan

**Phase 1 — scan primitive (`internal/graph`)**
- [ ] Unit: `ScanEntities`/`ScanEdges` return every live record across >1 page (DW-1.1, DW-1.2)
- [ ] Boundary: total count exactly on a page boundary; count one over (pagination continues)
- [ ] Dirty: expired entity excluded; superseded-not-expired edge (`invalid_at` set, `expired_at` nil) excluded (DW-1.3)
- [ ] Unit: tenant A scope never returns tenant B records (DW-1.4)
- [ ] Dirty: missing/empty index → empty result, not error; cursor from emptied index exhausts cleanly (DW-1.4)
- [ ] Dirty: entity present but all its edges expired → entity surfaces, no edges, no crash (edge case)

**Phase 2 — export RPC (`internal/server`, proto, client)**
- [ ] Integration: `make proto-check` clean after regeneration (DW-2.1)
- [ ] Unit: handler returns a bounded page of live entities+edges for the caller's tenant via Phase 1 scan; paging across cursors returns every record and empties `next_cursor` on exhaustion (DW-2.2)
- [ ] Dirty (security): token for tenant A never receives tenant B's records (DW-2.3)
- [ ] Dirty: record failing `ACL.CanRead` omitted, not fatal (DW-2.4)
- [ ] Dirty: nil `Exporter` seam → `Unimplemented`; unauthenticated call → `Unauthenticated` (DW-2.4, DW-2.5)
- [ ] Dirty: empty tenant → single empty page with empty `next_cursor`, no error (DW-2.2)
- [ ] Dirty: stale/garbage cursor from the client handled without leaking cross-tenant data
- [ ] Boundary: a page exactly filling the size bound continues cleanly on the next call
- [ ] Unit: `engramclient.Export(ctx, cursor)` returns a page + advancing cursor (DW-2.5)

**Phase 3 — CLI + vault rendering (`internal/cli`, `e2e`)**
- [ ] e2e: export populated tenant → one `.md` per entity with H1, frontmatter (aliases/mention_count/provenance), edge bullets (DW-3.1)
- [ ] e2e: piped `[[file|Display]]` links resolve to real files; no dangling links; dropped-edge count printed (DW-3.2, DW-3.5)
- [ ] Unit: homonym collision → deterministic id-suffixed filenames, identical across two runs (DW-3.3)
- [ ] Dirty/boundary: FS/Obsidian-illegal chars sanitized; name sanitizing to empty handled; two entities colliding after sanitization (DW-3.3)
- [ ] Dirty: re-run clobbers-and-regenerates; foreign non-empty dir refused without `--force`, cleaned with it (DW-3.4)
- [ ] Dirty (security): entity name containing `../` or path separators writes strictly inside `<dir>` — traversal confined (DW-3.6)
- [ ] Dirty: empty vault (0 entities) → valid empty output dir, no crash; missing target dir → created
- [ ] Boundary: large vault spanning many pages assembles fully via cursor paging (not buffered whole)

---

## Assumptions

| Assumption | Confidence | Verify Before Phase | Fallback If Wrong |
|---|---|---|---|
| Entity `id` is a stable, keyword-mapped field usable as a `search_after` tie-break sort | MED | Phase 1 | Add/adopt a keyword sort field, or use `_id` |
| A page bound holds a batch of entities+edges comfortably under gRPC's 4 MB message cap | HIGH | Phase 2 | Lower the page size bound |
| Obsidian resolves `[[file|Display]]` piped links + `aliases:` frontmatter as expected | HIGH | Phase 3 | Adjust link/frontmatter format; core graph still works |
| Graph store can be threaded into `server.New` without disturbing telemetry/retrieval wiring | HIGH | Phase 2 | Expose via a dedicated accessor from `wireGraph` |

## Decision Log

| Decision | Alternatives Considered | Rationale | Phase |
|---|---|---|---|
| Paginated unary `Export` RPC | Server-streaming; direct-OpenSearch CLI | Reuses the existing unary auth chain (no stream interceptor exists in repo); bounded pages still dodge the 4 MB cap | 2 |
| CLI renders markdown | Server renders | Keeps presentation/filesystem out of the service | 3 |
| `search_after` pagination | PIT; scroll | Stateless, simplest; snapshot semantics acceptable | 1 |
| Read graph tiers only, not semantic | Read semantic facts too | Edges already carry predicate+statement+endpoints | 1–3 |
| Deterministic id-suffix on filename collision | Alias-based; hash-only names | Stable across re-runs (clobber), keeps names readable | 3 |

---

## Notes

- **Empty-tier reality:** the current instance has 0 semantic / 0 graph — a vault built today is
  empty. The feature is only demonstrable once extraction populates the graph. Flag in HANDOFF.
- **Known limitations to document in the tool** (from research): literal objects become entity notes
  (`stage.go:60`); graph-view relationship lines are unlabeled (predicate only in bodies); frontmatter
  `valid_at` is unreliable while clients don't populate `occurred_at` (`worker.go:363`).
- First file-writing CLI subcommand in the repo — no local precedent; all existing subcommands print to stdout.
- Streaming `Export` was rejected because the repo has no gRPC stream interceptor — a streaming RPC would bypass auth entirely (see Rejected Approaches). Unary paging keeps the proven barricade.

---

## Execution Log

### Phase 1: Graph scan primitive (Gate: Full)
- [x] BUILD: Discovery + design + implementation (stub → implement → validate) complete
- [x] REVIEW: Verification passed (all 4 DW items with execution evidence; 4 edge cases covered)
- [x] Committed
Commit: _pending_
Summary: Added cursor-paginated `ScanEntities`/`ScanEdges` to `graph.Backend`/`Store`/`MemBackend` + OpenSearch backend (`search_after` + `sort:[{id:asc}]`), live- and tenant-filtered; opaque `Cursor` (zero=start, zero-next=exhausted). This is the full-index read foundation Phase 2's export RPC pages over. 18 new tests; MemBackend/OpenSearch pagination parity enforced.
