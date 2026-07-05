# Review: Phase 5 - Experience Memory (T3) Write-Gate (security-sensitive sample 1)

## Executed Results (Step 0)
- Build: `go build ./...` → success.
- Test suite: `make test` (`go test ./...`) → all packages OK; `internal/experience` 0.519s pass.
- Experience unit tests (verbose): `TestDW_5_1..5_5`, dedup, quarantine, tier, distill, prune → 19+ pass. DW-5.1 rates logged: **good admit=0.90, poisoned caught=0.93**. DW-5.4 A/B logged: **curated uplift=0.800 (n=5) vs noisy=0.314 (n=21), curated_beats_noisy=true**.
- Race: `go test -race ./internal/experience/` → OK (1.538s).
- Integration: `ENGRAM_OPENSEARCH_URL=:9200 go test -tags=integration ./internal/experience/` (live `engram-dev-os`, OpenSearch 3.1 @ :9200) → all pass incl. `TestIntegration_StoreRoundTrip` (0.10s).
- E2E: `make e2e` (full podman compose self-boot, ports :9201/:7071) → `ok ./e2e 12.7s`. Verbose re-run confirms **`experience/distill-gate-retrieve` PASS (1.11s)** and **`experience/poison-quarantined` PASS (4.65s)**.
- Typecheck: `go vet ./...` (incl. `-tags=integration` and `-tags=e2e`) → no issues.
- **Lint: `make lint` → FAIL (exit 1).** revive reports **24 violations, all inside the Phase-5 files under review** (`experience.go`, `tier.go`, `opensearch.go`, `store.go`): missing exported-method/const doc comments and two repetitive type names (`ExperienceStore`, `ExperienceTier`). See Issues.

## Requirement Fulfillment

### DW-5.1
PREMISE:  "injected-bad-record suite — ≥90% poisoned → Quarantine/Reject; ≥80% good → Admit."
EVIDENCE: `harness.go:19-106` (GoodFixtures/PoisonedFixtures/GateRates), `gate_test.go:14-27`.
TRACE:    10 good fixtures (one deliberately evidence-less) → RuleGatekeeper → 9 Admit = 0.90 ≥ 0.80; 14 poisoned (incl. one plausible false-negative) → 13 caught = 0.93 ≥ 0.90.
VERDICT:  PASS (executed rates 0.90 / 0.93).

### DW-5.2
PREMISE:  "no bypass — direct-index writes to T3 rejected at the store layer; code-path audit + test."
EVIDENCE: `store.go:79-90` (nil gate → ErrNoGate), `store.go:101-137` (Admit is sole gated write), code-path audit below; `store_test.go:33-78`.
TRACE:    Audit of every admitted-index writer — `PutAdmitted` is called only at `store.go:159` inside `admitMerged`; `admitMerged` is called only from `Admit` (post-gate, line 122) and `Release` (line 211). `Release` is reachable only from `internal/cli/cli.go:494` (the `engram quarantine release` admin CLI — not exposed via MCP/gRPC). `store.Admit` has one non-test caller: `distill.go:126` (gated stage). Every `OpenSearchBackend`/`NewExperienceStore` construction (stages_experience.go, cli.go) wraps the backend in the gated store. No ungated write path exists in-repo. Tests: nil-gate rejected, deny-all gate leaves admitted index empty over 20 writes, gate consulted exactly once per admit.
VERDICT:  PASS.

### DW-5.3
PREMISE:  "gate-LLM timeout → Quarantine (dirty test); nothing Admits on error."
EVIDENCE: `gate_http.go:72-107` (every failure mode → Quarantine), `store.go:107-118` (gerr and invalid verdict → Quarantine), `gate_test.go:71-126`, `store_test.go:82-104`.
TRACE:    Slow judge (500ms) vs 50ms ctx deadline → transport error → `Quarantine`, err surfaced (dirty test). 5xx / empty-choices / garbage / unknown-verdict → all `Quarantine`. Store folds a gate that returns `(Admit, err)` to `Quarantine` and admitted index stays empty.
VERDICT:  PASS.

### DW-5.4
PREMISE:  "curated-small-beats-large reproduced on the eval set (A/B logged)."
EVIDENCE: `harness.go:205-279` (ABEval/ABReport.Emit/CuratedSmall/LargeNoisy), `harness_test.go:11-28`.
TRACE:    Curated arm (5 clean high-Φ) → mean admitted utility 0.800; noisy arm (20 mediocre + 14 poisoned) → gate drops poison, 21 admitted, mean 0.314; CuratedBeatsNoisy=true. A/B emitted to logger.
VERDICT:  PASS (executed: 0.800 > 0.314).

### DW-5.5
PREMISE:  "experience-following correlation metric emitted; prune soft-expires low-Φ records, bi-temporally recoverable."
EVIDENCE: `harness.go:128-203` (FollowingCorrelation/Metrics.Emit), `store.go:234-255` (Prune→SoftExpire), `store.go:359-373`/`opensearch.go:198-202` (soft-expire, never delete), `store.go:223-232` (Recover includeExpired), `prune_test.go`, `harness_test.go:33-61`.
TRACE:    Pearson corr of followed high-Φ→success emitted (r>0, NaN-safe). Prune(0.2,0): low-Φ record soft-expired (expired_at stamped) → excluded from SearchAdmitted/tier but `Recover` returns it with ExpiredAt set and Live()=false; high-Φ untouched; retrieval-count floor spares popular low-Φ. Live-cluster integration confirms soft-expire+recover via `_update` (no delete).
VERDICT:  PASS.

### DW-5.6
PREMISE:  "e2e — agent completes a task, experience distills, gates, and is retrieved in a later session via MCP."
EVIDENCE: `e2e/scenarios_experience.go:34-68`, executed `experience/distill-gate-retrieve` PASS (1.11s), `experience/poison-quarantined` PASS (4.65s).
TRACE:    Session 1 MCP `memory_ingest` of a task-completion event (fact + `experience:` directive) → worker distill stage → gate Admit → session 2 MCP `memory_search` returns the distilled skill via the experience tier. Poison variant: quarantined, appears in `engram quarantine list`, never retrievable across 6 polls.
VERDICT:  PASS. (Deviation note below on the fact-coupling; the e2e event carries a fact so the full loop is genuinely exercised.)

**All requirements met:** YES (functionally/securely). See lint blocker under Issues.

## Test-DW Coverage
- [x] DW-5.1 → `TestDW_5_1_InjectedBadRecordRates` (ran).
- [x] DW-5.2 → `TestDW_5_2_NilGateRejected`, `..._DenyAllGateNeverAdmits`, `..._GateRunsBeforeEveryWrite` (ran) + code-path audit.
- [x] DW-5.3 → `TestDW_5_3_JudgeTimeoutQuarantines`, `..._JudgeErrorNeverAdmits`, `..._StoreFailClosedOnGateError` (ran; dirty tests present).
- [x] DW-5.4 → `TestDW_5_4_CuratedSmallBeatsLargeNoisy` (ran).
- [x] DW-5.5 → `TestDW_5_5_FollowingCorrelationEmitted`, `..._PruneSoftExpiresRecoverable`, `..._PruneRespectsRetrievalCount` (ran).
- [x] DW-5.6 → e2e `experience/distill-gate-retrieve` + `experience/poison-quarantined` (ran green) + `TestIntegration_StoreRoundTrip` (live cluster).
- [x] ≥1 dirty test per code-touching area: gate (timeout/5xx/garbage), store (gate-error fail-closed, deny-all, reject-dropped), distill (poisoned directive quarantined), tier (excludes quarantined+pruned, no-tenant), prune (retrieval-count floor).

## Dead Code
None found. No debug prints, TODO/FIXME, panics, or unreachable code in the reviewed non-test files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `MemBackend` guards all maps with a mutex; `ExperienceStore` holds no per-call mutable state; PruneJob uses a ticker + ctx. `go test -race` clean. Worker runs stages after fact reconcile with at-least-once idempotency (fingerprint upsert-merge). |
| Error Handling | PASS | Fail-closed everywhere: gate transport/decode/deadline/empty/unknown → Quarantine; store gerr and invalid-verdict → Quarantine; backend errors propagated and re-queue the event; retrieval-count bump best-effort (logged, non-fatal). No empty catch of substance. |
| Resources | PASS | HTTP response bodies deferred-closed; non-2xx body read is `LimitReader`-bounded (2048). No leaked handles/goroutines (prune goroutine bound to ctx). |
| Boundaries | PASS | Utility range `[0,1]` enforced (out-of-range → Reject); empty task → Reject; empty tenant → Reject; k<=0 defaulted; cross-tenant id-collision guard on GetAdmitted/GetQuarantine; NaN-safe Pearson on degenerate samples. |
| Security | PASS | This is the poisoning barricade. Adversarial checks all hold — (a) no ungated admitted-index writer (audit above); (b) timeout/error/empty/unknown → Quarantine, proven by tests; (c) tier/SearchAdmitted query only the admitted index — the separate quarantine index is unreachable through any retrieval path (only the human CLI `quarantine list` reads it); (d) provenance/scope/tenant/fingerprint are overwritten from the trusted event in `distill.go:118-124`, never from the agent-supplied directive (parser sets only task/skill/success/phi/evidence/signals/context); (e) prune only SoftExpires (`_update` expired_at) — no hard delete of admitted docs anywhere; Recover proves bi-temporal recoverability. Injection markers → Reject (dropped). Release is human-CLI-only, not agent-reachable. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep module / information hiding | PASS | `ExperienceStore` exposes intent methods (Admit/Release/ListQuarantine/SearchAdmitted/Prune/Recover) hiding two index names, dedup-merge, quarantine routing, soft-expire bi-temporal recovery. `Backend`/`Gatekeeper`/`Distiller` are narrow deep ports. |
| aposd-designing-deep-modules | No silent-failure red flag | PASS | Failures are surfaced — errors returned to re-queue events, fail-closed transitions logged (`WarnContext`/`InfoContext`); nothing is swallowed. |
| aposd-designing-deep-modules | Sole-writer guarantee is convention, not type-enforced | Note (non-blocking) | `Backend` and its methods are exported, so another package could in principle call `OpenSearchBackend.PutAdmitted` directly; no such caller exists in-repo. Design observation, not a demonstrated bypass. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | The gate is the barricade for untrusted experience; provenance re-sourced from the trusted event; tenant/utility/injection validated; agent-supplied directive never sets provenance. |
| cc-defensive-programming | No empty catch / correctness-over-robustness at trust boundary | PASS | Fail-closed (correctness lean): the store never returns Admit on any judge failure or unknown verdict. Best-effort paths (retrieval bump, response-body decode) are deliberate and logged, not silent bug-swallowing. |
| cc-defensive-programming | Assertions disabled-in-prod / executable-in-assert | N/A | Go; no assertion mechanism used. |

## Notes (non-blocking)
1. **Deviation 1 (disclosed) — distillation coupled to fact extraction.** `worker.go:276-282`: an event returning `ErrNoFacts` completes early and never reaches `runStages`, so `DistillStage` only fires when the event also yields ≥1 fact. The DW-5.6 e2e works around this by carrying a `fact:` line alongside the `experience:` directive, so the full distill→gate→retrieve loop IS genuinely exercised end-to-end (verified: scenario passed). No DW item requires distilling fact-less events. Worth tracking as a real product limitation (a pure-experience event silently distills nothing).
2. **Deviation 2 (disclosed) — gate as unconditional ExperienceStore guard vs literal Phase-4 WriteGuard.** The construction-time nil-gate rejection plus sole-writer routing makes the gate un-bypassable for every in-repo path (audit under DW-5.2). This is a sound realization of the no-bypass intent; the exported-Backend caveat above is the only theoretical seam.
3. HTTPGatekeeper decodes the 2xx judge body unbounded (non-2xx path is LimitReader-bounded). The judge is a trusted team-owned endpoint, so low risk; a size cap would harden it.
4. Release re-admits a quarantined record without re-gating (by design — human authority). Only Quarantine-verdict records reach the quarantine index (Reject is dropped), so a human never releases an injection payload.

## Issues (blocking)
1. `make lint` fails (exit 1) with 24 revive violations, all in the Phase-5 files under review.
   - File: `internal/experience/experience.go:37,40,45`; `internal/experience/tier.go:18`; `internal/experience/opensearch.go:106,115,139,161,198,204,223,236,260,281`; `internal/experience/store.go:69,292,299,312,347,359,375,382,397,404`.
   - Demonstrated by: `make lint` → `go run .../revive ... -set_exit_status` exits 1 (captured output). Violations are missing exported-method/const doc comments and two repetitive type names (`ExperienceStore`, `ExperienceTier` flagged as `will be used as experience.Experience...`).
   - Why blocking: `make lint` is a required phase gate (Makefile uses `-set_exit_status`; the repo's revive `exported` rule is the DW-0.2 doc-comment contract) and the project Error Policy mandates fixing all lint errors. Every functional/security requirement passes with strong execution evidence, but the phase does not pass its own required lint command, so the code is not commit-ready as-is.
   - Fix: add the standard `// Name ...` doc comment to each flagged exported method/const, and either rename the types (`ExperienceStore`→`Store`, `ExperienceTier`→`Tier`) or add a targeted revive exclusion with justification. Purely mechanical; no logic change.

**Verdict: FAIL — sole blocker is the failing `make lint` gate (24 revive violations in the Phase-5 files). All six DW-5.x requirements, every listed edge case, and both loaded-skill criteria sets pass with execution evidence; the security posture (no bypass, fail-closed, no quarantine leak, no provenance spoof, soft-expire-only) is verified. Re-run lint after adding the missing doc comments / type renames and this phase passes.**
