# Discovery + Design: Phase 2 - Graph edge lifecycle + echo dedup

## Files Found
- `internal/graph/graph.go` — `Edge.InvalidAt` (:114), `Edge.Live()` (:121), `edgeFingerprint()` (:184)
- `internal/graph/store.go` — `Store.UpsertMention` (:176), `Store.UpsertEdge` (:264), `Backend.GetEdge`/`PutEdge`, `MemBackend.Neighbors` (:482 — already skips `!e.Live()`)
- `internal/graph/stage.go` — `Stage.Process` (:51), already receives `[]ingest.FactOutcome` from Phase 1
- `internal/graph/expand.go` — the visited-set seeding (:107-113) vs the comparison (:134), `anchorEntities` (:176)
- `internal/graph/opensearch.go` — `PutEdge` (:217), `GetEdge` (:227), `Neighbors` (:250, `must_not: exists invalid_at`)
- `internal/graph/templates/edges.json:23` — `invalid_at` mapped as `date`
- `internal/ingest/ingest.go:91` — `FactOutcome{Fact, Decision, Predecessor}` + the late-arrival invariant
- `internal/ingest/reconciler.go:60` — predecessor selection: **same Subject AND same Predicate**, live head

## Current State
- `Edge.InvalidAt` is declared and read by `Live()`, but **no non-test code sets it**. Confirmed:
  `grep -rn "InvalidAt" internal/graph/*.go` → only the struct field, `Live()`, and test fixtures.
- `Stage.Process` upserts every triple and ignores `o.Decision` / `o.Predecessor` entirely.
- `expand.go:109-113` seeds `visitedEdges` with `h.ID` — a **semantic doc id** — then compares it at
  `:134` against `edge.ID`, a **sha256 edge fingerprint**. Two disjoint id spaces: the guard can
  never fire. Observed consequence: a seed fact's own edge is re-served as an "expanded" graph hit.

## Gaps
| Gap | Plan assumption | Reality |
|---|---|---|
| `CloseEdge` | Does not exist | Composes from `Backend.GetEdge` + `PutEdge` — **no Backend method needed** (plan's CHECK note verified) |
| `Neighbors` filtering | Already excludes closed edges | Confirmed on BOTH backends (Mem `!e.Live()`, OS `must_not exists invalid_at`) — zero changes outside `internal/graph/**` |
| Predecessor → entity ids | Not addressed | `Stage` needs a **read-only** mention resolution (candidate lookup + `Deduper.Decide`, no write) — `UpsertMention` would mutate `MentionCount`/`SourceIDs`. Extracting the read half of `UpsertMention` is the fix. |
| `CloseEdge` timestamp | Signature `(ctx, tenantID, edgeID)` — no time arg | Must use `s.now()`. Acceptable: nothing queries edges as-of a valid time; `InvalidAt` is only read by `Live()`. Documented on the method. |

## Assumption Verification (plan, MEDIUM confidence)
> "The predecessor fact's triple is enough to recompute its edge fingerprint. If an entity-merge can
> make a predecessor's fingerprint irrecoverable, fall back to closing by (from_entity, predicate)."

**VERIFIED — the assumption HOLDS; the fallback is NOT needed.** Evidence:

1. **Entity ids are stable under merge.** `mergeEntity` (store.go:248) is the LiCoMemory
   hyperlink-not-duplicate pattern: `merged := existing`, then it only *grows* accounting fields
   (`MentionCount`, `Aliases`, `SourceIDs`). `ID`, `Name`, `CreatedAt`, `Embedding` are preserved.
   A merge therefore never invalidates a previously-derived entity id, so
   `edgeFingerprint(tenant, from, predicate, to)` stays reachable.
2. **Re-resolving the predecessor's mention re-finds its entity.** `UpsertMention`'s own documented
   contract (store.go:173-175): "a repeated mention with identical Context always re-finds and
   re-merges into the same entity". Re-resolving `(p.Subject, p.Statement)` and `(p.Object,
   p.Statement)` is exactly that lookup — same name key, byte-identical context ⇒ identical
   embedding (embedders are deterministic functions of their input) ⇒ `Decide` merges to the same
   entity. The stage reuses that machinery read-only rather than reimplementing resolution.
3. **The fingerprint is directional and exact** — it can only close the edge the predecessor itself
   wrote, never a sibling.

**Why the fallback was actively REJECTED, not merely unneeded.** Closing by `(from_entity,
predicate)` over `Neighbors` **over-closes**: entity dedup can map two *different* subject strings
onto one entity (e.g. "Acme" and "Acme Corp" merge). The reconciler keeps one live head per
(subject-STRING, predicate), so `(Acme, owns, X)` and `(Acme Corp, owns, Y)` are both legitimately
live semantically — yet they share a from-entity and a predicate in the graph. A
(from_entity, predicate) close would retire the innocent sibling edge. Exact-fingerprint close
cannot. If resolution ever misses (a homonym created in between outscores the original), the failure
mode is fail-safe: `CloseEdge` no-ops on a missing id, leaving a stale edge — never closing a wrong one.

## Code Standards
- `docs/code-standards.md` present. Applicable rules:
  - **Never hard-delete / blind-overwrite** — `CloseEdge` is a soft close (`InvalidAt` stamp) and
    never removes a doc. Matches the semantic store's guarded-close convention.
  - **No transport/framework imports in a business package** — this phase touches only
    `internal/graph`, importing `internal/{ingest,memory,embed,retrieval,auth}`. Boundary holds.
  - Doc comments explain *why* (the package's prevailing style); errors wrapped with `%w` and a
    `graph:` prefix.

## Test Infrastructure
- Stdlib `testing`, table-free explicit tests, `MemBackend` + `embed.NewFakeEmbedder(8, nil)` +
  `RuleJudge` via `newTestStore(t)` / `newTestStage(t)` (store_test.go:13, stage_test.go:30).
- `added(facts...)` helper (stage_test.go:22) wraps facts as `OpAdd` outcomes — Phase 2 adds
  `superseded(...)` for the UPDATE/INVALIDATE shape.
- `semanticHit(...)` (expand_test.go:16) builds seed hits; it does **not** set `predicate`, so
  existing expand tests contribute no seed fingerprint and are unaffected by the DW-2.3 fix.
- Run: `make test` (`go test ./...`), `make lint`, `make build`.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-2.1 | Superseding a fact sets `InvalidAt` on the predecessor's edge; a test asserts the edge is no longer `Live()` | COVERED | `TestDW_2_1_UpdateClosesPredecessorEdge` (OpUpdate), `TestDW_2_1_InvalidateClosesPredecessorEdge` (retraction/OpInvalidate) — both fetch the predecessor edge by fingerprint from `MemBackend.GetEdge` and assert `InvalidAt != nil` && `!Live()` |
| DW-2.2 | A superseded edge is absent from `Store.Neighbors` and from search results | COVERED | `TestDW_2_2_SupersededEdgeAbsentFromNeighbors` (Neighbors returns only the new edge), `TestDW_2_2_SupersededEdgeAbsentFromExpansion` (Expander over the post-supersession graph never emits the old object) |
| DW-2.3 | A seed fact's own edge is never returned as an expanded hit (visited set seeded with fingerprints, not doc IDs) | COVERED | `TestDW_2_3_SeedEdgeNotReturnedAsExpandedHit` (seed hit A-works_at->B; expansion must not re-serve the A->B edge, must still serve B->C) |
| DW-2.4 | Closing an edge is idempotent — replaying an event does not error or double-close | COVERED | `TestDW_2_4_CloseEdgeIsIdempotent` (direct `CloseEdge` ×3: no error, `InvalidAt` unchanged after the first; missing edge id is a no-op), `TestDW_2_4_ReplayedOutcomeDoesNotClose` (`OpReplayed` never closes) |
| DW-2.5 | An UPDATE whose new object resolves to the same entity does not close the edge it just wrote | COVERED | `TestDW_2_5_UpdateToSameEntityDoesNotCloseItsOwnEdge` (predecessor and successor objects dedup to one entity ⇒ identical fingerprint ⇒ edge stays `Live()` and stays in `Neighbors`) |

**All items COVERED:** YES

Beyond the DW floor (edge cases named in the phase Scope): `TestStage_LateArrivalDoesNotCloseLiveEdge`
(the Phase-1 gotcha), `TestStage_PredecessorWithNoEdgeIsNoOp` (retraction predecessor / soft-expired
entity), `TestCloseEdge_UnknownIDIsNoOp`, `TestExpand_EpisodicSeedContributesNoFingerprint`.

## Design Decisions

**1. `Store.CloseEdge(ctx, tenantID, edgeID) error`** — the plan's pinned signature. Read-modify-write
over the existing `Backend.GetEdge` + `PutEdge`; no `Backend` method added (the port stays narrow).
Idempotent by construction: a missing edge and an already-closed edge both return `nil` without a
write. Soft only — sets `InvalidAt = now()`, never deletes.

**2. Read-only mention resolution — `Store.resolveMention` (unexported).** `UpsertMention`'s
lookup half (candidate fetch → scope-boundary filter → `Deduper.Decide`) is extracted into a shared
private helper that writes nothing. `UpsertMention` keeps its exact behavior on top of it; `Stage`
uses it to recover the predecessor's entity ids. Unexported because `stage.go` is in `package graph` —
no new public surface, so the module stays deep. Rejected alternative: calling `UpsertMention` for the
predecessor — it would inflate `MentionCount`/`SourceIDs` on every supersession, corrupting the
DW-6.3 entity-stability metric.

**3. `Stage.Process` gains one guarded step per outcome, after the upsert.** Close runs *after* the
new edge is written so the just-written edge id is known (DW-2.5's guard). Five short-circuits, in
order — this is the entire lifecycle contract:
   1. `o.Predecessor == nil` → nothing was superseded (OpAdd / OpNoop / **OpReplayed**) → DW-2.4.
   2. `o.Decision` not UPDATE/INVALIDATE → belt-and-braces against the invariant drifting.
   3. `p.Subject == "" || p.Object == ""` → the predecessor asserted no edge (retraction) → no-op.
   4. `o.Fact.ValidAt.Before(p.ValidAt)` → **the Phase-1 late-arrival gotcha**: `insertHistorical`
      reports OpUpdate with a predecessor it deliberately did NOT close, because the newly-landed
      fact is OLDER. Closing here would retire a still-live fact.
   5. predecessor fingerprint `== newEdgeID` → the UPDATE's new object resolved to the same entity;
      closing would kill the edge just written → DW-2.5.
   Then: resolve → fingerprint → `CloseEdge`. Unresolvable subject/object ⇒ no edge exists ⇒ no-op.

**4. `expand.go`: seed the visited set with fingerprints, not doc ids.** `anchorEntities` already
resolves each seed hit's subject/object names to candidate entities — it now returns the seeded
`visitedEdges` set alongside the anchors, computed from the cross-product
`subjectIDs × objectIDs` under the hit's own `predicate` field, each mapped through the *same*
`edgeFingerprint` the store keys edges by. One `CandidateEntities` call per distinct name (cached),
so the fix costs no extra backend round-trips. A hit with no `predicate` (or no subject/object —
the episodic tier) contributes no fingerprint, which is correct: it asserted no edge.

## Prerequisites
- [x] Phase 1's `FactOutcome` seam landed and `graph.Stage` compiles against it
- [x] `Backend.GetEdge` / `PutEdge` exist on both backends; `invalid_at` mapped in the edge template
- [x] `Neighbors` already excludes non-live edges on both backends

## Defect found during implementation (SEARCH step) — `UpsertEdge` resurrected closed edges

Grepping for every writer of `InvalidAt` (cc-debugging step 7: defects cluster) surfaced a latent
bug that would have silently defeated DW-2.1 and DW-2.2 in production.

`Store.UpsertEdge` rebuilds the `Edge` struct on every upsert and carried forward only
`CreatedAt` / `ValidAt` / `SourceIDs` — **never `InvalidAt` or `ExpiredAt`**. Because an edge's doc id
is a pure function of its triple, re-ingesting the *original* event lands an upsert on the very doc
the correction closed. Reproduced with a throwaway probe before touching any code:

```
ev-1 "service-a owns billing-db"      -> edge live
ev-2 supersedes it with billing-db-v2 -> edge closed   (InvalidAt set)
ev-1 REDELIVERED (at-least-once)      -> edge LIVE again (InvalidAt = <nil>)   <-- zombie
```

The stale relation returns to search and the supersession is undone — the exact symptom this phase
exists to kill, reachable through nothing more exotic than at-least-once delivery. It also violates
`docs/code-standards.md`'s "never blind-overwrite" rule directly.

**Fix:** `UpsertEdge` now carries both bi-temporal stamps forward, discriminating replay from
re-assertion by valid time — the only thing that separates them, since a replay can only ever carry
the valid time already recorded:
- `spec.ValidAt` **not after** `existing.ValidAt` (a replay) ⇒ preserve `InvalidAt`: never reopen.
- `spec.ValidAt` **strictly after** (a genuine later re-assertion of a retracted relation) ⇒ revive:
  clear `InvalidAt`, advance `ValidAt`. Mirrors `experience.Store`'s re-proven soft-expire revival
  (`internal/experience/store.go:210`), so the edge tier stays retired-not-tombstoned.
- `ExpiredAt` is preserved unconditionally — a soft-expire is not undone by a re-mention.

Covered by `TestDW_2_4_ReplayOfSupersededEventDoesNotResurrectEdge` and
`TestUpsertEdge_ReAssertionRevivesClosedEdge` (both sides of the discriminator). In scope: it is
inside `internal/graph/**`, and without it DW-2.1/DW-2.2 hold only until the first redelivery.

**Other SEARCH results (clean):** `internal/worker/repair.go:397`'s `visited` set is a single id
space (fact doc ids on both sides) — not a sibling of the expander's mismatch. `Store.CloseEdge` is
now the only production writer of `Edge.InvalidAt`. Every `edgeFingerprint` call site agrees on one
id space. **Noted, not chased (out of scope):** no production code sets `Entity.ExpiredAt` either —
entity soft-expire is unwired, exactly as edge closing was. `expand.go` already handles a
soft-expired entity defensively (the dangling-edge skip), so nothing is broken today; flagging it as
a candidate for a later phase, not a Phase-2 gap.

## Recommendation
**BUILD** — the plan fits reality, the MEDIUM-confidence assumption verified as sound (and its
fallback verified as *worse*), and nothing outside `internal/graph/**` needs to change.
