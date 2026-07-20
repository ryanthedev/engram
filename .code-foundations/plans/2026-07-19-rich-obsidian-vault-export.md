# Plan: Rich Obsidian Vault Export (dual-primary, memory-only, knowledge-ready)
**Created:** 2026-07-19
**Status:** complete
**Started:** 2026-07-19
**Completed:** 2026-07-19
**Complexity:** medium
---
## Context
Engram's Obsidian export builds one note per graph *entity* — thin sentence-fragments with empty bodies and orphan leaves, and the rich episodic prose never reaches the vault. We want a **rich, dual-primary vault** from a single deterministic export: readable event pages *and* a navigable concept graph threaded with provenance.

Validated against the live rtd store (9201): episodic prose is the human signal (~726-char events); `ExportEdge` already ships `Statement` **and** `ValidAt` on the wire (server/export.go:184–197) but the CLI renders only the bare predicate; episodic records are NOT on the wire (the `Exporter` seam scans graph only, and its sole implementer `*graph.Store` cannot reach the episodic index — a different subsystem, `*store.OpenSearchStore`). Every entity is `mention_count:1`, so **hub selection keys on edge degree, not mention_count**: a concept earns its own note only at **degree ≥ 2**; degree ≤ 1 leaves are **ghost nodes** (unresolved `[[links]]`, no file) — this is what keeps the vault from becoming a fragment field again (mean degree ≈1.2). Near-duplicate entities collapse **only when their normalized names are equal** (NOT by shared `source_id` — every entity from one event shares that event's id, which would fuse unrelated concepts). Knowledge tier is empty on the live store with zero linkage to memory — out of scope now, but the note-source abstraction stays tier-agnostic so a future `stageKnowledge` slots in without redesign.

## Constraints
- **Deterministic assembly only** — no LLM; byte-stable re-runs (preserve sorted output + atomic writes). All time formatting is `.UTC()`; no map-iteration-order or wall-clock leakage anywhere (folder paths included).
- **Breaking changes allowed** — no production users, no backward-compat; the entity-per-note format is replaced.
- **Untrusted-input safety** — episodic prose entering note bodies is a second-use injection vector. A body-level sanitizer must neutralize: leading `---` (frontmatter forge), `> [!callout]` forge, `[[`/`![[` wikilink/transclusion, fenced-code breakout, **raw HTML (`<...>`, e.g. `<iframe>` external-content beacons)**, and dangerous link/URI schemes (`obsidian://`, `data:`, `javascript:`). Quoting prose into a callout is a *separate* hazard (per-line `>` prefix incl. blank lines; a `[!x]`-leading line must not forge a callout after wrapping) handled by a dedicated `quoteBlock` helper. The existing `sanitizeFilename`/`cleanInline` cover names/predicates only.
- **Architecture (Approach A):** server stays a thin ACL-filtered tier pager; all join/cluster/render lives in the CLI. Server change is one new episodic stage behind a *separate* seam.
- **Knowledge-ready seam:** the note-source model is tier-agnostic; do NOT build the knowledge branch now (YAGNI — no live data, no linkage).
- Reuses the `.engram-vault` marker + regenerate/clobber flow, the `confinedNotePath` barricade, and atomic temp+rename.

---
## Chosen Approach
**A — Client-side assembly (thin server).** The server adds only an episodic export stage (via a separate `EpisodicExporter` seam, since the existing `Exporter` is graph-bound); all joining (`source_ids → event_id`), clustering, fact-sheet assembly, and rendering happen in the CLI. Rationale: presentation belongs in the delivery layer (Clean Architecture); format iterates without the costly container redeploy; data volumes (249 events, ~1.1k edges) are trivially client-sized; `Statement`/`ValidAt` already on the wire. **Fallback:** if client memory ever constrains at larger scale, promote clustering to a server stage behind the same wire contract.

## Rejected Approaches
- **B — Server-side vault projection:** couples the graph domain to vault presentation and forces a container redeploy per format tweak; no data-access benefit (richness already on the wire).
- **LLM dossier synthesis:** user chose deterministic assembly — nondeterministic output breaks byte-identical re-runs and adds hallucination + API/egress cost.

---
## Implementation Phases

### Phase 1: Server — episodic export stage (separate seam)
**Model:** fable
**Skills:** aposd-designing-deep-modules, cc-defensive-programming
**Gate:** Full

**Goal:** Put the caller's tenant-scoped, ACL-filtered, *successfully-extracted* episodic records on the export wire as a new stage behind a separate `EpisodicExporter` seam, so the client can render event notes and quote source prose.

**Scope:**
- IN: a `search_after`-paginated, tenant-scoped, live scan over the episodic index in `internal/store` (template: the existing `ScanLiveFacts` at facts.go:278, but over episodics), filtered to processed, non-dead-lettered docs; a new `ExportEpisodic` proto message + regenerated `engrampb`; a **separate `EpisodicExporter` seam** on `Server` (implemented by `*store.OpenSearchStore`) — NOT a widening of the graph-bound `Exporter`; an episodic stage in the wire-cursor state machine using its **own string `search_after` token** (graph.Cursor stays for entity/edge stages); `exportEpisodicProto` mapper with `canExport` gating; `cmd/engram-server/main.go` wiring; `engramclient` `ExportPage` view + `ExportEpisodic` type extended to expose the new records.
- OUT: any rendering; the CLI `fetchExport` drain (Phase 5); any knowledge stage.

**Constraints:** tenant pinned from verified identity, fail-closed on missing identity, per-record `canExport`; unknown wire stage → `InvalidArgument` (mirror `decodeExportCursor`); **episodic page uses a byte-budgeted bound** (episodic `Text` is unbounded — a 500-count bound could breach the 4 MB gRPC cap); scan is deterministic (stable `search_after` sort key). Do not let the episodic scan reach into graph's package (tier boundary).
**Edge cases:** empty episodic tier → chains into the entity stage within the same response (empty tenant = one page total, no wasted round trip); stale/forged cursor repositions only inside the caller's tenant; ACL-denied record omitted, ACL error fails the whole call closed; unprocessed / dead-lettered episodic docs excluded from the scan.
**Depends on:** none | **Unlocks:** Phase 2
**File scope:** `api/proto/engram.proto`, `api/engrampb/**`, `internal/server/export.go`, `internal/server/export_test.go`, `internal/store/facts.go`, `internal/store/*_test.go`, `internal/engramclient/export.go`, `internal/engramclient/*_test.go`, `cmd/engram-server/main.go`
**Produces:** `EpisodicExporter interface { ScanEpisodic(ctx, tenantID string, after string) (recs []graph-free episodic record, nextAfter string, err error) }` wired on `Server`; wire `ExportEpisodic{event_id, kind, text, occurred_at, source_ids, scope, team_id, owner_agent_id}`; `engramclient.ExportPage.Episodics []engramclient.ExportEpisodic` (fields `EventID, Kind, Text string; OccurredAt *time.Time; SourceIDs []string`).
**Security-sensitive:** yes
**Done when:**
- [ ] DW-1.1: `ExportEpisodic` in proto + regenerated `engrampb`; `make proto-check` green.
- [ ] DW-1.2: `Export` drains episodic (own string cursor) then chains to entities/edges; unknown stage → `InvalidArgument`; empty episodic tier chains into entities in the same response.
- [ ] DW-1.3: every episodic record passes `canExport`; missing identity → `Unauthenticated`; ACL error → call fails closed (three tests).
- [ ] DW-1.4: `engramclient.ExportPage` exposes episodic records across pages via the byte-bounded stage; unprocessed/dead-lettered docs are absent.
- [ ] DW-1.5: episodic page respects a byte budget (a synthetic oversized-Text set produces multiple pages, none exceeding the bound).

### Phase 2: Client — vault model, refs, sanitizer + quoteBlock
**Model:** fable
**Skills:** aposd-designing-deep-modules, cc-defensive-programming
**Gate:** Full

**Goal:** Build the deterministic, tier-agnostic vault model (events, concepts, and the shared link-resolution `VaultRefs`) from the drained export, plus the two safety primitives every renderer composes: `sanitizeBody` and `quoteBlock`.

**Scope:**
- IN: `sanitizeBody(text) string` neutralizing leading `---`, `> [!callout]`, `[[`/`![[`, fence breakout, raw HTML `<...>`, and `obsidian://`/`data:`/`javascript:` link schemes (transform-not-reject — never drops the whole event); `quoteBlock(text) string` wrapping prose for a `> [!quote]-` callout by prefixing **every** line (blank lines included) with `> ` and neutralizing a `[!`-leading line so wrapping can't forge a nested callout; a tier-agnostic model — `Event{EventID,Title,Body,OccurredAt,ConceptIDs}` (deduped by `event_id`, earliest `CreatedAt` then id tie-break), `Concept{EntityID,Name,Aliases,Degree,Claims,RelatedIDs,Ghost}`, `Claim{Statement,ValidAt,EdgeID,SourceEventID}` — assembled via the `source_ids → event_id` join; **hub rule: `Ghost = Degree < 2`** (degree = distinct edge-endpoint count); **collapse rule: entities whose normalized name (lowercase, collapse internal whitespace, strip surrounding punctuation/quotes) is EQUAL merge into one concept** (their claims/aliases union); `VaultRefs` mapping each event id and concept id to a deterministic, collision-suffixed `noteRef{File,Display,Folder}`.
- OUT: markdown rendering (Phases 3/4); clustering (Phase 4); file writing (Phase 5).

**Constraints:** pure/deterministic — sorted, no map-order or time leakage; join is total (an edge whose source event wasn't exported still renders its Statement, quote-less); collapse merges ONLY on exact normalized-name equality (no fuzzy threshold — nondeterministic).
**Edge cases:** claim whose source event is absent → Statement with no quote; two entities with equal normalized names → one concept; distinct-named entities sharing a `source_id` → NOT merged; duplicate `event_id` docs → one Event (earliest CreatedAt, id tie-break); prose with literal `---`/`> [!`/`<iframe>`/``` ``` ```/`[!danger]`-leading line → neutralized yet legible.
**Depends on:** Phase 1 | **Unlocks:** Phase 3, Phase 4
**File scope:** `internal/cli/vaultmodel.go`, `internal/cli/vaultmodel_test.go`, `internal/cli/sanitize.go`, `internal/cli/sanitize_test.go`
**Produces:** `buildVaultModel(page-drained records) (VaultModel, VaultRefs)` where `VaultModel{Events []Event, Concepts []Concept}` and `VaultRefs` is the shared `map[string]noteRef` (event id + concept id → ref) consumed by Phases 3, 4, 5; `sanitizeBody(string) string`; `quoteBlock(string) string`.
**Security-sensitive:** yes
**Done when:**
- [ ] DW-2.1: `sanitizeBody` neutralizes leading `---`, callout forge, `[[`/`![[`, fence breakout, raw HTML tags, and dangerous URI schemes on an adversarial corpus (≥5:1 dirty), leaving benign prose intact except neutralized tokens.
- [ ] DW-2.2: `quoteBlock` prefixes every line incl. blank lines with `> `; adversarial multi-line input (blank lines, `[!x]`-leading lines) cannot exit or forge a callout (dirty tests).
- [ ] DW-2.3: hub rule `Ghost = Degree < 2` (degree = distinct edge endpoints); normalized-name-equal entities collapse; distinct-named entities sharing a source_id do NOT; duplicate event_ids dedupe deterministically.
- [ ] DW-2.4: `buildVaultModel` joins claims via `source_ids → event_id` (absent event → quote-less claim); output + `VaultRefs` are byte-identical across runs.

### Phase 3: Client — event + concept note rendering
**Model:** sonnet
**Skills:** cc-control-flow-quality
**Gate:** Standard

**Goal:** Render the two primary note types from the model + `VaultRefs`: event notes (full sanitized prose, UTC-time-foldered, concept footer) and concept notes (provenance fact-sheet — each claim's `Statement` + source prose in a folded `> [!quote]-` callout via `quoteBlock`, ordered by `valid_at` then `EdgeID`, related-concept + ghost links).

**Scope:**
- IN: `renderEvent(Event, VaultRefs) (relPath, content)` → `events/YYYY/YYYY-MM-DD <slug>.md` (path from `OccurredAt.UTC()`; nil `OccurredAt` → `events/undated/<slug>.md`) with frontmatter + H1 + `sanitizeBody(text)` + a `**Concepts:**` footer linking joined concepts via `VaultRefs`; `renderConcept(Concept, VaultRefs) (relPath, content)` → `concepts/<name>.md` with a "What we've learned" claim list (each claim's `Statement` + `quoteBlock` of the source event, oldest-first, tie-break `EdgeID`), `## Related concepts`, ghosts as unresolved `[[links]]`; names via `sanitizeFilename`/`cleanInline`, prose via `sanitizeBody`, quotes via `quoteBlock`. No `**Map:**` footer (maps→notes is one-directional; Obsidian backlinks surface the reverse — keeps Phase 3 independent of Phase 4).
- OUT: clustering/maps (Phase 4); vault assembly + write (Phase 5).

**Constraints:** byte-identical re-runs (claims sorted `ValidAt` then `EdgeID`; footer links sorted by ref); ghost concepts emit no file; all quoting through `quoteBlock` (no hand-rolled `>` prefixing).
**Edge cases:** concept with zero claims but degree ≥2 → hub note listing related; event slug empty after sanitize → id-derived fallback; claim with no source quote → Statement alone; nil `OccurredAt` → `undated/`; colliding slugs → `VaultRefs` suffix.
**Depends on:** Phase 2 | **Unlocks:** Phase 5
**File scope:** `internal/cli/vaultnotes.go`, `internal/cli/vaultnotes_test.go`
**Produces:** `renderEvent(Event, VaultRefs) (relPath, content string)` and `renderConcept(Concept, VaultRefs) (relPath, content string)` — pure renderers consumed by Phase 5.
**Done when:**
- [ ] DW-3.1: event note carries full `sanitizeBody`'d prose, a UTC-foldered path (nil → `undated/`), and a concept footer resolving links via `VaultRefs`.
- [ ] DW-3.2: concept note renders claims oldest-first, each `Statement` + a folded `quoteBlock` callout linking the source event note.
- [ ] DW-3.3: only degree ≥2 concepts get files; degree ≤1 appear solely as unresolved ghost `[[links]]`.
- [ ] DW-3.4: identical model+refs → byte-identical notes across two runs.

### Phase 4: Client — topic-map clustering + MOC notes
**Model:** sonnet
**Skills:** cc-pseudocode-programming, cc-control-flow-quality
**Gate:** Standard

**Goal:** Group the concept graph into topic maps and render one MOC note per group so the vault has bounded entry points, sized to the real graph shape (hundreds of small components, not one giant).

**Scope:**
- IN: a deterministic **connected-components** pass over concepts+edges; components with ≥ `minMembers` (e.g. 3) become their own map; smaller ones funnel into **size-bounded `misc` buckets** (`maps/misc-NN.md`, each capped at e.g. 50 members, split deterministically by sorted concept key — NOT one unbounded mega-note); `renderMap(cluster, VaultRefs) (relPath, content)` → `maps/<top-concept>.md` (title = highest-degree member, id tie-break; filename via `sanitizeFilename`) with a member-concept list, a UTC event timeline for the cluster's source events, and cross-cluster out-links; all via `VaultRefs`. Map filenames get the **same deterministic collision suffixing** as `VaultRefs` (assigned in sorted-cluster-key order), and the `misc-` prefix is reserved so a concept map can never clobber a misc bucket.
- OUT: event/concept rendering (Phase 3); assembly/write (Phase 5).

**Constraints:** fully deterministic (fixed component-walk order, id tie-break, no randomness or label-propagation parity dependence); correctness never depends on the exact map count; misc buckets are size-bounded so noise can't relocate into one note; runs well under the export latency budget on ~1.8k concepts.
**Edge cases:** mostly-disconnected graph → many components, sub-threshold ones into bounded misc buckets (never 1817 map notes, never one giant misc); empty graph → zero maps, no error; a single large component → its own map (no artificial split).
**Depends on:** Phase 2 | **Unlocks:** Phase 5
**File scope:** `internal/cli/vaultmaps.go`, `internal/cli/vaultmaps_test.go`
**Produces:** `clusterConcepts(VaultModel) []Cluster` (internal type) and `renderMap(Cluster, VaultRefs) (relPath, content string)` — deterministic clustering + pure renderer consumed by Phase 5.
**Done when:**
- [ ] DW-4.1: clustering is deterministic (fixed order, id tie-break) — identical model → identical clusters and map files; two clusters whose titles sanitize to the same filename (or collide with a `misc-NN` bucket) are disambiguated by collision suffix, not silently clobbered.
- [ ] DW-4.2: sub-threshold components funnel into size-bounded misc buckets (no single misc note exceeds the cap; no per-node map explosion); a large component keeps its own map.
- [ ] DW-4.3: each map lists members, a UTC source-event timeline, and cross-cluster out-links; title = highest-degree member (deterministic); filename `sanitizeFilename`'d.
- [ ] DW-4.4: empty graph → zero map notes, no error.

### Phase 5: Client — vault assembly, CLI wiring, determinism
**Model:** fable
**Skills:** cc-refactoring-guidance, cc-defensive-programming
**Gate:** Full

**Goal:** Replace `writeVault`'s entity-per-note output with the rich assembler — extend `fetchExport` to drain the episodic stage, build the model, render events + concepts + maps, and write them under the existing path-confinement, atomic-write, and clobber-guard invariants, with updated stats and an edit-loss warning.

**Scope:**
- IN: extend `fetchExport` to accumulate `Episodics` across the new stage (cursor-non-advance abort preserved); rewire `runExport`/`writeVault` to `buildVaultModel` + the Phase 3/4 renderers; per-write `confinedNotePath` across the new nested folders (`events/`, `concepts/`, `maps/`) — flat-confined per path element; updated `vaultStats` (events, concepts, maps, ghosts, dropped) + summary; **print a warning that re-export clobbers any manual Obsidian edits** (the vault is now worth annotating); keep the clobber/marker/catastrophic-dir guards + atomic temp+rename + clean-after-fetch.
- OUT: server changes; model/render internals (owned by 2/3/4).

**Constraints:** deterministic full-vault output (byte-identical re-run over one export); no path escapes the vault dir (re-verify per write across nested folders); a failed fetch never clobbers an existing vault.
**Edge cases:** nested path-escape via crafted name → refused (whole-export abort at the barricade, not an escape); empty tenant → marker-only vault; slug collisions across folders → disambiguated within each folder by `VaultRefs`.
**Depends on:** Phase 3, Phase 4 | **Unlocks:** none
**File scope:** `internal/cli/export.go`, `internal/cli/export_test.go`, `internal/cli/vault.go`, `internal/cli/vault_test.go`
**Produces:** the working `engram export <dir>` rich vault (user-observable).
**Security-sensitive:** yes
**Rollback:** not destructive beyond the existing regenerate flow — clobber stays guarded by the `.engram-vault` marker + catastrophic-dir refusal; a failed export leaves the prior vault intact (clean-after-fetch). No point of no return. (Manual-edit loss on re-export is mitigated by the new warning; a `--merge` mode is a future enhancement, out of scope.)
**Done when:**
- [ ] DW-5.1: `engram export <dir>` writes `events/`, `concepts/`, `maps/`; entity-per-note format is gone; `fetchExport` drains episodics.
- [ ] DW-5.2: every write stays inside the vault dir incl. nested folders (path-escape test across all three folders).
- [ ] DW-5.3: full-vault re-run is byte-identical for the same export input.
- [ ] DW-5.4: a fetch failure leaves an existing vault untouched; empty tenant → marker-only vault; the clobber warning prints.
- [ ] DW-5.5: summary reports events/concepts/maps/ghosts/dropped counts.

---
## Test Coverage
**Level:** 100%

## Test Plan
- [ ] DW-1.1: proto+engrampb round-trip for `ExportEpisodic`; `make proto-check` (Unit).
- [ ] DW-1.2: cursor state machine — episodic(string cursor)→entities→edges, unknown-stage rejection, empty-tier same-response chain (Unit).
- [ ] DW-1.3: ACL — allowed/denied/error episodic; missing identity → Unauthenticated (Dirty).
- [ ] DW-1.4: episodic drained across pages; unprocessed/dead-lettered excluded (Unit + Dirty).
- [ ] DW-1.5: oversized-Text set → multiple byte-bounded pages (Boundary/Dirty).
- [ ] DW-2.1: `sanitizeBody` corpus — `---`, `> [!`, `[[`/`![[`, fence breakout, `<iframe>`/raw HTML, `obsidian://`/`data:`/`javascript:` (Dirty ≥5:1).
- [ ] DW-2.2: `quoteBlock` — blank lines prefixed, `[!x]`-leading line can't forge/exit callout (Dirty).
- [ ] DW-2.3: degree-based ghost rule; normalized-name collapse vs distinct-name-same-source non-collapse; duplicate event_id dedupe (Unit + Dirty).
- [ ] DW-2.4/3.4/4.1/5.3: determinism — identical input → identical output per layer + end-to-end (Unit).
- [ ] DW-3.1/3.2/3.3: event body + UTC/undated foldering; concept fact-sheet + folded quotes; ghost-only leaves; claim without quote (Unit + Dirty).
- [ ] DW-4.2/4.3/4.4: bounded misc buckets, large-component map, disconnected graph, empty graph, map contents + sanitized title (Unit + Boundary).
- [ ] DW-5.1/5.2/5.4: end-to-end vault write; nested path-escape refusal; fetch-failure leaves vault intact; empty tenant; clobber warning (Integration + Dirty).
- [ ] DW-5.5: summary counts (Unit).
- [ ] Manual: run `engram export` against live rtd (9201) via a host embed server; open in Obsidian; confirm graph view, folded quotes, and that no leaf-fragment field returned.

---
## Assumptions
| Assumption | Confidence | Verify Before Phase | Fallback If Wrong |
|---|---|---|---|
| A `search_after` tenant+live episodic scan can be added to `internal/store` mirroring `ScanLiveFacts` (facts.go:278) | Medium | Phase 1 | If the episodic index sort key is unstable, add an explicit deterministic sort field; worst case scan+sort in memory at this scale |
| Edge-level `ValidAt` suffices for per-claim chronology (no semantic-facts stage) | High (graph.go:110-113, on wire) | Phase 2 | Add a semantic export stage (deferred) if reconciliation makes edge ValidAt unreliable |
| Connected-components + bounded misc yields a usable map set on the real graph | Medium | Phase 4 | Tune `minMembers`/bucket cap or group by top-degree neighbor; correctness independent of count |
| Degree ≥2 hub cutoff yields a substantive (not empty) concept set | Medium | Phase 2 | Make the cutoff a constant that can drop to ≥1 if the hub set is too thin on real data |
| ~1.8k entities / 1.1k edges / 249 events fit in client memory | High | Phase 2 | Promote clustering to a server stage (documented fallback) |

## Decision Log
| Decision | Alternatives | Rationale | Phase |
|---|---|---|---|
| Client-side assembly (thin server) | Server-side projection; LLM synthesis | Presentation in delivery layer; iterable without redeploy; richness already on wire | All |
| Deterministic assembly, no LLM | LLM dossiers; hybrid enrich | User chose reproducible/trustworthy | 2,3 |
| Hub = edge degree ≥2 gets a file; degree ≤1 = ghost | mention_count threshold | mention_count is 1 for all entities; degree is the true hub signal; ghosting leaves is what prevents the fragment field | 2,3 |
| Collapse by **exact normalized name** | Shared source_id; fuzzy/embedding merge | source_id fuses all entities from one event (catastrophic); fuzzy is nondeterministic; normalized-name-equal is safe + deterministic | 2 |
| Separate `EpisodicExporter` seam | Widen the graph-bound `Exporter` | `Exporter` is implemented by `*graph.Store` which can't reach the episodic index; widening breaks the tier boundary | 1 |
| Maps→notes one-directional (no `**Map:**` footer) | Bidirectional footer | A footer needs Phase 4 output in Phase 3, breaking the parallel wave; Obsidian backlinks cover the reverse | 3,4 |
| Knowledge out of scope, seam tier-agnostic | Build knowledge stage now | Live knowledge tier empty, zero linkage — YAGNI; seam avoids later redesign | 1,2 |

---
## Notes
- `ExportEdge.Statement`/`ValidAt` are already on the wire — the concept fact-sheet needs no server change; only episodic does (why Phase 1 is episodic-only).
- Two distinct injection surfaces: `sanitizeBody` (untrusted prose as body content) and `quoteBlock` (safe wrapping of that prose into a callout). Both live in Phase 2 (Full gate) so Phase 3 stays a Standard consumer and 3∥4 remain a parallel wave.
- Clustering is re-specced for the *real* graph (hundreds of tiny components): the danger is a misc mega-note, not one giant component — hence size-bounded misc buckets, not label propagation.
- **Manual-edit loss:** a readable vault invites annotation, and re-export clobbers it. Phase 5 warns at export; a `--merge`/incremental mode is a deliberate future enhancement, not in this plan.
- Phases 3 and 4 have disjoint file scopes and depend only on Phase 2 — they run as a parallel wave during build.

---
## Execution Log

### Phase 1: Server — episodic export stage (Gate: Full)
- [x] BUILD: Discovery + design + implementation complete
- [x] REVIEW: 3-sample fable, PASS (3/3)
- [x] Committed
Commit: 0ae5ffa
Summary: Episodic records are now on the export wire via a separate `EpisodicExporter` seam (impl `*store.OpenSearchStore`), drained first in the stage machine; `engramclient.ExportPage.Episodics []ExportEpisodic{EventID,Kind,Text string; OccurredAt *time.Time; SourceIDs []string}` exposes them. Byte-budgeted (2 MiB) `search_after` scan, processed+non-dead-lettered+tenant-pinned, per-record `canExport` fail-closed.

### Phase 2: Client — vault model, refs, sanitizer + quoteBlock (Gate: Full)
- [x] BUILD: Discovery + design + implementation complete (+ security fix)
- [x] REVIEW: 3-sample fable, fail→pass (round 1 found a real bypass; round 2 PASS 3/3)
- [x] Committed
Commit: 3f28670
Summary: `buildVaultModel(episodics, entities, edges) (VaultModel{Events,Concepts}, VaultRefs)` — deterministic model; `Event{EventID,Title,Body,OccurredAt,ConceptIDs}` (deduped by event_id via OccurredAt/Kind/Text, since CreatedAt isn't on the wire), `Concept{EntityID,Name,Aliases,Degree,Claims,RelatedIDs,Ghost}` (Ghost=Degree<2, hubMinDegree=2 constant), `Claim{Statement,ValidAt,EdgeID,SourceEventID}`. Collapse on exact normalized-name equality only. `VaultRefs map[string]noteRef{File,Display,Folder}` is the shared link map. `sanitizeBody`/`quoteBlock` barricade at 100% coverage; control-char wikilink-recombination bypass fixed (scanner tracks emitted-rune state).

### Phase 3: Client — event + concept note rendering (Gate: Standard)
- [x] BUILD: implementation complete (+ prose-quote correction: renderConcept embeds source-event prose, signature gained `events map[string]Event`)
- [x] REVIEW: single-sample sonnet, PASS
- [x] Committed (wave member, cherry-picked)
Commit: 5010f3f
Summary: `renderEvent(Event, VaultRefs)` and `renderConcept(Concept, VaultRefs, events map[string]Event)` — pure renderers. Event notes: UTC-foldered path (nil→undated/), sanitized prose, Concepts footer. Concept notes: claims oldest-first (EdgeID tie-break), each Statement + folded `> [!quote]-` callout of the source event's sanitized prose, related + ghost links; degree≥2 only get files. 100% coverage; all untrusted fields sanitized. NOTE for Phase 5: renderConcept needs `VaultModel.Events` as a `map[string]Event`.

### Phase 4: Client — topic-map clustering + MOC notes (Gate: Standard)
- [x] BUILD: implementation complete (+ nesting flatten + 100% coverage fixes)
- [x] REVIEW: single-sample sonnet, fail→pass (round 1 flagged >3 nesting + <100% coverage; round 2 PASS)
- [x] Committed (wave member, cherry-picked)
Commit: c8debf8
Summary: `clusterConcepts(VaultModel) []Cluster` (deterministic connected-components; ≥minMembers → own map, smaller → size-bounded `misc-NN` buckets) and `renderMap(Cluster, VaultRefs)` (title = highest-degree member, sanitizeFilename'd, collision-suffixed with `misc-` reserved; member list + UTC event timeline + cross-cluster out-links). 100% coverage, max nesting 3. Wave-integration test (Phase 3+4) green.

### Phase 5: Client — vault assembly, CLI wiring, determinism (Gate: Full)
- [x] BUILD: implementation complete (+3 review-fix rounds: symlink guard, byte-budget, UTF-8 choke point)
- [x] REVIEW: 3-sample fable, fail→pass (4 rounds; caught & closed a symlink `rm -rf $HOME` bypass and a clobber-then-abort data-loss class via both length and encoding vectors)
- [x] Committed
Commit: fbf8378
Summary: `engram export <dir>` now writes the rich dual-primary vault (events/ + concepts/ + maps/); entity-per-note format removed. `fetchExport` drains episodics; `writeVault` assembles via buildVaultModel + the Phase 3/4 renderers under path confinement, atomic writes, marker/clobber guards, and a re-export-clobbers-edits warning. Symlinked-vault catastrophic-guard bypass fixed (EvalSymlinks); single `safeNoteName` choke point guarantees every basename is valid UTF-8, ≤255 bytes, illegal-char-free regardless of source field. Deterministic. Follow-up (non-blocking): NFC/NFD normalization collision could silently drop one note on APFS — worth an NFC fold in a later pass.
