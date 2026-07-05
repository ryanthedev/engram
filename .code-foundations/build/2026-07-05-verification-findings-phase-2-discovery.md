# Discovery + Design: Phase 2 - Graph dedup — make the dev embedder cluster same entities

## Files Found
- `internal/graph/dedup.go` — `Deduper.Decide`, `Weights`/`DefaultWeights` (0.7 embed / 0.3 lex), `Candidate`, `Judge`/`RuleJudge`. No `Scope` field on `Candidate`.
- `internal/graph/dedup_test.go` — `TestHomonym_SameNameDifferentEntityStaysUnmerged` (lines 89-108) feeds `Decide` two hand-built `Candidate`s with orthogonal embeddings directly; never calls `Store` or an embedder.
- `internal/graph/store.go` — `Store.UpsertMention` (candidate fetch → `Decide` → merge/create), `Store.embed` (computes the mention's embedding from `m.Context` — the FULL fact statement, not the name), `MemBackend` (unit-test `Backend`).
- `internal/graph/opensearch.go` — `OpenSearchBackend.CandidateEntities`: filters by `tenant_id` only (no scope filter); `_search` reads use the existing `osJSON`/`osDo` helpers (Phase-1's 404-as-empty guard already applied here per the Phase-1 execution log).
- `internal/graph/graph.go` — `Mention{Name, Context, Scope, TenantID, ...}`, `Entity{Scope, ...}`, `normalizeName`, `Fingerprint`.
- `internal/graph/stage.go` — `Stage.Process`: for each fact, upserts a Subject mention and (if present) an Object mention, BOTH with `Context: f.Statement` — this is the real ingestion path and the mechanism that reproduces finding #2 (the SAME entity, e.g. "B" in an A→B→C chain, is mentioned once as the Object of fact 1 and again as the Subject of fact 2, each time with a DIFFERENT `f.Statement`).
- `internal/embed/fake.go` — `FakeEmbedder.Embed`: `sha256(text or fixture-key) → seeded PRNG → unit vector`. Critically, this is a literal hash of the whole input string: two input strings differing by even one token produce uncorrelated (effectively orthogonal-looking) vectors. There is no partial/fuzzy similarity for near-but-not-identical strings — only exact-string equality reliably yields equal vectors.
- `internal/embed/embedder.go` — `Embedder` interface (`Embed`, `Info`); `internal/embed/http.go` — `HTTPEmbedder`, the production real-model client.
- `internal/graph/store_test.go` — `newTestStore` helper (MemBackend + `RuleJudge` + `FakeEmbedder`); `TestHomonymDisambiguation_ThroughStore` (already existing, passes today) exercises two "Jordan" mentions with UNRELATED context end-to-end through `Store.UpsertMention` and asserts 2 distinct entities.
- `internal/graph/opensearch_integration_test.go` — `newLiveStore` helper; existing `TestDW_6_2_Integration_TwoHopConnectTheDots` builds A/B/C by calling `UpsertMention` for B only ONCE (context "A works_at B") and never re-mentions B with fact 2's different context — it wires the edge B→C directly off the already-known `bID`. This is exactly the "fixture that sidesteps dedup" the plan's Context section calls out: it does not exercise the fragmentation bug at all.
- `cmd/engram-server/stages_graph.go` (`wireGraph`) / `cmd/engram-server/main.go` — the ONE call site that decides `embed.FakeEmbedder` (no `-embed-url`) vs `embed.HTTPEmbedder` (real model), then constructs `graph.NewStore(backend, dedup, embedder, logger)`.
- `internal/acl/acl.go` — `ScopePrivate`/`ScopeTeam`/`ScopeOrg` constants (graph package doesn't import acl; `Scope` is carried as an opaque string on `Mention`/`Entity`).

## Current State
`Store.embed` always feeds the mention's FULL fact context (`m.Context` = the fact's `Statement`) to whichever `Embedder` is configured, both in production and in dev/e2e. `Deduper.Decide` (unchanged, and correctly so) blends 0.7×embed-sim + 0.3×lex-sim. Under a REAL semantic embedder this is exactly right: same-entity-different-context mentions land close in embedding space (shared meaning) while true homonyms (same name, unrelated meaning) land far apart — `TestHomonym_SameNameDifferentEntityStaysUnmerged` encodes this by feeding `Decide` hand-built orthogonal vectors directly (it never touches `Store.embed` or any real/fake embedder).

Under the deterministic `FakeEmbedder`, embedding full context breaks this: it is a pure hash of the exact string, so ANY two different context strings — even two mentions of the exact same entity — land at ~0 cosine similarity, indistinguishable from a true homonym. `Decide`'s combined score for same-entity-different-context mentions under `FakeEmbedder` sits at/below the split threshold (0.45), so they never merge. This is finding #2: the dev/local stack cannot demonstrate ≥2-hop connect-the-dots because the SAME entity re-mentioned across two facts (exactly what `Stage.Process` does for any entity that appears in more than one fact) fragments into multiple entity docs, breaking the edge chain a 2-hop traversal needs.

`CandidateEntities` (both `MemBackend` and `OpenSearchBackend`) filters candidates by `tenant_id` only — `Scope` is not part of the merge boundary anywhere today, so a private-scope and a team-scope entity sharing an exact normalized name in one tenant could merge into each other if their embeddings/lexical scores happened to clear the merge threshold (a latent gap the plan-review flagged, not yet exploited by any failing test but a real boundary hole).

## Code Standards
`docs/code-standards.md` conventions applied: wrapped errors + sentinels (`fmt.Errorf(...: %w...)`, `ErrNotFound`/`ErrDepthExceeded`-style sentinels), `context.Context` first param, deep modules with doc comments explaining WHY not just WHAT, consumer-defined interfaces (`Backend`, `Judge`), no OpenSearch types crossing package boundaries, table-driven + ≥1 dirty test per phase, `log/slog` structured logging. The existing `graph` package is itself a strong exemplar (functional-option constructors like `NewExpander`/`NewOpenSearchBackend`, `_ Interface = (*Impl)(nil)` compile-time assertions) — this phase's additions follow the same idioms already in the file (functional option on `Store` mirrors `ExpanderOption`/`BackendOption`).

## Test Infrastructure
Go `testing`, table-driven where natural, `t.Helper()` on constructor helpers (`newTestStore`, `mustDeduper`, `newLiveStore`). Unit tests in `internal/graph/*_test.go` run under plain `go test ./...` (no build tag) against `MemBackend`; integration tests carry `//go:build integration` and run against the real local OpenSearch cluster via `make integration`, using `testutil.OpenSearchURL()`/`testutil.HTTPClient` and scratch per-test indices (`scratchBackend`). `FakeEmbedder` (dim from `store.EmbeddingDim`) backs both tiers — no real embedding service exists in this repo/environment.

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-2.1 | Regression test: same entity name in two facts, different fact context → exactly ONE entity doc under the deterministic dev embedder (fails before, passes after) | COVERED | `TestDW_2_1_NameKeyedDedup_SameEntityDifferentFactContextMerges` (new store-level unit test using a name-keyed `Store`); proven fails-before/passes-after by reverting `store.go` and re-running (see Validation section) |
| DW-2.2 | Integration test: 3-fact A→B→C chain answers a 2-hop query returning C from an A-anchored query, on the local stack | COVERED | `TestDW_2_2_Integration_NameKeyedDedup_TwoHopThroughRealStage` (new integration test) — drives the REAL `graph.Stage.Process` over two facts so entity B is independently re-mentioned with two different `Statement` contexts (exactly the verifier's failing scenario), then runs the real `MultiRetriever` + `Expander` pipeline and asserts C is reached |
| DW-2.3 | DW-6.3 preserved: entity count stable across 10 re-ingests | COVERED | Existing `TestDW_6_3_RepeatedIngestEntityCountStable` / `TestDW_6_3_Integration_RepeatedIngestEntityCountStable` continue to pass UNCHANGED (default, non-name-keyed store); NEW `TestDW_2_3_NameKeyedDedup_RepeatedIngestEntityCountStable` proves the same stability holds under the new name-keyed dev mode too |
| DW-2.4 | `TestHomonym_SameNameDifferentEntityStaysUnmerged` passes UNCHANGED; new test: private vs shared same-name entities in one tenant stay separate | COVERED | `TestHomonym_SameNameDifferentEntityStaysUnmerged` is not touched (verified: it calls `Decide` directly, never `Store.embed`, so it is structurally unaffected by any change here); NEW `TestScopePreFilter_PrivateAndTeamSameNameStaySeparate` (identical context+name, different Scope → must NOT merge; would merge before the scope pre-filter, proving the boundary) |
| DW-2.5 | ACL still honored at expansion (no regression to DW-6.4); `make integration` green | COVERED | Existing `TestDW_6_4_ExpansionACLBlocked` untouched (Expander/ACL code not modified by this phase); `make integration` run at the end of Validate |

**All items COVERED:** YES

## Design Decisions

### Design: Mention-embedding input for dedup clustering

#### Approaches Considered
1. **Universal name-weighted mention text** (the plan's stated first preference): change `Store.embed`'s input for ALL embedders (dev fake AND production real) from `m.Context` to a name-dominant string (e.g. the name repeated/prepended before the context).
2. **Dev-only name-keyed embedding, opt-in `Store` option**: leave `Store.embed`'s default behavior (embed `m.Context`) completely untouched; add a `StoreOption` (`WithNameKeyedDedup`) that, when enabled, embeds `normalizeName(m.Name)` instead. Wired ON only when the configured embedder is literally `*embed.FakeEmbedder` (dev/e2e); production (`embed.HTTPEmbedder`) never enables it.
3. **Decisive-merge shortcut in `Decide` for exact-name matches**: explicitly REJECTED by the plan (breaks the homonym test, regresses real-embedder homonym separation) — not seriously considered here, listed for completeness.

#### Comparison
| Criterion | 1. Universal name-weighted text | 2. Dev-only opt-in option | 3. Decisive merge in Decide |
|-----------|---|---|---|
| Interface simplicity | No new API, but changes what "the mention embedding" means everywhere | One new functional option (`StoreOption`), mirrors existing `ExpanderOption`/`BackendOption` idiom already in this package | None — but rejected by plan |
| Information hiding | `Store.embed`'s formatting choice leaks into production embedding semantics silently | The choice is explicit, named, and documented at the option site; default behavior (production) is provably unchanged | N/A |
| Real-embedder homonym safety | **Unsafe.** `FakeEmbedder` is a literal hash of the WHOLE input string — there is no "partial weighting" for a hash-based embedder: two strings that share a dominant substring still hash to ~orthogonal vectors unless they are byte-identical. So "name-weighted but still includes context" text would NOT cluster under `FakeEmbedder` at all (defeats the fix), while making the input MORE name-dominant for a REAL embedder measurably shrinks the context signal that disambiguates homonyms — directly risking the production guarantee `TestHomonym_...` encodes in spirit (even though that specific unit test bypasses `Store.embed` and would still pass mechanically) | **Safe.** Production embedding input is byte-for-byte unchanged; only the dev/e2e path (which never carried real homonym-disambiguation guarantees to begin with — it's a deterministic hash fixture) changes | N/A |
| Caller ease of use | No caller change, but the meaning of "the mention embedding" changes under the hood for every deployment | `wireGraph` needs a two-line, self-contained decision (`if _, ok := embedder.(*embed.FakeEmbedder); ok { ... }`) — no new parameter threaded through, no config flag to keep in sync | N/A |
| Matches plan's fallback authorization | N/A (this IS the primary option) | Plan explicitly authorizes this as the fallback "whichever keeps `Decide` AND the homonym test unchanged" | N/A |

#### Choice: 2 — Dev-only opt-in `StoreOption` (`WithNameKeyedDedup`), wired only when the concrete embedder is `*embed.FakeEmbedder`
Rationale: `FakeEmbedder.Embed` is `sha256(exact input string)` — a genuine hash, not a bag-of-words or attention-weighted embedding. There is no such thing as "name-weighted, context still present" for a hash function: either the input string is character-for-character identical across mentions (→ identical vector, clusters) or it differs at all (→ effectively random, uncorrelated vector, does not cluster). This means Approach 1 (keep some context in the text, just prepend the name) literally cannot fix the dev-embedder fragmentation — the string would still differ per fact and still hash orthogonally. The ONLY way to make `FakeEmbedder` cluster same-entity mentions is to feed it the exact same string every time: the normalized name ALONE, with context fully excluded. But excluding context from the embedding input for a REAL semantic embedder would be a substantive production regression: it removes exactly the signal that lets a real embedder tell "Jordan the country" from "Jordan the athlete" apart. So the one string transformation that actually fixes the dev/fake-embedder path (name-only, no context) is simultaneously the one transformation that would be unsafe to apply universally. This resolves the plan's own flagged uncertainty ("whether a name-weighted mention embedding alone clusters same-entity while preserving the homonym unit test") in favor of its documented fallback: keep production's embedding input (`m.Context`) completely untouched, and add an explicit, narrowly-scoped, opt-in dev/e2e mode.
Sacrificed: dev-stack homonym separation (accepted and documented — see Edge Cases/Constraints below); a marginally larger API surface (one new exported option function) versus "no new symbols at all."

### Depth Check
- Interface methods added: 1 (`WithNameKeyedDedup() StoreOption`); `NewStore` gains a variadic `opts ...StoreOption` tail — every existing call site (`stages_graph.go`, `store_test.go`, `opensearch_integration_test.go`) compiles unchanged (no signature break).
- Hidden details: the fact that `FakeEmbedder` is a literal hash function (not a fuzzy embedding) and therefore requires byte-identical input to cluster; the decision of WHEN to enable the option (tied to embedder identity, not a separately-threaded boolean that could drift out of sync with reality).
- Common case complexity: simple — a caller who does nothing gets today's exact behavior; only `wireGraph`'s already-existing embedder-selection branch grows two lines.

### Design: (tenant, scope) merge boundary

`Candidate` (the `Decide`-facing type) carries no `Scope` field, and the plan explicitly directs the boundary to live in `UpsertMention`, not `Decide` — preserving `Decide`'s existing signature/contract (which the homonym test and every other `dedup_test.go` test call directly). Concretely: in `UpsertMention`, when building the `candidates` slice from `backend.CandidateEntities`'s (tenant-only-filtered) result, add `if e.Scope != m.Scope { continue }` alongside the existing `if !e.Live() { continue }` liveness filter. This is a one-line, single-responsibility addition to a routine that already loops over `existing` to build `candidates` — it does not change `UpsertMention`'s cohesion (still "resolve m to a canonical entity") and does not touch `Decide`'s signature or internals.

## Prerequisites
- [x] Required files exist (`internal/graph/dedup.go`, `store.go`, `graph.go`, `stage.go`, `opensearch.go`, `internal/embed/fake.go`, `cmd/engram-server/stages_graph.go`/`main.go`)
- [x] Dependencies available (local OpenSearch 3.1.0 up, green; `make integration` runnable)
- [x] Phase 1 (read-path 404-as-empty) already committed — graph's OpenSearch reads already benefit from it

## Recommendation
BUILD.

What actually needs to be done:
1. `internal/graph/store.go`: add `Store.nameKeyedDedup bool` field, `StoreOption`/`WithNameKeyedDedup()`, make `NewStore` variadic-option-accepting (non-breaking), change `Store.embed` to take the `Mention` (not just `m.Context`) and choose `normalizeName(m.Name)` vs `m.Context` based on the flag, and add the `e.Scope != m.Scope` pre-filter in `UpsertMention`'s candidate-building loop.
2. `cmd/engram-server/stages_graph.go`: in `wireGraph`, detect `*embed.FakeEmbedder` via type assertion and pass `graph.WithNameKeyedDedup()` only in that case; log the decision.
3. Tests: new unit tests in `internal/graph/store_test.go` (DW-2.1 merge-under-name-keying, DW-2.3 stability-under-name-keying, DW-2.4 scope pre-filter) and one new integration test in `internal/graph/opensearch_integration_test.go` (DW-2.2, driven through the real `Stage.Process` + `Expander` + `MultiRetriever` pipeline, matching the existing `TestDW_6_2_...` test's shape but deliberately re-mentioning the middle entity with two DIFFERENT fact contexts).
4. Prove fails-before/passes-after for DW-2.1/DW-2.2/DW-2.4 by stashing the `store.go`/`stages_graph.go` changes, confirming the new tests fail (or fail to compile, for the option itself) against the pre-fix code, then restoring and confirming green.
