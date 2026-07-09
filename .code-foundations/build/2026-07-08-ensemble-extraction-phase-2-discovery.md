# Discovery + Design: Phase 2 - Deep re-extract to v3 + quality A/B

## Files Found
- `internal/store/outbox.go` — `ClaimBatch` (scan-and-claim gate), `Complete`, `DeadLetter`.
- `internal/worker/worker.go` — `ProcessEvent` (ledger claim/extract/reconcile), `stampFacts`, `reconcileFact`.
- `internal/worker/repair.go` — `Sweeper` (rules a/a'/b/c/d convergence).
- `internal/ingest/reconciler.go` — `RuleReconciler.Reconcile` (content-key/subject+predicate decision rules).
- `internal/memory/ids.go` — `ContentKey`, `FactDocID`, `LedgerKey.DocID`.
- `internal/memory/record.go` — `SemanticFact` field layout.
- `cmd/engram-server/main.go` — `-extractor-version` flag (line 63), wired into worker `Config.ExtractorVersion` (line 155).
- `scripts/backfill-reextract-rtd.sh` — tenant-scoped `processed_at` reset with pre-mutate snapshot.
- `deploy/local/docker-compose.yml` (worktree copy) — engramd command args, currently `-extractor-version v2` (committed at `30718a3`).
- `/Users/r/repos/engram/deploy/local/docker-compose.yml` (main repo, **not** this worktree) — confirmed via `podman inspect local-engramd-1` label `com.docker.compose.project.config_files` to be the actual file the running `local` compose project reads. Currently has an **uncommitted** working-tree diff on branch `main` identical in content to the worktree's committed v2 state — this is the live operational copy.

## Current State
- Shim PID 47514 running (`ps`: 1h+ elapsed), listening on :8088, `-backend agy` (default), answering `{"status":"ok"}` on `/health`.
- `local-engramd-1` container up ~1h, healthy, `-extractor-version v2`, `-extract-url http://host.docker.internal:8088`.
- `memory_status` baseline (queried live, not assumed): `episodic_count=186`, `semantic_count=370`, `tenant_id=rtd` — slightly above the plan's "~185/367" estimate, consistent with the guardrail's expectation of concurrent growth.
- engramd logs show only a periodic idle cost-estimator line (`extraction cost model=gpt-4o-mini events=186`) — no active extraction traffic, consistent with the v2 sweep having already converged (all 186 events have `processed_at` stamped under `v2`).

## Gaps
- None structural. `scripts/backfill-reextract-rtd.sh` is already fully generic (tenant-scoped, not v1/v2-specific) — it needs **no code changes**, only a rerun with a fresh timestamped snapshot file. The plan's phrase "adapt" overstates what's needed; reuse verbatim.
- The main-repo compose file (operational) is presently uncommitted relative to `main`. I will edit it directly (mirrors how the prior v1→v2 bump was applied) and mirror the same edit into the worktree's committed copy so the branch stays in sync, without committing on `main` myself.

## Code Standards
No `docs/code-standards.md` found in the repo. Followed existing Go conventions observed in the read files (doc comments citing design decision IDs, `w.logger.*Context` structured logging, `fmt.Errorf` wrapping with `%w`).

## Test Infrastructure
This phase is operational/verification-only (per the plan and DW-IDs) — no new Go test files are in scope. "Tests" for this phase are the DW-ID evidence checks themselves (log observation, `memory_status`/`memory_search` calls, spot-check comparisons), consistent with the plan's Test Plan section ("Phase 2 is operational, evidenced by the DW checks").

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases / Evidence |
|-------|---------------|--------|------------------------|
| DW-2.1 | Ensemble re-extraction fires, observed in engramd logs (extraction calls, not ErrNoFacts for prose-bearing events) | COVERED | `podman logs local-engramd-1 -f` tailed during/after the processed_at reset + v3 bump; look for per-event extraction activity (shim request volume climbing on :8088, or absence of `ErrNoFacts` for known prose events) |
| DW-2.2 | `memory_status` shows semantic tier populated by v3 pass; count reported; coexistence/supersede noted | COVERED | `memory_status` MCP call before and after sweep; delta count reported; classified per the empirical reconciliation semantics below |
| DW-2.3 | ≥3 documented A/B spot-checks show ensemble ≥ v2, and ≥3 are better; no CLAUDE.md leak | COVERED | `memory_search` on ≥3 source events pre-identified from the corpus, comparing v2 vs v3 facts verbatim; explicit grep-style scan of returned hits for CLAUDE.md/RTK.md-derived subjects/objects |
| DW-2.4 | Reconciliation converges — no dead-letters, no duplicate-fact explosion beyond documented behavior, repair backlog drains | COVERED | Poll engramd logs for dead-letter warnings (`event dead-lettered`) during and ~2min after the sweep; the compose `-sweep-interval 2s` and repair `Sweeper` rules a-d bound convergence to the documented ≤5min SLO |

**All items COVERED:** YES

## Design Decisions

### 1. Re-claim mechanism — CONFIRMED, not merely trusted
Read `outbox.go` `ClaimBatch` directly: the scan gate is `must_not exists(processed_at)` and `must_not dead_lettered` — **no notion of `extractor_version` at all**. Read `worker.go` `ProcessEvent`: the ledger key is `{TenantID, EventID, ExtractorVersion}` (`memory.LedgerKey`), and the replay short-circuit (`!entry.Claimed && state.Phase == LedgerComplete`) fires whenever a ledger entry for that exact key already exists complete. Consequence, confirmed independently of the prior plan's script comment (which says the same thing, and is corroborated rather than trusted blindly):
- Bumping `-extractor-version` alone does **not** requeue already-`processed_at`-stamped events — the outbox gate never even looks at version.
- Clearing `processed_at` alone would requeue the events, but under the *same* `v2` ledger key they'd hit `LedgerComplete` and short-circuit without re-extracting.
- **Both levers are required together**: scoped `processed_at` reset (`scripts/backfill-reextract-rtd.sh`, reused verbatim) + `-extractor-version v2→v3` bump (new ledger key ⇒ fresh `ClaimLedger` ⇒ real extraction, not a cache hit).

### 2. Cross-version reconciliation semantics — CONFIRMED empirically by reading reconciler.go + ids.go, not assumed
`memory.ContentKey(tenantID, subject, predicate, object)` — **does not include `extractor_version`**. `memory.FactDocID(contentKey, validAt)` — same. `SemanticFact.ExtractorVersion` is stamped on every fact (provenance / ledger key) but plays **zero role** in fact identity or in `RuleReconciler.Reconcile`. The reconciler's five rules (`internal/ingest/reconciler.go`) are purely content-based:
1. Same `content_key` + same `valid_at` (any liveness) → **NOOP** (exact identity match — this is what a v3 pass produces for a triple v2 already extracted verbatim: the doc ids collide and dedupe to nothing new).
2. Same `content_key`, live → **NOOP** (already current).
3. Empty object → invalidate-intent.
4. Same `subject`+`predicate`, live head, different `object` → **UPDATE (supersede)**: the v3 fact becomes the new live head; the v2 fact's row is **not deleted**, it is closed (`invalid_at` stamped) via the guarded close path — it persists as closed history.
5. Otherwise → **ADD**: a genuinely new `subject`+`predicate` combination the v3 pass surfaced that v2 never asserted lands as a wholly independent, additive live fact — **coexists** alongside every unrelated v2 fact.

**So the answer is neither a pure "supersede" nor a pure "coexist" — it is content-key-scoped and decided per fact, version-blind:**
- Identical v3 output → **dedupes to nothing** (same doc, NOOP).
- Reworded/better-scoped v3 output for the same subject+predicate → **supersedes** v2 (v2 row closed, not deleted; v3 becomes live head).
- Net-new subject+predicate v3 discovers → **coexists** additively.
This will be verified against the live count delta and spot-checked in Phase 2 execution (DW-2.2/2.3), not just asserted from source reading.

### 3. Compose file location — CONFIRMED via `podman inspect`, not assumed
`podman inspect local-engramd-1` → `com.docker.compose.project.config_files = /Users/r/repos/engram/deploy/local/docker-compose.yml` (the **main repo path**, distinct from this worktree's copy at `.claude/worktrees/extraction-cli-shim/deploy/local/docker-compose.yml`). The two files are currently byte-identical (both at `-extractor-version v2`), but the main-repo copy is the one podman-compose actually reads and is presently **uncommitted** on `main`. Plan: edit both copies identically (main repo = operational effect; worktree copy = branch history stays truthful), commit only on the feature branch (not on `main`, which build-agent must not touch un-asked).

## Prerequisites
- [x] Shim binary buildable from this worktree (`cmd/engram-extract-shim` already has `-backend ensemble` from Phase 1, commit `ca8e7b8`).
- [x] `local-engramd-1` running and healthy; :8088 currently answered by PID 47514.
- [x] `scripts/backfill-reextract-rtd.sh` present, generic, reusable without modification.
- [x] Baseline `memory_status` captured live: `episodic_count=186`, `semantic_count=370`.

## Recommendation
BUILD (operational execution). No code changes to Go source are required for this phase (Phase 1 already delivered `-backend ensemble`); the work is: build+swap the shim binary, edit compose (both copies) to `v3`, restart `local-engramd-1` in place, rerun the backfill script, monitor the sweep, then verify via `memory_status`/`memory_search` and document the A/B.

---

## RESUMED SESSION — Execution Verification (independent re-check, not the sweep itself)

This build agent was dispatched after an earlier agent's sweep execution was interrupted mid-session but had already completed unattended (background processes independent of agent lifecycle). This section is my own independent verification of the completed sweep, gathered by direct tool calls (`memory_status`, `podman logs`, direct OpenSearch queries against `localhost:9201`, source reads) — not by trusting the orchestrator's summary.

### Settlement (2 checks, ~15+ min apart including intervening investigation)
- Check 1: `memory_status` → `episodic_count=186, semantic_count=730`.
- Check 2 (after full investigation below): `memory_status` → `episodic_count=186, semantic_count=730`. Identical. **Settled.**
- v2 baseline recorded earlier in this file: `semantic_count=370`. Current `engram-semantic-000001` breakdown (direct OpenSearch aggregation, tenant `rtd`, 730 docs pulled and grouped): **370 `v2` + 360 `v3` = 730.** The v2 count is untouched (370 == 370), confirming v2 rows are never deleted, only ever added alongside.

### Cross-version reconciliation semantics — CONFIRMED against live data (not just source reading)
Pulled all 730 `rtd` semantic docs (minus embeddings) and analyzed programmatically:
- **0 `content_key` collisions** (every doc has a unique `content_key`) — confirms NOOP-dedupe never leaves a duplicate row; exact-match content never re-lands as a second doc.
- **16 `subject`+`predicate` pairs have both a `v2` and a `v3` row present** — the supersede case predicted by reading `reconciler.go`: same subject+predicate, different (usually more precise/complete) object, both versions co-exist as rows (v2 closed-but-not-deleted, v3 live head — consistent with the source-level analysis above; I did not additionally verify the `invalid_at` flag on each closed v2 row via a live query, so I'm reporting the row-coexistence fact directly observed, and the "closed not deleted" mechanism as read from `reconciler.go`/`ids.go` in the original design pass, not independently re-verified bit-for-bit here).
- Net-new `v3`-only `subject`+`predicate` pairs (topics v2 never touched) coexist additively alongside unrelated v2 facts — observed directly in the per-event fact dumps below (e.g. `fable-bench-ideation-inline-2026-07-08` v3 facts like "confirmatory framing for LLM judges | reduces | defect detection" have no v2 counterpart at all).
- **Conclusion, empirically confirmed, not assumed:** reconciliation is content-key-scoped and version-blind, exactly as the source-reading pass predicted. Answer is neither pure-supersede nor pure-coexist — it's decided per fact.

### A/B spot-check (4 pairs — exceeds the ≥3 requirement; all 4 are quality WINS for the ensemble pass, plus one parity pair for calibration)

**1. Event `pensieve-pivot-2026-07-07`** (source: `claude-mux.mcp/DESIGN.md`, the pivot decision doc) — v2 extracted **7** facts, v3 extracted **8** facts from the *same* source text. v3 uniquely captures the decision's actual thesis and justification that v2 missed entirely:
   - v3-only: `Pensieve | thesis | compaction by retrieval, not by summary` — the core intellectual content of the decision.
   - v3-only: `Pensieve | implemented as | MCP rather than raw grep` — the justification for the architecture choice.
   - v3-only: `compacted session JSONL representation | unverified gotcha | how a compacted session is represented in the JSONL` — an open question the source text explicitly flags, which v2 silently dropped.
   - Both versions caught the pivot fact itself: v2 `claude-mux.mcp repo | pivoted to | Pensieve (working name)` vs v3 `claude-mux.mcp repo | pivoted to | Pensieve` — equivalent, v3 slightly cleaner.
   - **Verdict: v3 strictly better** — captures the decision's reasoning, not just its entities.

**2. Event `fable-bench-ideation-inline-2026-07-08`** — v2 extracted **5** facts, v3 extracted **9** facts from the same source. v3 uniquely captures argumentative/justification content:
   - v3-only: `fable-bench single-judge design | is considered | defensible`
   - v3-only: `fable-bench JUDGE-as-subagent design | is considered | well-founded`
   - v3-only: `confirmatory framing for LLM judges | reduces | defect detection`
   - v3-only: `Claude Code ecosystem ideation | is predominantly done | inline in the main session`
   - v2 caught only the structural/entity facts (modes, renames, instructions); v3 caught those *and* the reasoning behind the design decisions.
   - **Verdict: v3 strictly better.**

**3. Event `sdd-agy-vision-sandbox-2026-07-08`** — v2 extracted **8** facts, v3 extracted **9** facts. v3 captures the actual mechanism, not just the test observation:
   - v2 has: `agy --sandbox | does not block reading | files added via --add-dir` — a narrow observation.
   - v3 has the same fact *plus* the qualifier `even outside job cwd`, *plus* a new fact v2 never asserted: `agy --sandbox | blocks | network access and shell-spawn` (what `--sandbox` actually restricts — v2 only recorded what it *doesn't* block).
   - v3-only: `agy vision frame analysis | requires | strong vision model (e.g. Gemini 3.1 Pro (High))` — a generalized takeaway from the specific Gemini-3.1-vs-3.5 test results v2 recorded narrowly.
   - **Verdict: v3 strictly better** — generalizes the mechanism instead of just restating the specific test outcome.

**4. Fact-level pair, event `model-tier-benchmark-research-2026-07#5`** (subject `model tier cost/performance comparison methodology`, predicate `requires pinning`):
   - v2: *"pin a single reasoning-effort level uniformly across every model in the comparison to avoid confounding the cost comparison."*
   - v3: *"pin a single reasoning-effort level uniformly across every model in the comparison — a pricier tier run at max effort can cost more than a cheaper tier run at high effort, which would confound the cost comparison."*
   - **Verdict: v3 better** — same claim, plus the causal mechanism (*why* it confounds) v2 asserted without explaining.

**5. Parity calibration pair, event `review-agent-skill-as-acceptance-criteria-2026-06#1`** (subject `plain vanilla review`, predicate `caught path traversal vulnerability`):
   - v2: *"caught a path traversal vulnerability only ~50% of the time in a full harness, and 0% by pure reasoning alone."*
   - v3: *"caught the path traversal vulnerability only about 50% of the time in a full harness, and 0% of the time by pure reasoning alone."*
   - **Verdict: equally faithful** (wording-only difference) — included to show the ensemble is never *worse*, not just usually better.

**Faithfulness floor confirmed:** across all 5 pairs and the 16 multi-version groups scanned, v3 never lost or contradicted a fact v2 asserted; it only added precision, causal detail, or previously-missed content.

### CLAUDE.md / RTK / global-config leak scan — CLEAN
Scanned all 730 `rtd` semantic docs' `subject`/`predicate`/`object`/`statement` text for: `CLAUDE.md`, `RTK`, `rust token killer`, `omniping.dev`, `First Principles`, `Never assume`, `Never guess`, `Never lie`.
- **0 hits** for RTK / rust token killer / omniping.dev / First Principles / Never-assume-guess-lie phrasing — none of the live global `~/.claude/CLAUDE.md` or `RTK.md` instruction text leaked into extraction.
- **5 hits** for the literal string `CLAUDE.md` — all 5 verified legitimate: they are facts *about* a real, pre-existing rtd corpus gotcha (`engram-port:self/claude-skills/claude-md-filename-collision#1`, episodic event predates this session) — e.g. `skill reference/documentation file | must not be named | claude.md`, and a separate genuine `upublish` fact about `.upublishignore` not excluding `CLAUDE.md`. Not leakage — this is the user's own documented convention about *avoiding* filename collisions with `CLAUDE.md`, correctly extracted from their own corpus.
- **Result: PASS, no leak.**

### Dead-letter / convergence finding — REAL GAP, evidence below (contradicts the orchestrator's "no ERROR/panic lines observed" summary — independently found and must be reported honestly)
Direct `podman logs local-engramd-1` (full history, container started `2026-07-09T03:53:26Z`, 937 lines, no `--tail` truncation) and direct OpenSearch queries against `engram-episodic-000001` on `localhost:9201`:
- **3 `rtd` episodic events are permanently `dead_lettered: true`**: `pensieve-pivot-2026-07-07`, `fable-bench-ideation-inline-2026-07-08`, `sdd-agy-vision-sandbox-2026-07-08`, each `dead_letter_reason: "attempts exhausted (6 > 5)"`, `attempts: 6`.
- Root cause (from logs): repeated `context deadline exceeded (Client.Timeout exceeded while awaiting headers)` calling the shim's `/chat/completions`. `cmd/engram-server/main.go:88` builds `httpClient := &http.Client{Timeout: store.DefaultTimeout}` (`store.DefaultTimeout = 30s`, `internal/store/apply.go:224`) and reuses that **same 30-second-timeout client** for the extraction call (`main.go:140`, `ingest.NewHTTPExtractor(httpClient, ...)`). The ensemble backend (2 parallel candidate CLI calls + 1 judge call) is materially slower than the single-pass `agy` backend the 30s budget was presumably tuned for; under concurrent load these 3 events' calls didn't return in time on 6 consecutive attempts each and got permanently dead-lettered. Confirmed via `internal/store/outbox.go:117-123` and `internal/worker/worker.go:211-218`: dead-lettering is **not self-healing** — dead-lettered docs are permanently excluded from the outbox scan (`must_not dead_lettered` gate), so this will never auto-recover.
- **Data-loss check, the important nuance:** despite the dead-letter flag, all 3 events already have **full v3 semantic fact coverage in the live store right now** (8, 9, and 9 v3 facts respectively — see A/B pairs 1–3 above, all drawn from these exact 3 "dead-lettered" events). Cross-referencing `source_ids` on every `rtd` semantic doc against these 3 event IDs confirms this directly. So there is **no content loss** — extraction and reconciliation for these events did succeed and land in the store (most plausibly on an earlier `engramd` container instance before the current one restarted at `03:53:26Z` and re-claimed them under a reset `processed_at`, timing out this time). What's broken is purely the outbox bookkeeping: these 3 event *records* are stuck flagged dead-lettered even though their content made it through.
- **Outbox backlog state (direct OpenSearch counts, tenant `rtd`):** `183` processed-and-clean + `3` dead-lettered + `0` pending = `186` total. **The repair backlog has fully drained to zero** — nothing is still retrying or stuck mid-flight. The only non-terminal-clean state is the 3 permanent dead-letters.
- **No duplicate-fact explosion:** confirmed above (0 `content_key` collisions; the only cross-version overlap is the expected 16 supersede-style pairs, well within "documented cross-version behavior").
- **DW-2.4 verdict: PARTIALLY MET.** "Repair backlog drains" — TRUE. "No duplicate-fact explosion beyond documented behavior" — TRUE. "No dead-lettered events" — **FALSE, 3 exist**, confirmed live and permanent. This is a real, reportable gap, not resolved by this verification pass. I did **not** attempt to remediate it (e.g., by manually clearing `dead_lettered`/`processed_at` on these 3 docs or re-running any part of the sweep) — that would be new scope beyond "verify and report," and the dispatch instructions were explicit not to repeat the sweep. Flagging for `UPDATE_PLAN` / follow-up instead.

### Shim backend decision — EXECUTED
Per the plan's stated intent ("agy stays the fast live default; ensemble is on-demand only") and now that the deep sweep is settled with 0 pending backlog, switched the shim back to `-backend agy` for ongoing live steady-state:
- Verified the plain `engram-extract-shim` binary (scratchpad, built `2026-07-08 21:48:54` from the same commit `ca8e7b8` as the ensemble variant — `git status --short cmd/engram-extract-shim` is clean, so it's current) defaults to `-backend agy`.
- Smoke-tested it on a scratch port (`:8089`) first — `{"status":"ok"}` — before touching the live port.
- Safe swap: killed old ensemble PID `56437`, polled until `:8088` freed, immediately started the new `agy`-backend instance (gap: sub-second; and the outbox was already fully drained — 0 pending — at the moment of swap, so nothing was in flight to lose). New shim: **PID 55653, `-backend agy`, listening on `:8088`, `{"status":"ok"}`**, parent PID 1 (properly detached, matches the persistence pattern of the prior long-lived shim process).
- **Caveat I'm flagging, not resolving:** `local-engramd-1`'s compose config still has `-extractor-version v3` (unchanged, per instructions — I did not touch this). This means any *new* live events ingested going forward will be extracted by the (now fast/cheap) `agy` backend but ledger-keyed and fact-stamped as `extractor_version: v3` — a labeling mismatch, since `v3` was meant to signal ensemble-quality provenance. This wasn't in my dispatched scope (only the shim backend choice was asked of me) and reverting it would mean another `engramd` restart, which I was told not to do without cause. Surfacing this as a design question for the next plan iteration rather than acting on it unilaterally.

### Per-DW final status
| DW-ID | Requirement | Status | Evidence |
|-------|------------|--------|----------|
| DW-2.1 | Ensemble re-extraction fired, observed in logs | **MET** | `podman logs` full history shows real extraction activity (graph-dedup decisions, cost-meter climbing 182→186 events, `context deadline exceeded` retries — all evidence of live calls, not `ErrNoFacts` skips); 360 v3 semantic facts now in store, none of which existed before this sweep. |
| DW-2.2 | `memory_status` shows v3-populated semantic tier; count reported; coexistence noted | **MET** | `episodic_count=186, semantic_count=730` (370 v2 + 360 v3), settled across 2 checks. Coexistence confirmed empirically (0 content_key collisions, 16 supersede pairs, additive new facts) — not just source-read. |
| DW-2.3 | ≥3 A/B pairs show ensemble ≥ v2, ≥3 better, no CLAUDE.md leak | **MET** | 5 pairs documented verbatim above, all 4 comparative ones are strict wins, 1 parity case for calibration. Leak scan: 0 real hits, 5 legitimate `CLAUDE.md`-topic false-positives verified clean. |
| DW-2.4 | Reconciliation converges: no dead-letters, no dup explosion, backlog drains | **PARTIALLY MET** | Backlog drained to 0 pending — TRUE. No duplicate-fact explosion — TRUE. **3 permanent dead-lettered events exist** (confirmed live) — FALSE on the literal "no dead-lettered events" clause, though no content was actually lost (all 3 have full v3 fact coverage already). Root cause identified: fixed 30s HTTP timeout undersized for the ensemble backend's 3-call pipeline. |

### Status: UPDATE_PLAN
Phase 2's substantive deliverable — a populated, quality-verified v3 ledger with a documented A/B — **is fully realized and verified**: 360 new v3 facts, demonstrably higher-fidelity than v2 on every spot-checked pair, zero CLAUDE.md leakage, reconciliation semantics empirically confirmed, and the shim is left in the correct steady-state (`agy`, PID 55653). The one open item is DW-2.4's literal "no dead-lettered events" clause, which is false as observed (3 permanent dead-letters, root-caused to an undersized shared HTTP timeout under the ensemble backend's load) — though with zero actual data loss. Recommend a small follow-up (either bump the extraction client's timeout for future ensemble runs and/or a targeted, reviewed remediation to clear the 3 known-safe dead-letters) rather than silently marking DW-2.4 fully met or re-running the sweep to paper over it.
