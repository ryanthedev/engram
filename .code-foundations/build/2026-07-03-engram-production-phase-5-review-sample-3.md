# Review: Phase 5 - Experience Memory (T3) + Mandatory Write-Gate (security sample 3)

## Executed Results (Step 0)
- Build: `go build ./...` → Success (exit 0)
- Unit suite: `go test ./internal/experience/...` → PASS (exit 0)
- Full suite: `make test` → all packages `ok`
- Lint: `make lint` (`go vet` + revive) → exit 0
- Integration (live OpenSearch 3.1.0 @ :9200): `ENGRAM_OPENSEARCH_URL=http://localhost:9200 go test -tags=integration -count=1 -v ./internal/experience/` → PASS (exit 0), incl. `TestIntegration_StoreRoundTrip` (0.12s) against the real cluster mappings.
- E2E: compose stack already running (podman `engram-e2e-os` @ :9201, `local-engramd-1` @ :7071). Ran the suite in external mode directly against it rather than `make e2e` (which would rebuild + teardown the live stack): `ENGRAM_OPENSEARCH_URL=http://localhost:9201 ENGRAM_E2E_ADDR=localhost:7071 go test -tags=e2e -count=1 -timeout=300s -v ./e2e/` → PASS (exit 0). Includes `experience/distill-gate-retrieve` (0.57s) and `experience/poison-quarantined` (3.05s). **Note:** used external-mode against the live stack, not `make e2e`; equivalent code path (same TestMain/Boot external branch), noted per dispatch allowance.

## Requirement Fulfillment

### DW-5.1
PREMISE:  "injected-bad-record — ≥90% poisoned → Quarantine/Reject; ≥80% good → Admit."
EVIDENCE: harness.go:19-106 (fixtures), gate_test.go:14-27 (`TestDW_5_1_InjectedBadRecordRates`); e2e `expPoisonQuarantined` scenarios_experience.go:70.
TRACE:    GoodFixtures (10; 9 evidence-backed success → Admit, 1 no-evidence → Quarantine) ⇒ admit 0.90 ≥ 0.80. PoisonedFixtures (14; 13 caught, 1 deliberate plausible miss admitted) ⇒ caught 0.929 ≥ 0.90. Assertions run and pass; e2e confirms a no-evidence success is quarantined and never retrievable.
VERDICT:  PASS

### DW-5.2
PREMISE:  "no bypass — direct-index writes to T3 rejected at store layer; code-path audit + test."
EVIDENCE: store.go:79-90 (`NewStore` ErrNoGate), store.go:101-137 (`Admit` gate-first), store.go:144-163 (`admitMerged`); tests store_test.go:33/42/64.
TRACE:    Caller audit: `Admit` is called only from DistillStage.Process; `admitMerged`/`PutAdmitted` are reached only through `Admit` and `Release`; `Release` is called only from the CLI (`runQuarantineRelease`). `NewStore(nil gate)`→ErrNoGate; a deny-all gate leaves the admitted index empty over 20 writes; the gate is consulted exactly once per admit. Backend is a lower port constructed in `wireExperience` and handed only to the Store — no production path writes the admitted index without the gate.
VERDICT:  PASS  (deviation "unconditional ExperienceStore guard vs literal WriteGuard" judged below — accepted)

### DW-5.3
PREMISE:  "gate-LLM timeout → Quarantine (dirty test); nothing Admits on error."
EVIDENCE: gate_http.go:72-107 (every failure arm → Quarantine), store.go:107-118 (fail-closed fold incl. unknown verdict); tests gate_test.go:71 (`TestDW_5_3_JudgeTimeoutQuarantines`, 0.50s real slow-server), gate_test.go:97 (5xx/empty/garbage/unknown), store_test.go:82 (gate returning `Admit,error` → Quarantine).
TRACE:    50ms ctx deadline vs 500ms server ⇒ transport error ⇒ Quarantine + surfaced error, never Admit. A gate that returns `(Admit, "would-admit", err)` is folded to Quarantine by the store; admitted index stays empty.
VERDICT:  PASS

### DW-5.4
PREMISE:  "curated-small-beats-large reproduced (A/B logged)."
EVIDENCE: harness.go:205-279 (`ABEval`/`ABReport.Emit`/`CuratedSmall`/`LargeNoisy`); test harness_test.go:11 (`TestDW_5_4_CuratedSmallBeatsLargeNoisy`).
TRACE:    Curated (5 clean high-Φ, all admit) vs noisy (20 mediocre-Φ + poison). Gate strips poison; noisy admitted mean is diluted by mediocre bulk ⇒ `CuratedBeatsNoisy=true`, `CuratedUplift>NoisyUplift`, `NoisyAdmitted<len(noisy)`; `report.Emit` logs the A/B. Assertions pass.
VERDICT:  PASS

### DW-5.5
PREMISE:  "following-correlation metric emitted; prune soft-expires low-Φ, bi-temporally recoverable."
EVIDENCE: harness.go:128-203 (`FollowingCorrelation`/`Metrics.Emit`); prune.go + store.go:234-255 (`Prune`→`SoftExpire`), store.go:223-232 (`Recover` includeExpired); tests harness_test.go:33/51, prune_test.go:12/65, integration `TestIntegration_StoreRoundTrip`.
TRACE:    Prune(0.2,0) soft-expires the Φ=0.1/retrieval=0 record (stamps `expired_at`, never deletes); it drops from SearchAdmitted but `Recover` still returns it with `ExpiredAt!=nil` and `Live()==false`; high-Φ untouched; a retrieved (count=1) low-Φ record survives retrievalMax=0. FollowingCorrelation>0 for high-Φ-followed-success, NaN-safe (returns 0) on degenerate input; `Metrics.Emit` logs following_correlation. All pass on both mem and live OpenSearch backends.
VERDICT:  PASS

### DW-5.6
PREMISE:  "e2e — task completes, experience distills, gates, retrieved in a later session via MCP."
EVIDENCE: scenarios_experience.go:32-68 (`expDistillGateRetrieve`); executed e2e run — `experience/distill-gate-retrieve` PASS (0.57s).
TRACE:    Session 1 `memory_ingest` of a fact+`experience:` completion event → worker distill stage gates (Admit) → session 2 `memory_search` returns the distilled skill through the gated experience tier (source=="experience", fields contain the marker). Poison companion scenario confirms a no-evidence experience lands in `quarantine list` and never surfaces as an experience hit.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have automated tests that ran in Step 0 (DW-5.1/5.2/5.3/5.4/5.5 unit+integration; DW-5.6 e2e scenario).
- [x] ≥1 dirty test per code-touching area: gate error/timeout (gate_test), fail-closed fold (store_test), poisoned directive quarantined (distill_test), no-tenant tier (tier_test), prune recoverability (prune_test), degenerate correlation (harness_test).
- [x] Coverage matches "100% of DW items"; live-cluster round-trip proves the OpenSearch backend honors the same contract as the in-memory one.

## Dead Code
None found. `go vet` + revive clean; no unreachable code, debug prints, or commented-out blocks. `recordRetrieval` degrades gracefully via the optional `retrievalBumper` capability (used by both backends).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | MemBackend mutex-guarded; PruneJob ticker stopped; gate has no shared mutable state. Note: admitMerged read-modify-write is not atomic, but the doc is fingerprint-keyed so a race still yields ONE doc (no double-count) — only MergedCount may under-count. Not a security property; non-blocking note. |
| Error Handling | PASS | Every gate failure mode → Quarantine (gate_http.go:82-107); store folds gate error + unknown verdict to Quarantine (store.go:108-118); HTTP bodies closed; osDo surfaces non-2xx. |
| Resources | PASS | `defer resp.Body.Close()` on every request; `defer ticker.Stop()`; io.LimitReader on error-body reads. |
| Boundaries | PASS | pearson n<2 / zero-variance → 0 (NaN-safe, tested); utility [0,1] enforced (→Reject); no-tenant Admit→Reject, no-tenant tier→empty. |
| Security | PASS | Injection markers Reject at barricade (gate_rule.go:27-31, 87-97); provenance/scope stamped from trusted event not the untrusted directive (distill.go:118-124, tested distill_test.go:33); cross-tenant `_id` collision guard in GetAdmitted/GetQuarantine (opensearch.go:132-134, 284-286); quarantine index physically separate and never reached by the tier; release gated to explicit CLI. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| aposd-designing-deep-modules | Deep module / information hiding | PASS | Store exposes intent methods (Admit/ListQuarantine/Release/SearchAdmitted/Prune/Recover) hiding two index names, verdict routing, dedup-merge, soft-expire recovery; Backend is a narrow port (dependency inversion). No shallow/pass-through module. |
| aposd-designing-deep-modules | Silent-failure red flag | PASS | Failures are surfaced: gate errors logged + folded to Quarantine, prune/retrieval failures logged, verdict returned to caller. |
| cc-defensive-programming | External input validated at entry (barricade) | PASS | The gate IS the barricade for agent-supplied experience; tenant/utility/injection/evidence all validated before any write. Security-critical path re-validated (cross-tenant guard) inside the store layer. |
| cc-defensive-programming | No empty catch / no silent swallow | PASS | No empty catches; best-effort accounting logs on failure rather than swallowing (tier.go:81-89). |
| cc-defensive-programming | Fail-closed correctness over robustness | PASS | Poisoning threat model → correctness bias: timeout/error/empty/unknown all resolve to Quarantine, never Admit (store.go + gate_http.go), matched by dirty tests. |

## Adversarial bypass checklist (dispatch focus)
| Probe | Result |
|-------|--------|
| (a) writer of admitted/quarantine skipping the gate | None. `Admit` (gated) and `Release` (explicit human CLI) are the only writers; grep-audited callers of Admit/admitMerged/PutAdmitted/Release. |
| (b) timeout/error/empty/unknown → Quarantine, never Admit | Confirmed in code + `TestDW_5_3_*`. |
| (c) quarantine reachable via tier / retrieval | No. Tier→SearchAdmitted→admitted index only; quarantine is a separate index, listed only via CLI admin. `TestTier_ExcludesQuarantinedAndPruned` + e2e poison scenario. |
| (d) provenance/scope spoofable from directive | No. DistillStage overwrites tenant/team/scope/owner/trajectory/sourceIDs from the trusted event; directive controls only content/outcome. Tested. |
| (e) prune hard-deletes admitted docs | No. SoftExpire stamps `expired_at` (partial `_update`), never deletes; Recover(includeExpired) proves recoverability. |
| (f) quarantine release automatic | No. `Release` invoked solely from `runQuarantineRelease` (CLI); no automatic caller. |

## Notes (non-blocking)
1. **Disclosed deviation — distillation only on fact-bearing events:** the distill stage runs in the post-fact worker pipeline, so a distillable completion event must also emit a fact (scenarios document the `fact:` + `experience:` pairing). No DW item requires distillation on fact-less events; accepted as a documented pipeline constraint.
2. **Disclosed deviation — unconditional ExperienceStore guard vs literal WriteGuard:** rather than a separate WriteGuard type, the guard is the Store as the sole admitted-index writer with a mandatory gate enforced at construction (ErrNoGate). This is arguably deeper — you cannot obtain an ungated store. Accepted; the "no ungated write path" property holds structurally at the store layer.
3. `admitMerged` MergedCount increment is not atomic under concurrent same-fingerprint admits; dedup (one doc, no double-count) is preserved regardless, only the count could be marginally off. Not a DW requirement.
4. RuleGatekeeper injection scan covers Task/Context/DistilledSkill/Evidence but not Outcome.Signals; Signals are never retrievable content and used only for contradiction detection, so no retrievable-poison surface is exposed.
5. Ran e2e in external mode against the already-running compose stack instead of `make e2e` (avoids rebuilding/tearing down the live stack); same Boot external-mode path the Makefile target exercises.

## Issues (if FAIL)
None.

**Verdict: PASS**
