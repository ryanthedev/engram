# Plan: Fix Verification Findings — Production Hardening

**Created:** 2026-07-05
**Status:** in-progress
**Started:** 2026-07-05 (branch fix/verification-findings)
**Current Phase:** 1
**Complexity:** medium
**Source:** three code-foundations e2e verifier subagents driving the live local stack (2026-07-05). Their captured-evidence findings are the requirements below.

---

## Context

**Problem:** Fresh-eyes end-to-end verification of Engram (all 8 phases merged) confirmed the product works — bi-temporal supersession, auth barricade, ACL scope matrix, experience gating, eval gates all verified with real captured output — but surfaced five defects the passing test suite masked, because the suite seeds semantic data by *writing* first and builds graph fixtures that sidestep dedup. This plan fixes all five to reach production-ready.

**The findings (severity as assessed by the verifiers):**
1. 🔴 **Read-before-write on a not-yet-created index hangs reconciliation.** On any instance whose configured index does not already exist (scratch/isolated/first-deploy with `-semantic-index` overridden, or simply a fresh cluster reached by a read before a write), the reconciler's candidate search 404s with `index_not_found_exception`; no read path treats that as "empty", so the first fact retries forever. Confirmed independently by all three verifiers. Masked on default names only because `store.Apply` eagerly creates the hardcoded defaults.
2. 🟡 **Graph multi-hop can't be exercised under the dev/deterministic embedder.** The same entity mentioned in two facts gets two separate entity docs, so ≥2-hop connect-the-dots finds nothing at the second hop. **Corrected root cause (from plan review):** the production dedup logic is *correct* — it blends embedding similarity (0.7) with lexical (0.3) precisely so a real semantic embedder clusters same-entity-different-context mentions (merge) while keeping true homonyms apart (`TestHomonym_SameNameDifferentEntityStaysUnmerged` at dedup_test.go:89-108 encodes this, and it must keep passing). The failure is that the **deterministic dev embedder hashes each mention's full context**, so the same entity across two facts gets orthogonal vectors indistinguishable from a homonym — a dev-fixture limitation, NOT a production dedup bug. An unconditional exact-name merge in `Decide` would break the homonym test and regress real-embedder behavior. The fix belongs in the dev embedding path, not `Decide`'s core rule.
3. 🟡 **Gate-verdict-rate metric is in-process only** — resets on restart and never reflects the durable OpenSearch state.
4. 🟡 **No graph gauge on `/metrics`** despite a code comment implying the entity-count-stability metric is wired to telemetry.
5. ⚪ **CLI UX**: the `fact: subject | predicate | object` extractor grammar is pipe-delimited but unhinted; a malformed line silently yields zero facts with no feedback; and `engram ingest`'s `--event-id` "idempotency" wording overstates what is deduped (the episodic log is not).

**Success criteria:** every finding has a fix with a regression test that fails before and passes after; `make lint`/`make test`/`make integration`/`make e2e` all green; a fresh custom-index-named engramd reconciles its first fact with no manual index bootstrap; a two-hop chain of identical-named entities connects; the gate-verdict and a graph gauge survive a restart / appear on `/metrics`; the CLI gives feedback on a zero-fact ingest.

## Constraints

- Honor `docs/code-standards.md`: wrapped errors + sentinels, `context.Context` first, deep modules, consumer-defined interfaces, OpenSearch types out of public signatures, table-driven + ≥1 dirty test per phase, `log/slog`.
- Fixes must not weaken any verified guarantee: bi-temporal correctness, the auth barricade, ACL fail-closed enforcement, the mandatory experience gate, and the eval gates all stay intact — each phase's REVIEW confirms no regression against them.
- Bi-temporal/ACL/gate correctness is data-integrity critical: the read-path and graph phases carry Full gates.

## Chosen Approach

**Defensive read paths + decisive-signal dedup + durable telemetry + CLI feedback**, one phase per concern. For finding #1, fix it at the robustness layer (treat `index_not_found_exception` as an empty result in every read path) AND ensure the server materializes its *configured* index names on boot — belt and suspenders, because relying on eager default-index creation is exactly what broke. **Fallback:** if treating 404-as-empty proves too broad, scope it to the reconciliation/outbox/sweep read paths only and keep explicit index creation as the primary fix.

## Rejected Approaches

- **Only make `store.Apply` create the configured index names** (not the 404-as-empty guard): leaves the latent "a read reaches a fresh index first" class open for any future index; the defensive guard is the durable fix.
- **Unconditional exact-name decisive merge in `Deduper.Decide`** (original Phase-2 approach, rejected after plan review): breaks the passing homonym test and regresses real-embedder homonym separation — the production logic is correct, so the fix must not touch `Decide`'s core rule.
- **Raise the lexical weight so exact names dominate**: same problem — weakens homonym separation under any embedder; the weights are tuned for a real embedder and must stay.

---

## Implementation Phases

### Phase 1: Read-path index robustness (finding #1)
**Model:** fable
**Skills:** cc-defensive-programming, aposd-designing-deep-modules
**Gate:** Full
**Depends on:** none
**File scope:** `internal/store/**, cmd/engram-server/**`

**Goal:** A fresh engramd whose configured indices do not yet exist reconciles its first fact with no manual bootstrap — every read path treats a missing index as an empty result, and the server materializes its configured index names on startup.

**Scope:**
- IN: treat `index_not_found_exception` as an empty result (not an error, not an infinite retry) at EVERY OpenSearch `_search`/`_count` read site that can touch a not-yet-created index. Enumerated from the plan review — cover all of: reconciliation `Candidates()`, repair-sweep scans (`LiveSuperseders`, `DuplicateLiveContentKeys`, `LiveByContentKey`, `FindByEventID`, `ScanIncomplete` in ledger.go, **`ClosedOverlapChainKeys` — rule-d, facts.go:264-321**), `ValidTimeNeighbors`, the outbox `ClaimBatch`, the enrichment `_search` (`enrich.go:39`), and the telemetry `_count`/`_search` in `counts.go`/`lag.go`. Implement via a shared 404-detector helper applied at each `doJSON` read call site (there are ~6 inline sites separate from any shared `searchFacts` helper — guarding one wrapper is NOT sufficient; each inline site must use the detector). Make server boot ensure the *configured* (`-episodic/-semantic/-ledger-index`) index names exist, not only the hardcoded defaults — PUT the templates FIRST (already done by `Apply`) then create the configured names so they inherit the `engram-*` template mapping (knn_vector + bi-temporal); the configured names MUST match the template patterns (`engram-episodic*` etc.) or they get a wrong dynamic mapping.
- OUT: changing the bi-temporal write protocol; graph/experience logic (other phases).

**Constraints:** the guard must match ONLY the specific `index_not_found_exception` shape (OpenSearch error `type`), never a blanket "any 404 is fine" that could hide a real missing-doc bug on a `_doc/{id}` GET where absence is already handled distinctly. Documented correctness-vs-robustness trade: 404-as-empty turns a mistyped index name from a loud failure into silent empty-reads + an auto-created wrong-named index — mitigated by boot-ensuring the configured names exist (a typo then surfaces as a created-but-wrong index visible in `_cat/indices`, and the names are validated against the template pattern).
**Edge cases:** index missing on first read → empty, worker proceeds to ADD (dirty test); index exists but empty → empty; a real transport 404 vs `index_not_found_exception` distinguished; concurrent first-write index auto-create race (already tolerated by `Apply`) still holds.
**Produces:** the read paths return `(empty, nil)` on `index_not_found_exception`; `store.Apply`/`EnsureIndices` creates the configured index names. **Contract:** no public signature change — `Candidates`/sweep/outbox keep their signatures; behavior on a missing index changes from error to empty.
**Security-sensitive:** no
**Done when:**
- [ ] DW-1.1: a regression test boots the reconcile/candidate path against a semantic index name that does not exist and asserts it returns empty candidates (not an error) — fails before the fix (reproduces the 404), passes after.
- [ ] DW-1.2: an end-to-end style test (integration) with an overridden, never-before-seen semantic index name ingests one event and the derived fact becomes retrievable without any manual index PUT.
- [ ] DW-1.3: server boot ensures the configured `-semantic/-episodic/-ledger-index` names exist (asserted against a scratch name); re-running is idempotent.
- [ ] DW-1.4: dirty test — the guard distinguishes `index_not_found_exception` from other error shapes (a genuine transport error on a read still propagates as an error).
- [ ] DW-1.5: `make integration` green including the new tests; no regression in existing store/worker tests.

**Difficulty:** MEDIUM
**Uncertainty:** exact set of read paths — the build agent enumerates every OpenSearch `_search`/`_count` call reachable before a first write and covers each; fallback is to centralize the guard in one shared search helper.

### Phase 2: Graph dedup — make the dev embedder cluster same entities (finding #2)
**Model:** sonnet
**Skills:** cc-routine-and-class-design, aposd-designing-deep-modules
**Gate:** Full
**Depends on:** none
**File scope:** `internal/graph/**, internal/embed/**`

**Goal:** The local/dev stack can actually demonstrate ≥2-hop connect-the-dots — the same entity across two facts resolves to one node under the deterministic embedder — **without changing `Deduper.Decide`'s production logic or breaking the homonym test.** Production homonym separation (a real-semantic-embedder property) is preserved and documented.

**Scope:**
- IN: fix the dev-stack fragmentation at its real root — the mention-embedding input. For dedup similarity, embed a **name-weighted** representation of the entity mention (normalized entity name as the dominant signal, not the full surrounding fact context) so a deterministic embedder yields the *same* vector for the same normalized name across different facts → they cluster and merge through the EXISTING weighted `Decide` rule. This is a real improvement (dedup compares the entity, and the entity is its name — the fact context is precisely what differs between two facts about one entity). If a name-weighted mention embedding cannot both cluster same-entity AND preserve the homonym unit test, instead add a deterministic-test-embedder mode keyed on normalized name used only by the graph dedup / dev + e2e path, leaving production embedding untouched — pick whichever keeps `Decide` and the homonym unit test unchanged.
- Also (plan-review finding): enforce the (tenant, scope) merge boundary in **`UpsertMention`** (pre-filter candidates by scope before calling `Decide`), NOT inside `Decide` — so `Decide`'s signature contract holds and a private+shared same-name merge cannot conflate distinct-scope entities.
- OUT: changing `Deduper.Decide`'s decision rule or weights; changing the production real-embedding contract; >2-hop; community summarization.

**Constraints:** `TestHomonym_SameNameDifferentEntityStaysUnmerged` (dedup_test.go:89-108) MUST keep passing UNCHANGED — it feeds `Decide` hand-built orthogonal embeddings and asserts no-merge; the fix must not alter that logic. Document explicitly that homonym separation depends on a real semantic embedder, and that the deterministic dev embedder (clustering on name) will merge dev-stack homonyms — an accepted dev-fixture limitation, not production behavior.
**Edge cases:** same entity, two facts, different fact context → one entity under the dev embedder (the finding); the homonym unit test (orthogonal embeddings fed directly to `Decide`) still yields two entities; repeated identical ingest keeps entity count flat (DW-6.3); private vs shared same-name entities in the same tenant stay separate (scope pre-filter); dangling edge skipped at expansion.
**Produces:** the dev/deterministic embedder clusters same-normalized-name mentions so multi-hop connects locally; scope boundary enforced in `UpsertMention`. **Contract:** `Deduper.Decide` unchanged; `UpsertMention` gains an internal scope pre-filter (no exported-signature change); the mention-embedding input becomes name-weighted.
**Security-sensitive:** no
**Done when:**
- [ ] DW-2.1: regression test — under the deterministic dev embedder, the same entity name in two facts with different fact context produces exactly one entity doc (fails before: two docs; passes after: one).
- [ ] DW-2.2: integration test — a 3-fact A→B→C chain answers a 2-hop connect-the-dots query returning the C node from an A-anchored query (the verifier's exact failing scenario, now passing on the local stack).
- [ ] DW-2.3: DW-6.3 preserved — entity count stable across 10 re-ingests of the same fact set.
- [ ] DW-2.4: `TestHomonym_SameNameDifferentEntityStaysUnmerged` still passes UNCHANGED (Decide logic intact); a new test asserts private vs shared same-name entities in one tenant stay separate (scope pre-filter in UpsertMention).
- [ ] DW-2.5: ACL still honored at expansion (no regression to DW-6.4); `make integration` green.

**Difficulty:** MEDIUM
**Uncertainty:** whether a name-weighted mention embedding alone clusters same-entity while preserving the homonym unit test — if not, fall back to a graph-dedup-only deterministic test embedder keyed on normalized name (production untouched). The build agent picks the option that leaves `Decide` and the homonym test unchanged and records which in the discovery doc.

### Phase 3: Durable + complete telemetry (findings #3, #4)
**Model:** sonnet
**Skills:** cc-quality-practices, aposd-designing-deep-modules
**Gate:** Standard
**Depends on:** none
**File scope:** `internal/telemetry/**, internal/graph/**, internal/experience/**, cmd/engram-server/**`

**Goal:** Gate-verdict rates survive a restart (reflect durable state, not just in-process counters), and a graph gauge appears on `/metrics`.

**Scope:**
- IN: add **durable inventory gauges** for the experience tiers — derived from a cheap per-poll `_count` of the admitted and quarantine indices — so a restarted server reports the real live composition instead of process-lifetime counters that reset to 0. **Because `Reject` drops the record (store.go:110) there is no durable reject trace**, and admitted/quarantine `_count` measures *current live inventory* (admitted docs get pruned/soft-expired; quarantine deleted on release), NOT cumulative verdict rate — so name and describe the new gauges honestly as inventory (e.g. `engram_experience_admitted_count`, `engram_experience_quarantined_count`), NOT as an "admit rate". Keep the existing in-process verdict-rate counters as-is (they measure per-process verdict flow) but correct their gauge descriptions to say "in-process since start, resets on restart" so no metric's description misstates what it computes. Wire a graph gauge (all-tenant entity count, the DW-6.3 stability signal) into the recorder so it renders on `/metrics` and moves as entities are added — note `CountEntities` is currently per-tenant (a source edit adds an all-tenant count).
- OUT: new eval metrics; dashboards (Phase-8 territory); changing gate logic; resurrecting a durable reject count (rejects leave no trace by design).

**Constraints:** metric collection is read-only against OpenSearch and must not perturb gate/graph behavior; the poll must be cheap (a `_count` per gauge on the existing metrics-interval cadence) — no per-request scans. Every gauge's description must match what it actually computes (inventory vs in-process rate).
**Edge cases:** empty/missing index → `_count` reads 0, not error (relies on Phase 1's 404-as-empty); restart with pre-existing admitted+quarantined docs → the durable inventory gauges reflect them, not 0 (the in-process rate gauges legitimately reset — documented); metrics scrape before first poll → gauge absent-or-0 documented.
**Produces:** durable inventory gauges (`engram_experience_admitted_count`/`_quarantined_count`) + corrected descriptions on the in-process rate gauges; an `engram_graph_entity_count` gauge on `/metrics`. **Contract:** new gauge names are additive; existing gauge NAMES unchanged (only descriptions corrected).
**Security-sensitive:** no
**Done when:**
- [ ] DW-3.1: test — the durable experience-inventory gauges reflect the admitted/quarantined `_count` after a simulated restart (fresh recorder reading existing store state), not 0; the in-process rate gauges' descriptions state they reset on restart.
- [ ] DW-3.2: test — a graph entity gauge is registered and its value tracks the entity count (increments as entities are added).
- [ ] DW-3.3: `/metrics` integration assertion — both the durable gate gauge and the graph gauge appear with non-garbage values.
- [ ] DW-3.4: dirty test — gauge poll against an empty/missing index yields 0, not an error or a crash.
- [ ] DW-3.5: `make integration` green; no regression to existing telemetry (DW-7.8) gauges.

**Difficulty:** MEDIUM
**Uncertainty:** whether to derive gate rates live from the store each poll vs seed a counter at boot — build agent chooses the cheaper correct option; default to a `_count`-per-poll if it stays within the metrics cadence budget.

### Phase 4: CLI ingest UX (finding #5)
**Model:** sonnet
**Skills:** cc-defensive-programming, code-clarity-and-docs
**Gate:** Standard
**Depends on:** none
**File scope:** `internal/cli/**, internal/ingest/**, internal/experience/**`

**Goal:** A user driving `engram ingest` learns the extractor grammar, gets an advisory when a directive-looking line is malformed, and reads accurate `--event-id` semantics.

**Scope:**
- IN: document the pipe-delimited `fact: subject | predicate | object` / `retract:` / `experience:` grammar in the CLI usage text and near the ingest command; the advisory fires ONLY on a **malformed directive-looking line** — a line that starts with `fact:`/`retract:`/`experience:` but fails to parse — and stays SILENT on plain prose (plain prose is the correct, expected input for the production LLM extractor `HTTPExtractor`, so warning on "no directive found" would cry wolf on every legitimate production ingest — do NOT do that); correct the `--event-id` help text to state precisely what is idempotent (derived semantic facts are content-key deduped; the raw episodic log is appended per call).
- OUT: changing the extractor grammar itself; changing the async pipeline; warning on prose that lacks a directive.

**Constraints:** no false synchronous guarantee — the advisory is client-side and advisory only. Reuse the authoritative parsers so the CLI and server agree on "well-formed": `fact:`/`retract:` live in `internal/ingest/rule.go`, `experience:` lives in `internal/experience/distill.go` — export/reuse both rather than duplicating grammar (this means Phase 4 touches `internal/experience` for the parser reuse — see File scope).
**Edge cases:** a correctly-formed `fact:`/`experience:` line → no warning; a malformed (e.g. space-delimited) `fact:` line → advisory naming the expected pipe grammar; plain prose with no directive → NO warning (valid for LLM extraction).
**Produces:** CLI usage + ingest help updated; a client-side advisory only when a directive-looking line is malformed; corrected `--event-id` wording. **Contract:** no wire-protocol change; advisory is client-side only.
**Security-sensitive:** no
**Done when:**
- [ ] DW-4.1: test — the ingest command warns (advisory, non-fatal) when its text contains a directive-looking line (`fact:`/`retract:`/`experience:`) that fails to parse, and stays SILENT on both well-formed directives and plain prose with no directive.
- [ ] DW-4.2: CLI usage/help text documents the pipe-delimited grammar and accurate `--event-id` semantics (asserted by a test scanning the help output).
- [ ] DW-4.3: dirty test — a space-delimited (malformed) `fact:` line triggers the advisory naming the correct grammar; a prose-only text triggers nothing.
- [ ] DW-4.4: `make test`/`make lint` green; no change to successful-ingest behavior.

**Difficulty:** LOW
**Uncertainty:** none material — the parsers in `internal/ingest/rule.go` and `internal/experience/distill.go` are the single sources of truth; the advisory reuses them (export a small `IsDirectiveLine`/`ParseDirective`-style predicate from each) rather than duplicating grammar.

---

## Test Coverage
**Level:** 100% — every DW item tested, ≥1 dirty test per phase, each fix carries a regression test that fails before and passes after.

## Test Plan
- [ ] P1: 404-as-empty unit test on candidate path (DW-1.1); overridden-fresh-index e2e ingest→retrieve (DW-1.2); boot-ensures-configured-indices idempotency (DW-1.3); index_not_found vs other-error discrimination dirty test (DW-1.4); make integration (DW-1.5).
- [ ] P2: same-name-two-contexts → one entity under dev embedder (DW-2.1); A→B→C 2-hop connect e2e (DW-2.2); 10× re-ingest entity-count stable (DW-2.3); homonym unit test still passes unchanged + scope pre-filter separates private/shared same-name (DW-2.4); ACL-at-expansion no-regression (DW-2.5).
- [ ] P3: durable inventory gauge post-restart reflects `_count`, in-process gauges' descriptions corrected (DW-3.1); graph entity gauge tracks count (DW-3.2); /metrics shows durable inventory + graph gauge (DW-3.3); empty/missing-index gauge → 0 dirty test (DW-3.4); telemetry no-regression to DW-7.8 gauges (DW-3.5).
- [ ] P4: malformed-directive advisory + silent on prose AND on well-formed (DW-4.1); help-text documents grammar + event-id (DW-4.2); malformed-line advisory + prose-silent dirty test (DW-4.3); lint/test green (DW-4.4).

---

## Assumptions
| Assumption | Confidence | Verify Before Phase | Fallback If Wrong |
|---|---|---|---|
| `index_not_found_exception` is the exact OpenSearch error shape on a read to a missing index | HIGH (verifiers observed it verbatim) | 1 | match on HTTP 404 + the error `type` field |
| Exact normalized-name merge won't wrongly conflate real homonyms at S1 | MED | 2 | keep provenance-based homonym separation; make exact-name decisive only within same source lineage |
| Gate-verdict rate can be derived from a cheap `_count` per poll | HIGH | 3 | seed an in-process counter from a one-time boot scan |

## Decision Log
| Decision | Alternatives | Rationale | Phase |
|---|---|---|---|
| 404-as-empty guard (shared detector at every read site) AND boot-ensures-configured-indices | either alone | belt-and-suspenders; relying on eager default creation is what broke; the guard must cover ~6 inline read sites, not one wrapper | 1 |
| Fix graph fragmentation in the dev EMBEDDING (name-weighted mention), NOT in `Decide` | unconditional exact-name merge in Decide (rejected — breaks the homonym test); raise lexical weight | production dedup logic is correct; the fake embedder was the root cause; keeps homonym separation for real embedders | 2 |
| Durable INVENTORY gauges (not "admit rate"); keep in-process rate gauges but correct their descriptions; no durable reject count | rebuild a durable verdict-rate | rejects leave no durable trace; a `_count` measures live inventory not cumulative rate — name it honestly | 3 |
| Scope boundary enforced in `UpsertMention`, not `Decide` | pass scope into Decide (breaks signature contract) | keeps `Decide` signature; candidates pre-filtered by scope | 2 |
| Client-side advisory lint reusing the server's directive parser | duplicate grammar in CLI; server-side sync validation | single source of truth; honest about async | 4 |

---

## Notes
- Phases 2, 3, 4 all touch `internal/graph` and/or `internal/experience` (P2 graph+embed; P3 adds an all-tenant graph entity-count — a source edit, not read-only, correcting the earlier "read-only" note; P4 reuses the experience directive parser). Run **serially** in order — the overlaps mean no safe parallel wave anyway, and the Full gates don't share waves. Keep **Phase 1 first**: its 404-as-empty guard is what lets P3's inventory `_count` gauges read 0 on a fresh/empty index instead of erroring (the graph/experience indices aren't flag-overridable, so absent P1 they'd hit an existing empty index → 0, but P1 still hardens the general case).
- Each phase's REVIEW must confirm no regression to the verified guarantees (bi-temporal, auth, ACL, gate, eval) — the fixes are additive/defensive, not changes to those contracts.
- This plan is seeded by the e2e verification report (2026-07-05); the findings are the confirmed requirements.

---

## Execution Log
_To be filled during /code-foundations:build_

## Execution Log

### Phase 1: Read-path index robustness (Gate: Full) — Commit $(git rev-parse --short HEAD)
- [x] BUILD: isIndexNotFound guard at all 14 read sites + EnsureIndices boot-ensure
- [x] REVIEW: PASS (reviewer reran fail-before/pass-after; auth/ACL correctly unguarded)
- [x] Committed
Summary: The infinite-reconcile-retry bug is fixed — a fresh/overridden semantic index now yields empty candidates instead of a 404 loop, and the server materializes its configured index names on boot. Auth/ACL barricade reads stay loud on a missing index (security). Bi-temporal + worker suites unregressed.

### Phase 2: Graph dev-embedder clustering (Gate: Full) — Commit 4c1973a
- [x] BUILD: WithNameKeyedDedup dev-only override + UpsertMention scope pre-filter
- [x] REVIEW: PASS (dedup.go + homonym test byte-identical to Phase 6; dev-only gate; fail-before/pass-after reproduced)
- [x] Committed
Summary: Local/dev multi-hop connect-the-dots now works — the FakeEmbedder-only name-keyed dedup clusters same-named entities so A→B→C connects, while production HTTPEmbedder embedding and the homonym guarantee are untouched. Scope boundary enforced in UpsertMention. No regression to graph ACL (DW-6.4).
