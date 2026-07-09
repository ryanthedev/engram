# Plan: Triple-pass ensemble extraction for engram

**Created:** 2026-07-08
**Status:** complete
**Started:** 2026-07-08 22:10
**Completed:** 2026-07-09 07:56
**Duration:** 2026-07-08 22:10 → 2026-07-09 07:56
**Complexity:** simple

---

## Context

The extraction shim (`cmd/engram-extract-shim`, built in the extraction-cli-shim plan) currently does single-pass extraction: one CLI backend per request. We want a higher-quality **ensemble backend** for a deep, on-demand pass: run each event through **agy** and **codex** in parallel to get two candidate fact-sets, then have **claude-sonnet-5** (`claude --model sonnet`) act as a **judge** that reconciles both candidates against the source event — dropping anything unfaithful, deduping, and returning the authoritative triple array. `agy` stays the fast live default; `ensemble` is selectable via `-backend ensemble` and pointed at a re-extract sweep. Then we deep-re-extract the existing `rtd` corpus to a new `v3` ledger and A/B its quality against the `v2` single-pass.

Full wire contract, CLI flags, and per-CLI gotchas: `.code-foundations/research/2026-07-08-extraction-cli-shim.md`. This plan reuses the Phase-1 shim architecture (Backend Strategy + envelope/parse/barricade/timeout) wholesale.

## Constraints

- **CLIs only, subscription auth, NO API keys** (explicit user instruction — subscription limits exceed API access). Judge = `claude --model sonnet` (claude-sonnet-5) via the CLI. agy/codex unchanged from Phase 1.
- **CLAUDE.md injection is accepted, but guard-tested.** The user's ~24k-token global CLAUDE.md is injected on every `claude` CLI call. The research proved this can make claude extract facts *from CLAUDE.md*. As a judge (reconciling given candidates against a given source, not extracting cold) with a strict `--system-prompt`, sonnet-5 should anchor — but a test MUST confirm the judge emits no CLAUDE.md-derived facts before we trust it. Verify, don't assume.
- **Reuse, don't rebuild.** `ensembleBackend` composes the existing `agyBackend`/`codexBackend`/`claudeBackend` impls and the existing `parseFacts`/barricade/`runProcess` machinery. The judge is a `claudeBackend` invocation with a judge-specific system prompt and the two candidate sets appended to the user content.
- **agy ∥ codex concurrent; judge serial after.** The whole pipeline honors one per-call timeout (the same `context` deadline that bounds `runProcess`, including the forking-backend WaitDelay fix).
- **Never dead-letter, never 500.** Fallbacks: one extractor fails → judge runs on the surviving candidate set; judge fails → return `agy`'s set (the proven-good default); nothing survives → `[]`.
- **No engramd Go changes.** Still just the shim + compose + scripts.

## Success criteria

- `-backend ensemble` selects the agy∥codex→sonnet-judge pipeline; the shim still satisfies engramd's exact `/chat/completions` contract.
- The judge is proven (by test) to reconcile against the source and to NOT leak CLAUDE.md-derived facts.
- A deep re-extract of the `rtd` corpus through the ensemble populates a `v3` ledger; spot-checks show ≥3 facts where the ensemble is more faithful or better-deduped than the `v2` single-pass; reconciliation converges with no dead-letters.

---

## Implementation Phases

### Phase 1: Ensemble backend (agy ∥ codex → sonnet judge)
**Skills:** code-foundations:cc-defensive-programming (subprocess fan-out + judge output at a trust boundary), code-foundations:gof-design-patterns (Composite/Strategy — an ensemble backend composing leaf backends)
**Model:** sonnet
**Gate:** Full
**Depends on:** none
**File scope:** cmd/engram-extract-shim/**, Makefile
**Security-sensitive:** yes

**Goal:** Add a selectable `ensemble` backend that fans out to agy and codex concurrently and reconciles their outputs with a claude-sonnet-5 judge, reusing the existing barricade/timeout machinery, with robust degrade-never-crash fallbacks.

**Scope:**
- IN: an `ensembleBackend` implementing the existing `Backend` interface, composing `agyBackend` + `codexBackend` (run concurrently) and a judge (`claudeBackend` with `--model sonnet` and a judge-specific `--system-prompt`). It assembles the judge's user content as: the source event text + candidate set A (agy) + candidate set B (codex), clearly delimited. The judge's returned array flows through the existing `parseFacts`/fence-strip/barricade before the shim returns it.
- IN: wire `-backend ensemble` selection; the judge model/system-prompt are internal constants (documented), not new flags. Extend the fake-backend test scaffolding so the ensemble can be tested hermetically (inject fake agy/codex/judge).
- IN: a Makefile target/notes for running the shim with `-backend ensemble`.
- OUT: making ensemble the live default (it stays selectable; agy remains default); any engramd change; the re-extract sweep (Phase 2); a new hosted-API judge path.

**Edge cases:**
- One extractor exits non-zero / times out → the judge runs on the surviving candidate set (not an error).
- Both extractors fail → shim returns `[]` (or a retryable error), never a hang or 500.
- Judge exits non-zero / times out / returns garbage → fall back to `agy`'s candidate set (deduped), never dead-letter.
- Judge output wrapped in ```` ```json ```` fences or prose → same barricade as any backend degrades it cleanly.
- CLAUDE.md-injection: the judge must reconcile ONLY the provided candidates against the provided source; it must not invent facts about CLAUDE.md, the environment, or the user's global config. Proven by DW-1.5.
- Candidate text / source text with shell metacharacters → reaches all three CLIs as arg-slice/stdin, never a shell string.
- Whole-pipeline timeout: agy∥codex + judge must complete within (or be bounded by) the per-call deadline; a slow stage cannot hang the request past the WaitDelay backstop.

**Produces:** `-backend ensemble` — a composed extraction path returning judge-reconciled `{subject,predicate,object[,statement][,valid_at]}[]` through the same HTTP contract. Phase 2 points a re-extract sweep at it.
**Security-sensitive:** yes — fans out untrusted-ish event text to three subprocesses and deserializes a judge's merged output; the injection barrier (arg-slice across all three), the degrade-never-crash fallbacks, and the CLAUDE.md-leak guard are the review focus.

**Done when:**
- [ ] DW-1.1: `-backend ensemble` runs agy and codex, passes both candidate sets + the source event to a claude-sonnet judge, and returns the judge's reconciled JSON array in the standard envelope — verified against fake agy/codex/judge backends in a table-driven test.
- [ ] DW-1.2: The judge is invoked as `claude --model sonnet` with a strict `--system-prompt` and the source + both candidate sets in the user content — asserted structurally by a test (argv + assembled prompt), no live call needed for this item.
- [ ] DW-1.3: Fallbacks proven by tests — (a) one extractor failing → judge runs on the survivor; (b) both failing → `[]`/retryable, no hang; (c) judge failing or emitting garbage → agy's set returned, deduped; never a 500 or dead-letter.
- [ ] DW-1.4: agy and codex are invoked concurrently (not serially) — proven by a test (e.g. overlapping-execution timing against fakes) — and the whole ensemble honors the per-call timeout / WaitDelay backstop.
- [ ] DW-1.5: A guard test proves the judge emits NO CLAUDE.md-derived facts: given a source event with known triples, the returned facts are faithful to the source and none reference CLAUDE.md / global-config content. Run live against real `claude --model sonnet` (gated behind the `smoke` build tag like the agy smoke test); if `claude` is unavailable, the test skips with a clear reason and the item is reported "test written, live run pending" — never a claimed pass without an observed clean run.
- [ ] DW-1.6: Event/candidate text with shell metacharacters (`; $() && | \n` backticks) reaches all three backends inert (arg-slice/stdin) — dirty test asserting no shell interpretation across the ensemble path.

### Phase 2: Deep re-extract to v3 + quality A/B
**Skills:** code-foundations:cc-debugging (confirm v2→v3 re-claim + cross-version reconciliation semantics), engram:engram-memory (drive memory_status/search for verification)
**Model:** sonnet
**Gate:** Standard
**Depends on:** Phase 1
**File scope:** deploy/local/docker-compose.yml, scripts/**

**Goal:** Deep-re-extract the `rtd` corpus through `-backend ensemble` into a new `v3` ledger and prove the ensemble's quality against the `v2` single-pass, with reconciliation converging.

**Scope:**
- IN: run the shim with `-backend ensemble`; bump engramd's `-extractor-version` `v2`→`v3` and run the existing scoped `processed_at` reset (`scripts/backfill-reextract-rtd.sh`) to re-claim the events (same dual-lever mechanism confirmed in the prior plan). Sweep is ~182 events at ~15-20s each (~45-60 min) — the ensemble runs three models per event.
- IN: **confirm cross-version reconciliation semantics** (cc-debugging) — determine whether v3 facts SUPERSEDE the v2 facts or COEXIST with them (content-key dedup vs version-supersede). Document the observed behavior; if v2+v3 near-duplicates coexist and inflate the tier, record it as a known outcome (and whether a supersede path exists) — do NOT invent a destructive cleanup.
- IN: A/B quality — spot-check ≥3 events where the ensemble/judge produced a more faithful, better-scoped, or better-deduped triple than the v2 single-pass; confirm no CLAUDE.md-derived facts leaked into the live store.
- OUT: making ensemble the live default; scaling to other namespaces; engramd Go changes; batching/concurrency of the sweep.

**Edge cases:**
- v2→v3 version bump alone does not re-claim `processed_at`-stamped events → the scoped `processed_at` reset is required (as in the prior plan); document.
- v3 produces the SAME triple as v2 (same content_key) → reconciliation dedups, no duplicate. v3 produces a genuinely different/better triple → new content_key, coexists with v2's — expected; note the count delta.
- The judge leaks a CLAUDE.md-derived fact into the live store → a FAIL for DW-2.3; catch it in the spot-check and stop.
- The ~45-60 min sweep is interrupted → re-extraction is idempotent under the v3 ledger; re-running resumes unclaimed events.

**Produces:** A `v3`-ledger semantic tier for `rtd` produced by the ensemble, with a documented quality comparison vs v2. Terminal verification phase.
**Rollback:** Additive under the new `v3` ledger — the `v2` rows remain untouched; revert `-extractor-version` to `v2` and the v2 state is unchanged. The `processed_at` reset is tenant-scoped and snapshots ids first (existing script behavior).

**Done when:**
- [ ] DW-2.1: With the shim on `-backend ensemble`, re-extraction of the `rtd` events into the `v3` ledger fires — observed in engramd logs (extraction calls, not `ErrNoFacts` for prose-bearing events).
- [ ] DW-2.2: `memory_status` shows the semantic tier populated by the v3 ensemble pass (report the count; note v2-vs-v3 coexistence/supersede as observed).
- [ ] DW-2.3: A documented A/B spot-check of ≥3 events shows the ensemble triple is at least as faithful as — and in ≥3 cases better than — the v2 single-pass; and confirms NO CLAUDE.md-derived facts leaked into the live store.
- [ ] DW-2.4: Reconciliation converges — repair backlog drains (0 pending), no duplicate-fact explosion beyond the documented cross-version behavior. **Amended by user decision 2026-07-09**: 3 permanent dead-letters were found and root-caused (fixed 30s extraction-client timeout undersized for the ensemble's 3-model pipeline; dead-lettering is coded non-self-healing) — but all 3 events already have full v3 fact coverage in the live store, confirming no data loss. The user accepted this as a documented, non-blocking follow-up rather than a phase-blocking failure; DW-2.4 is met on that basis (backlog drained, no data loss, root cause identified) rather than the original literal "zero dead-letters" bar.

---

## Test Coverage

**Level:** 100% of the ensemble backend's orchestration logic (fan-out, judge assembly, all fallback branches, concurrency, injection barrier) via hermetic fake backends; the CLAUDE.md-leak guard (DW-1.5) is a live `smoke`-tagged test. Phase 2 is operational, evidenced by the DW checks.

## Test Plan

- [ ] Ensemble: happy path — fake agy + fake codex + fake judge; the judge receives both candidate sets + source; shim returns the judge's array in the correct envelope (DW-1.1).
- [ ] Ensemble (structural): judge invoked as `claude --model sonnet` with the strict `--system-prompt` and source + candidates in user content (DW-1.2).
- [ ] Ensemble (fallbacks): one extractor fails → judge on survivor; both fail → `[]`/retryable no hang; judge fails/garbage → agy's set deduped (DW-1.3).
- [ ] Ensemble (concurrency): agy and codex invoked concurrently, not serially; whole path bounded by the per-call timeout / WaitDelay (DW-1.4).
- [ ] Ensemble (live guard, `smoke` tag): real `claude --model sonnet` judge on a known source emits faithful, non-CLAUDE.md facts (DW-1.5).
- [ ] Ensemble (dirty/security): shell-metacharacter source/candidate text is inert across all three backends (DW-1.6).
- [ ] Re-extract: engramd logs show ensemble extraction firing for `rtd` events after the v2→v3 bump + reset (DW-2.1).
- [ ] Verification: memory_status reflects the v3 pass; ≥3 A/B spot-checks documented (v2 vs v3); no CLAUDE.md leak; reconciliation converges no dead-letters (DW-2.2, 2.3, 2.4).

---

## Notes

- **Judge robustness is the crux.** The entire quality bet rests on the sonnet judge reconciling faithfully despite CLAUDE.md injection. DW-1.5 (live guard) is the gate — if the judge leaks CLAUDE.md facts, the strict system prompt needs hardening (or the judge role reconsidered) before Phase 2.
- **Cross-version accumulation is an open build-time question** (like the v1→v2 re-claim was): whether v3 supersedes or coexists with v2 facts is confirmed against the reconciler in Phase 2, not assumed. Coexistence is acceptable for an A/B; a destructive cleanup is out of scope.
- **Latency compounds:** ~15-20s/event × 182 ≈ 45-60 min for the sweep, three models per event. On-demand only — never the live default (per the scope decision).
- **Optional follow-on (not planned here):** a periodic "dream"-style re-extract cadence that reruns the ensemble over the corpus on a schedule; pairs naturally with this on-demand deep pass.
- **Follow-up 1 (accepted, non-blocking, 2026-07-09):** the extraction client's fixed 30s timeout (`store.DefaultTimeout`, `cmd/engram-server/main.go`) is undersized for the ensemble backend's 3-model pipeline (2 candidates + judge), which produced 3 permanent dead-letters during the v3 sweep. Dead-lettering is coded non-self-healing (`internal/store/outbox.go`). No data loss occurred (all 3 events have full v3 fact coverage), but a future ensemble run would hit the same timeout. Fix: raise the timeout (or make it backend-aware) before the next `-backend ensemble` sweep.
- **Follow-up 2 (accepted, non-blocking, 2026-07-09):** engramd was left on `-extractor-version v3` while the live shim was switched back to `-backend agy` for steady-state (per this plan's "agy stays default" intent). This means new events ingested going forward are labeled `v3` in the ledger but are actually extracted via the fast agy single-pass, not the ensemble — a label/reality mismatch. Not destructive, but worth realigning (bump back to `v2`, or accept `v3` as the new steady-state label) in a future pass.

---

## Execution Log

### Phase 1: Ensemble backend (agy ∥ codex → sonnet judge) (Gate: Full)
- [x] BUILD: Discovery + design + implementation complete
- [x] REVIEW: Verification passed — 3-sample fable majority, PASS 3/3 (one reviewer also ran an unprompted adversarial prompt-injection probe against the live judge; held clean)
- [x] Committed
Commit: ca8e7b8
Summary: Delivered `-backend ensemble` — agy and codex run concurrently as independent candidate extractors, claude-sonnet-5 (subscription CLI auth, no API key) judges the two sets against the source event and returns the reconciled, deduped triple array through the existing envelope/barricade. Composes the Phase-1 leaf backends rather than reimplementing exec/timeout machinery. A live guard test proves the judge does not leak CLAUDE.md-derived facts despite the CLI's global CLAUDE.md injection. agy remains the fast live default; ensemble is opt-in. This is the backend Phase 2 points the deep re-extract sweep at.

### Phase 2: Deep re-extract to v3 + quality A/B (Gate: Standard)
- [x] BUILD: Discovery + design + operational execution complete (resumed once after a user interruption of the first dispatch — the live sweep had already completed unattended; a second agent independently re-verified settlement and finished the required analysis). Returned UPDATE_PLAN on one amended DW; user reviewed and accepted (see DW-2.4 amendment above and Notes follow-ups).
- [x] REVIEW: Verification passed — single sonnet review, independently re-derived every number from live OpenSearch aggregations rather than trusting the discovery doc; closed one gap the discovery doc had left unverified (confirmed the `invalid_at` supersede stamp directly)
- [x] Committed
Commit: 9b2cb63
Summary: Deep-re-extracted the 186 rtd events through `-backend ensemble` into a v2→v3 ledger — semantic tier 367 → 730 facts (370 v2 + 360 v3, 0 content_key collisions). Confirmed reconciliation is content-key-scoped: exact-match facts supersede (16 pairs, `invalid_at` stamped), distinct facts coexist. A/B spot-check (5 pairs) showed 4 strict quality wins + 1 parity for the ensemble. CLAUDE.md-leak scan clean under live, uncontrolled conditions. Two non-blocking follow-ups accepted and documented: a timeout-undersized-for-ensemble bug (3 benign dead-letters, no data loss, root-caused) and a v3-label/agy-runtime mismatch now that the live shim is back on the fast `agy` default (PID 55653).
