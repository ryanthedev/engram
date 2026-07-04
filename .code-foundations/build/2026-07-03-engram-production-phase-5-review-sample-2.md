# Review: Phase 5 - Experience Memory Write-Gate (security-sensitive sample 2)

## Executed Results (Step 0)
- Build: `go build ./...` → Success (exit 0)
- Test suite: `make test` (`go test ./...`) → all packages `ok`, exit 0
- Experience unit: `go test ./internal/experience/ -v` → 40 tests PASS, exit 0
- Integration: `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration ./internal/experience/` → PASS incl. `TestIntegration_StoreRoundTrip` (0.11s) against live OpenSearch 3.1.0 (`engram-dev-os`), exit 0
- E2E: `ENGRAM_OPENSEARCH_URL=http://localhost:9201 ENGRAM_E2E_ADDR=localhost:7071 go test -tags=e2e ./e2e/` against the running compose stack → `TestDW_3_6_ScenarioPackRunsWithoutCoreEdits` PASS (12.28s), including `experience/distill-gate-retrieve` and `experience/poison-quarantined`, exit 0. (Ran against the already-up stack rather than `make e2e` self-boot to avoid a rebuild; ports 9201/7071 match the Makefile.)
- Lint: `make lint` (`go vet` + revive) → exit 0. Revive count on touched packages = 0 (the prior 24 violations are resolved). `gofmt -l` clean.

## Requirement Fulfillment

### DW-5.1 — injected-bad-record: ≥90% poisoned → Quarantine/Reject; ≥80% good → Admit
PREMISE:  ≥90% of poisoned fixtures caught (quarantine/reject), ≥80% of good fixtures admitted.
EVIDENCE: harness.go:19-106 (fixtures), harness.go:111-126 (GateRates), gate_test.go:14-27.
TRACE:    RuleGatekeeper over GoodFixtures (11; 10 admissible, 1 conservative-no-evidence) → admit=0.90; over PoisonedFixtures (14; 13 caught, 1 disclosed plausible-miss) → caught=0.93. Logged: `gate rates: good admit=0.90, poisoned caught=0.93`.
VERDICT:  PASS (0.90 ≥ 0.80; 0.93 ≥ 0.90, executed).

### DW-5.2 — no bypass: direct-index writes to T3 rejected at store layer; audit + test
PREMISE:  No code path writes the admitted T3 index without passing the Gatekeeper.
EVIDENCE: store.go:79-90 (nil-gate → ErrNoGate at construction), store.go:101-137 (Admit runs gate first), store.go:144-163 (admitMerged is the only PutAdmitted caller). Full-tree audit below.
TRACE:    Writer audit — `PutAdmitted`/`SoftExpire` are called ONLY from `Store.admitMerged` (from `Admit` and human `Release`) and `Store.Prune`. `Admit` always calls `s.gate.Evaluate` before any admitted write. `DistillStage.Process` (distill.go:126) writes only via `store.Admit`. No other package references the admitted index or `PutAdmitted`. Tests: NilGateRejected, DenyAllGateNeverAdmits, GateRunsBeforeEveryWrite (exactly 5 gate calls for 5 admits).
VERDICT:  PASS (structural + executed).

### DW-5.3 — gate-LLM timeout → Quarantine; nothing Admits on error
PREMISE:  Judge timeout/error/empty/unknown-verdict all fail-closed to Quarantine, never Admit.
EVIDENCE: store.go:107-118 (gerr → Quarantine; !Valid() → Quarantine), gate_http.go:72-128 (every failure path returns Quarantine). Tests gate_test.go:71-126, store_test.go:82-98.
TRACE:    Dirty test `TestDW_5_3_JudgeTimeoutQuarantines`: slow server (500ms) + 50ms ctx → transport deadline → `Quarantine`, error surfaced (0.50s, real). `JudgeErrorNeverAdmits`: 5xx / empty-choices / garbage / unknown-verdict → all Quarantine. `StoreFailClosedOnGateError`: a gate returning `Admit`+error is folded to Quarantine, admitted index stays empty.
VERDICT:  PASS (executed).

### DW-5.4 — curated-small-beats-large reproduced (A/B logged)
PREMISE:  A small curated set yields higher retrieval uplift than a large noisy set; A/B logged.
EVIDENCE: harness.go:205-279 (ABEval/ABReport/Emit), harness_test.go:11-28.
TRACE:    `ABEval(RuleGatekeeper, CuratedSmall[5], LargeNoisy[34])` → curated uplift 0.800 (n=5) vs noisy uplift 0.314 (n=21 admitted after gate drops poison), `curated_beats_noisy=true`. Emitted via `ABReport.Emit` (`experience A/B eval …` log line captured).
VERDICT:  PASS (executed).

### DW-5.5 — following-correlation emitted; prune soft-expires low-Φ, bi-temporally recoverable
PREMISE:  Following-correlation metric emitted; prune soft-expires low-Φ records, recoverable, never deletes.
EVIDENCE: harness.go:128-203 (FollowingCorrelation/Metrics.Emit), prune.go:1-53, store.go:234-255 (Prune→SoftExpire), store.go:221-232 (Recover include-expired), opensearch.go:202-207 (partial-update, no delete). Tests harness_test.go:33-61, prune_test.go:12-85, integration round-trip.
TRACE:    `TestDW_5_5_FollowingCorrelationEmitted`: r>0 for followed high-Φ; degenerate NaN-safe → 0; `Metrics.Emit` logs `following_correlation`. `PruneSoftExpiresRecoverable`: low-Φ (0.1) pruned → not retrievable but `Recover` returns it with `ExpiredAt` set, `Live()==false`; high-Φ untouched. `PruneRespectsRetrievalCount`: retrieval_count=1 survives retrievalMax=0. Delete audit: the ONLY `http.MethodDelete` is `DeleteQuarantine` (opensearch.go:293) on the quarantine index at release; no delete/delete_by_query ever touches the admitted index.
VERDICT:  PASS (executed, incl. live-cluster round-trip).

### DW-5.6 — e2e: task completes, distills, gates, retrieved in a later session via MCP
PREMISE:  Full loop — task completion → distill → gate → retrieved in a later MCP session.
EVIDENCE: e2e/scenarios_experience.go:32-68 (`expDistillGateRetrieve`), stages_experience.go:34-68 (wiring), tier.go:39-69.
TRACE:    E2E `experience/distill-gate-retrieve` PASS (0.57s, live stack): session 1 `memory_ingest` a fact+experience event → distillation stage gates+admits → session 2 `memory_search` retrieves the distilled skill as an `experience`-source hit. `experience/poison-quarantined` PASS (4.08s): no-evidence success quarantined, appears in `engram quarantine list`, and never returned as an experience hit across 6 polls.
VERDICT:  PASS (executed end-to-end).

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-5.1 → `TestDW_5_1_InjectedBadRecordRates` + `TestRuleGate_VerdictClasses`
- [x] DW-5.2 → `TestDW_5_2_NilGateRejected`, `_DenyAllGateNeverAdmits`, `_GateRunsBeforeEveryWrite` + writer audit
- [x] DW-5.3 → `TestDW_5_3_JudgeTimeoutQuarantines` (dirty), `_JudgeErrorNeverAdmits`, `_StoreFailClosedOnGateError`
- [x] DW-5.4 → `TestDW_5_4_CuratedSmallBeatsLargeNoisy`
- [x] DW-5.5 → `TestDW_5_5_FollowingCorrelationEmitted`, `_PruneSoftExpiresRecoverable`, `_PruneRespectsRetrievalCount`, `TestFollowingCorrelation_Degenerate`
- [x] DW-5.6 → e2e `experience/distill-gate-retrieve` + `experience/poison-quarantined` (live stack)
- [x] Edge: no-evidence→Quarantine, contradictory→Quarantine (gate_test), dedup-merge (`TestDedup_SameFingerprintMerges`), release=human-CLI-only (`TestQuarantineReleaseRoundTrip` + cli.go audit)
- [x] ≥1 dirty test per code-touching area (timeout, gate-error, injection reject, nil-gate, cross-tenant guard)
- [x] Live-cluster integration (`TestIntegration_StoreRoundTrip`) proves OpenSearch backend honors the same contract

Coverage matches the 100%-of-DW + ≥1-dirty-per-area level.

## Dead Code
None found. `go vet` + revive exit 0; no unused imports, unreachable code, debug prints, or commented-out blocks in the touched files.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | PruneJob runs in a goroutine; MemBackend guarded by a mutex; OpenSearchBackend is stateless HTTP; Store holds no shared mutable state. Admitted docs keyed by fingerprint `_id`, so concurrent same-fingerprint admits are last-write-wins on ONE doc — the dedup requirement ("merge not double-count") holds even under a race. (Non-atomic MergedCount accounting noted below — not a defect against any DW item.) |
| Error Handling | PASS | Every gate failure mode is fail-closed to Quarantine and logged (store.go:108-118, gate_http.go). OpenSearch backend checks status codes and decode errors. Store.Admit rejects empty-tenant. No swallowed errors. |
| Resources | PASS | All HTTP response bodies `defer resp.Body.Close()` (gate_http.go:94, opensearch.go:333); judge non-2xx body read via `io.LimitReader(…, 2048)`. PruneJob ticker stopped on ctx done. |
| Boundaries | PASS | Utility range enforced [0,1] (reject out-of-range); FollowingCorrelation NaN-safe on <2 points / zero variance; k<=0 defaults to 10; cross-tenant `_id` collision fails closed (opensearch.go:132, 284). |
| Security | PASS | Trust boundary honored: provenance/scope (tenant/team/scope/owner/trajectory/source) are stamped from the trusted event, NEVER the agent-supplied directive (distill.go:117-124; parser sets only task/skill/outcome/utility/context). Injection markers Reject at the barricade. Quarantine index is unreachable via any retrieval path (Tier→SearchAdmitted queries only the admitted index with `must_not exists expired_at`; quarantine reads exist only in ListQuarantine/GetQuarantine, used by the human CLI + Release). Tier hits re-verified through the ACL predicate (defense in depth). Release is invoked ONLY from cli.go runQuarantineRelease. |

### Adversarial break attempts (all failed to break the gate)
- (a) Ungated write to T3: impossible — `PutAdmitted`/`SoftExpire` reachable only through `Store` (gate-first Admit, human Release, soft-expire Prune); nil-gate construction fails with `ErrNoGate`.
- (b) Fail-open on timeout/error/empty/unknown: all resolve to Quarantine (verified live + unit).
- (c) Quarantine leak through retrieval: no retrieval path reads the quarantine index; e2e confirms poison never surfaces as a hit.
- (d) Provenance/scope spoof from directive: overwritten from the event; directive cannot set tenancy/scope/owner.
- (e) Hard-delete on prune: only soft-expire (partial-update expired_at); the single DELETE is on the quarantine index at release.
- (f) Auto quarantine release: none — Release has a single caller, the `engram quarantine release` CLI command.

### Disclosed deviations (judged)
1. **Distillation fires only on events that also yield an extracted fact.** Confirmed in worker.go:276-282: an `ErrNoFacts` event completes early and never reaches `runStages`, so a fact-free experience directive would not distill. This is a real functional constraint but violates **no listed DW item** — DW-5.6 requires the distill→gate→retrieve loop to be proven e2e, and the e2e scenario (fact + experience directive in one event) exercises the full loop and PASSES on the live stack. Non-blocking note.
2. **Gate is an unconditional `ExperienceStore` guard rather than the literal Phase-4 WriteGuard.** Judged genuinely un-bypassable: `Store` is the sole writer of the admitted index, its only admit path runs the gate first, and construction fails without a gate. The guard is stronger than a filter seam because there is no `Store` method that writes the admitted index without the gate. Acceptable.

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep module / information hiding | PASS | `Store` exposes intent methods (Admit/Search/ListQuarantine/Release/Prune/Recover) and hides two index names, dedup-merge, quarantine routing, and soft-expire bi-temporal recovery. `Gatekeeper`/`Distiller`/`Backend` are narrow ports. |
| aposd-designing-deep-modules | No shallow / pass-through / temporal decomposition | PASS | `Backend` is a persistence port with no gating logic; `Store` adds real behavior (routing, merge, fail-closed), not a wrapper. No classitis. |
| aposd-designing-deep-modules | Silent failure surfaced | PASS | Gate errors are both logged AND observable via the returned Quarantine verdict; `recordRetrieval` failure logged, never silently dropped. |
| cc-defensive-programming | External input validated at barricade | PASS | Agent-supplied directive is untrusted: RuleGatekeeper rejects injection/fabricated-utility/empty-task; provenance sourced from the trusted event. |
| cc-defensive-programming | Fail-closed at trust boundary / no fail-open | PASS | store.go:108-118 + gate_http.go: any inability to reach a confident Admit → Quarantine. |
| cc-defensive-programming | No empty catch / no swallowed errors | PASS | Every error path returns or logs; `osJSON` best-effort JSON unmarshal is intentional and status is still checked by callers. |
| cc-defensive-programming | Defense in depth on security-critical path | PASS | Admitted tier hits are re-verified through the ACL predicate by the MultiRetriever; tenant re-checked on GetAdmitted/GetQuarantine. |

## Notes (non-blocking)
1. **Non-atomic merge accounting (OpenSearch).** `admitMerged` does read-then-write (GetAdmitted → PutAdmitted); two concurrent admits of the same fingerprint could each read "no existing" and land `MergedCount=0` instead of 1. This does not create a duplicate doc (dedup requirement holds — fingerprint is the `_id`) and does not affect admit/quarantine routing; only the merge counter can under-count. Consider a scripted upsert if merge counts become load-bearing.
2. **Rule-gate false negative is deliberate.** The `plausible-but-fabricated` poisoned fixture is admitted by the deterministic rule judge (disclosed); the production HTTPGatekeeper LLM judge is the real defense. The ≥90% bar (0.93) survives the miss.
3. **`ScanPrunable`/`ListQuarantine` use `size: 1000`** with no pagination; fine at current scale, worth revisiting if a tenant's low-Φ backlog exceeds 1000.

## Issues (if FAIL)
None.

**Verdict: PASS**
