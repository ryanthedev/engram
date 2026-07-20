# Plan: Memory→Knowledge mapping prototype, rendered in the Obsidian vault

**Created:** 2026-07-19
**Status:** complete
**Started:** 2026-07-19 22:29
**Completed:** 2026-07-19 23:18
**Duration:** ~1h (22:29 → 23:18)
**Current Phase:** 3
**Complexity:** simple
---
## Context

**Problem:** Engram's KNOWLEDGE tier (flat, BM25-only document collections) and MEMORY tier (episodic + semantic/graph) are deliberately isolated — knowledge docs carry no links and never reference memory. We want to prove, end-to-end and visible in Obsidian, that a knowledge doc can be *mapped* to a memory entity via a soft foreign key and rendered as a wikilinked note alongside the memory vault.

**Mechanism (chosen, verified against the code):** a knowledge doc carries a `memory_ref` keyword field = a real rtd memory ENTITY ID. At export time a new knowledge renderer resolves that id through the existing `buildVaultRefs` entity-id→note-name map and emits a resolving `[[wikilink]]` into the memory concept note; the mapped concept note gets a reciprocal backlink. Knowledge collections are read via the existing `KnowledgeCollections`/`KnowledgeSearch` RPCs (already served by the live engramd) — **no proto or server change, no redeploy**.

**Key facts (do not re-derive):**
- Knowledge write ops (create_collection/ingest) require the admin/harvester role; `engram token create` (`internal/cli/cli.go:143`) mints role-LESS identities and exposes no `--roles` flag. `auth.TokenIssuer.Issue` already binds `id.Roles` (`internal/auth/auth.go:225,241`). rtd has ZERO knowledge collections → the prototype must seed demo docs.
- If the seeded collection is created `public:true`, the ordinary role-less export identity can READ it — so only *seeding* needs the admin token; the *export* path is unchanged auth-wise.
- Rich vault export lives in `internal/cli/export.go` + `vault.go`/`vaultnotes.go`/`vaultmaps.go`/`vaultmodel.go`. `safeNoteName`/`uniqueNoteName` (export.go) are the filename choke points; `buildVaultRefs` builds the id→note-name map; sanitizer barricades are `sanitizeFilename`/`cleanInline`/`sanitizeBody`/`quoteBlock`; `confinedVaultPath` (vault.go) enforces path confinement. Knowledge doc `title`/`text`/`fields` are UNTRUSTED and must pass the same barricades.
- Runtime: live engramd = podman `local-engramd-1` on `localhost:7071`; OpenSearch `engram-e2e-os` is VOLUMELESS — never `down`, never touch it. Host CLI `./bin/engram`.

**Success criteria:** After building and running the orchestrator live-run, `~/vaults/rtd-rich` contains a `knowledge/` folder whose notes carry `[[wikilinks]]` that resolve (in Obsidian's graph view and link-follow) to existing memory concept notes, and the mapped concept notes show a reciprocal "Referenced by" backlink — all rendered deterministically and injection-safely.

## Constraints
- No change to the volumeless OpenSearch container; no `compose down`. Server already redeployed and serving knowledge RPCs — CLI-only rebuild for this feature.
- Untrusted knowledge content (title/text/memory_ref) must route through the existing sanitizer + path-confinement barricades — no new injection or path-escape surface.
- Deterministic, reproducible export (no LLM in the loop), consistent with the existing exporter.
- Prototype scope: a tight demonstrable end-to-end over exhaustive coverage, but per-phase tests and the security gates stay.
- Breaking changes acceptable (single-user personal box).
---
## Implementation Phases

### Phase 1: Role-bearing token minting
**Skills:** code-foundations:cc-defensive-programming
**Model:** sonnet
**Gate:** Full
**Depends on:** none
**File scope:** internal/cli/cli.go, internal/cli/cli_test.go

**Goal:** Add a `--roles` flag to `engram token create` so an admin/harvester token can be minted, unblocking knowledge seeding.

**Scope:**
- IN: parse a comma-separated `--roles` flag in the `token create` handler; set `auth.Identity.Roles` before `Issue`; trim/normalize empties; role-less create keeps working unchanged.
- OUT: any change to token verification, role semantics, or the knowledgeauth policy (roles already flow end-to-end once bound at mint).

**Edge cases:** empty/whitespace-only `--roles` → no roles (identical to today); duplicate/whitespace-padded role names normalized; unknown role strings are accepted at mint (authorization is policy-side, not mint-side).

**Produces:** `engram token create --tenant T --user U [--agent A] --roles admin,harvester` prints a raw token whose verified `Identity.Roles` contains exactly the normalized set. Consumed by the orchestrator live-run (to mint the seeding token) — not by another build phase.
**Security-sensitive:** yes
**Rollback:** additive flag; no persisted migration. Tokens minted are revocable via `engram token revoke <handle>`.

**Done when:**
- [ ] DW-1.1: `token create ... --roles admin,harvester` mints a token; verifying it yields `Roles == ["admin","harvester"]` (order-normalized).
- [ ] DW-1.2: `token create` with no `--roles` mints a role-less token exactly as before (regression).
- [ ] DW-1.3: dirty input `--roles " admin , , harvester "` normalizes to `["admin","harvester"]` (no empty/whitespace entries) — table test.

### Phase 2: Knowledge→vault export rendering with memory mapping
**Skills:** code-foundations:cc-defensive-programming, code-foundations:code-clarity-and-docs
**Model:** sonnet
**Gate:** Full
**Depends on:** none
**File scope:** internal/cli/export.go, internal/cli/vaultknowledge.go, internal/cli/vaultknowledge_test.go, internal/cli/vaultnotes.go, internal/cli/vault.go, internal/cli/export_test.go, internal/engramclient/knowledge.go, internal/engramclient/knowledge_test.go

**Goal:** After the memory vault is assembled, fetch every readable knowledge collection's docs and render a `knowledge/` folder, resolving each doc's `memory_ref` to a `[[wikilink]]` into the mapped memory concept note and adding a reciprocal backlink on that concept note.

**Scope:**
- IN: reuse the existing `KnowledgeCollections` + `KnowledgeSearch` client wrappers (engramclient files in scope only if a thin drain helper is added) to enumerate collections and fetch a collection's docs via a **single empty-query `KnowledgeSearch` call at `k=100` (MaxK — the RPC has no offset/cursor/paging), warning in the summary when a collection returns exactly `k` docs (possible truncation)**; sort fetched docs **client-side by doc id** before naming/rendering so output is deterministic despite tied `match_all` scores; a `knowledge/` renderer that emits one note per doc (frontmatter: `engram_id`=doc id, `collection`; body: sanitized `title` + `text`); `memory_ref` resolved via the `buildVaultRefs` id→note-name map to a `[[concept]]` wikilink (or an inert "unresolved: <memory_ref_name or id>" line when the id isn't in the exported graph); a "Referenced by" backlink section appended to each mapped concept note; note filenames through `uniqueNoteName`/`safeNoteName`; every path through `confinedVaultPath`; counts folded into the export summary line.
- OUT: knowledge WRITE from the CLI; server/proto changes; server-side paging (the RPC has none — YAGNI); kNN/fusion; any non-`memory_ref` link derivation (name-matching, identifier scraping) — out of prototype scope.

**Edge cases:** zero collections or empty collection → no `knowledge/` folder, no error, export unchanged; `memory_ref` absent/empty → doc renders with no wikilink; `memory_ref` present but the entity isn't in the exported graph (ghost/filtered) → inert unresolved marker, no dangling link, backlink skipped; a knowledge fetch error must NOT destroy the already-assembled memory vault (clean-late invariant preserved — knowledge is additive and fails soft with a warning); untrusted `title`/`text`/`memory_ref` with control chars, `[[`/`]]`, path separators, `../`, over-long or NFC/NFD names → sanitized, confined, and NFC-folded exactly like memory notes; multiple docs mapping to the same concept → all listed in that concept's backlinks, deterministically ordered.

**Produces:** `engram export <dir>` additionally writes `<dir>/knowledge/*.md` and augments `concepts/*.md` with backlinks; summary reports a knowledge count. Consumed by the orchestrator live-run.
**Security-sensitive:** yes
**Rollback:** additive renderer (new `knowledge/` files + append-only concept-note backlink section); re-exporting without the feature restores the prior vault byte-for-byte.

**Done when:**
- [ ] DW-2.1: with a stub server serving one public collection of docs whose `memory_ref`s hit exported entities, export writes one `knowledge/<name>.md` per doc (rendered in deterministic doc-id order), each containing a `[[<concept note>]]` that matches an actual concept filename.
- [ ] DW-2.2: each mapped concept note gains a "Referenced by" section listing the knowledge note(s) that map to it, deterministically ordered by doc id; a concept mapped by two docs lists both.
- [ ] DW-2.3: a doc whose `memory_ref` matches no exported entity renders with an inert unresolved marker (labeled with `memory_ref_name` when present, else the raw id) and produces no dangling wikilink and no backlink.
- [ ] DW-2.4: injection trap — a doc with `title`/`text`/`memory_ref` carrying control chars, `[[`/`]]`, `../`, and an over-long NFC/NFD name is sanitized, byte-budgeted, NFC-folded, and written strictly inside `<dir>` (path-confinement + wikilink-sanitizer assertions).
- [ ] DW-2.5: knowledge fetch failure leaves the fully-assembled memory vault byte-intact and exits with a soft warning, not a hard error.
- [ ] DW-2.6: zero knowledge collections → no `knowledge/` folder and the memory-only vault is identical to pre-change output.

### Phase 3: Seed tooling for the mapped collection
**Skills:** code-foundations:cc-defensive-programming
**Model:** sonnet
**Gate:** Standard
**Depends on:** none
**File scope:** cmd/engram-seed-knowledge/**, internal/cli/seedknowledge.go, internal/cli/seedknowledge_test.go, scripts/seed-curated-notes.sh

**Goal:** Provide a small, idempotent seeding tool that creates a `public` `curated_notes` collection with a `memory_ref` keyword mapping and ingests a handful of demo docs whose `memory_ref`s point at real rtd entity ids.

**Scope:**
- IN: a seed routine/command that, given `-addr` + admin token, calls `CreateCollection(curated_notes, {memory_ref:keyword/filterable, memory_ref_name:keyword})` (idempotent: tolerate already-exists) then `KnowledgeIngest` of a fixed demo-doc set (title/text/memory_ref/memory_ref_name) under a stable `source`+`harvest_id`; re-runnable (upsert by doc id).
- OUT: choosing entity ids dynamically from the live graph (the demo set carries a small curated list of real rtd entity ids + names, editable in one place); any deletion beyond the mark-and-sweep the ingest supports.

**Edge cases:** collection already exists → skip create, proceed to ingest (no error); re-run → docs upsert in place, no duplicates; missing/invalid token → clean PermissionDenied surfaced with a clear message pointing at `token create --roles admin`.

**Produces:** running the tool against the live server with an admin token yields a readable `curated_notes` collection containing the demo docs. Consumed by the orchestrator live-run.
**Security-sensitive:** no
**Rollback:** docs removable via `knowledge_delete` mark-and-sweep; the empty collection stub persists (no drop-collection RPC exposed) — acceptable, named `curated_notes` and harmless. Point of no return: none data-wise (rtd memory untouched; this only adds a knowledge collection).

**Done when:**
- [ ] DW-3.1: against a stub server, the seed routine issues one create-collection (with the `memory_ref` mapping) and one ingest batch of the demo docs; a re-run issues ingest again without a duplicate-create failure (idempotency asserted).
- [ ] DW-3.2: the demo-doc set is defined in one place, each doc carrying a `memory_ref` (entity id) + `memory_ref_name`, and a table test asserts every doc has a non-empty `memory_ref`.
- [ ] DW-3.3: a missing/role-less token path surfaces the server's PermissionDenied with a message naming the `--roles admin` remedy (dirty test).
---
## Test Coverage
**Level:** 100% of new/changed functions (matches the existing exporter suite's standard).
## Test Plan
- [ ] Phase 1: DW-1.1/1.2/1.3 — table test over `--roles` parsing incl. the dirty normalization case, driven through the `token create` handler; regression for the no-flag path.
- [ ] Phase 2: DW-2.1–2.6 — extend the `exportStub` to serve `KnowledgeCollections`/`KnowledgeSearch`; assert rendered `knowledge/` notes, resolving wikilinks, reciprocal backlinks, unresolved-marker soft path, fetch-failure clean-late (dirty), zero-collection no-op, and the injection/path-confinement trap (dirty).
- [ ] Phase 3: DW-3.1–3.3 — stub-server create+ingest with idempotent re-run, single-source demo-doc invariant, and the role-less PermissionDenied message (dirty).
---
## Notes
- Read RPCs are unauthenticated-role (public collection) so the export path keeps the current token; only seeding needs admin — this is why Phase 1 is a prerequisite for the *live run*, not for Phases 2/3 as code.
- Phases 1/2/3 have disjoint file scopes and no code dependency → build may run all three BUILD agents in parallel; the live sequencing (token → seed → export) is orchestrator-time.
- Entity ids for the demo docs: take from concept-note frontmatter `engram_id` in the already-exported `~/vaults/rtd-rich/concepts/` (e.g. `agent instructions`), so wikilinks resolve on the very next export.
- Orchestrator live-run (post-build, not a build phase): rebuild `./bin/engram`; mint admin token via Phase 1; run Phase 3 seed; `engram export ~/vaults/rtd-rich`; verify `knowledge/` + backlinks; open in Obsidian.
---
## Execution Log
_To be filled during /code-foundations:build_

### Phase 1: Role-bearing token minting (Gate: Full)
- [x] BUILD: Discovery + design + implementation complete
- [x] REVIEW: Verification passed (3-sample fable, 3/3 PASS)
- [x] Committed
Commit: see git log (Phase 1/3 trailer)
Summary: `engram token create` now accepts `--roles a,b` (parseRoles helper in cli.go), binding roles into the minted token's identity via the existing Issue/normalizeRoles path; role-less mint is byte-identical to before. Unblocks minting an admin token for knowledge seeding.

### Phase 2: Knowledge→vault export rendering with memory mapping (Gate: Full)
- [x] BUILD: Discovery + design + implementation complete
- [x] REVIEW: Verification passed (3-sample fable, 3/3 PASS)
- [x] Committed
Commit: see git log (Phase 2/3 trailer)
Summary: `engram export` now runs a knowledge post-pass: fetches every readable collection (single empty-query k=100, truncation-warned), renders a deterministic `knowledge/` folder, resolves each doc's `memory_ref` to a `[[concept]]` wikilink via buildVaultRefs (inert marker when unresolved), and appends "Referenced by" backlinks to mapped concept notes — all through the existing sanitizer + confinedVaultPath barricades; knowledge failures fail soft, leaving the memory vault byte-intact.

### Phase 3: Seed tooling for the mapped collection (Gate: Standard)
- [x] BUILD: Discovery + design + implementation complete
- [x] REVIEW: Verification passed (single-sample sonnet, PASS)
- [x] Committed
Commit: see git log (Phase 3/3 trailer)
Summary: New `engram-seed-knowledge` tool (core in internal/cli/seedknowledge.go) idempotently provisions the public `curated_notes` collection (memory_ref keyword/filterable) and ingests a single-source demo-doc set mapped to real rtd entity ids; role-less/PermissionDenied surfaces an actionable `--roles admin` message. Added transport-edge IsAlreadyExists/IsPermissionDenied predicates to engramclient (internal/cli may not import grpc/codes — importlint boundary).
