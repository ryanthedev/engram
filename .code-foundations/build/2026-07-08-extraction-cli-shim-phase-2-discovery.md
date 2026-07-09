# Discovery + Design: Phase 2 - Backfill re-extraction + verification

## Files Found

- `cmd/engram-server/main.go` — `-extractor-version` flag (default `"v1"`), wired into `worker.Config.ExtractorVersion`.
- `internal/worker/worker.go` — `ProcessEvent` claims the ledger with `LedgerKey{TenantID, EventID, ExtractorVersion}` (line 246); the ledger claim is where a version bump would matter.
- `internal/store/outbox.go` — `ClaimBatch` (line 20-41) and `Complete` (line 111-115): the outbox scan/claim mechanism.
- `internal/store/ledger.go` — `ClaimLedger` (line 56-89): op_type=create against a doc id keyed by `(tenant, event_id, extractor_version)`.
- `internal/memory/ids.go` — `LedgerKey.DocID()` = `sha256(tenant_id·event_id·extractor_version)` (line 42-44).
- `internal/memory/record.go` — `Episodic.ProcessedAt *time.Time` (outbox gate field).
- `deploy/local/docker-compose.yml` (worktree, already rewired in Phase 1) — engramd command args; no `-extractor-version` flag currently set (defaults `v1`).
- `Makefile` — `extract-shim` target (`go run ./cmd/engram-extract-shim -addr $(SHIM_ADDR) -backend $(BACKEND)`, `SHIM_ADDR ?= :8088`); `E2E_OS_URL := http://localhost:9201`, `E2E_ADDR := localhost:7071`.
- `cmd/engram-extract-shim/main.go` — default backend `agy`, default addr `:8088`, default timeout 60s.

## Current State

**Live containers** (`podman ps`): compose project `local`, config file `/Users/r/repos/engram/deploy/local/docker-compose.yml` (the **main repo checkout**, not this worktree). Services: `local-stub-llm-1`, `local-embed-1`, `engram-e2e-os` (opensearch, host port **9201**), `local-engramd-1` (host port **7071**). A separate, unrelated standalone container `engram-dev-os` runs on host port 9200 — not part of this compose project, must not be touched.

The main repo's `deploy/local/docker-compose.yml` (what the running stack actually reads) does **not** yet have the Phase 1 rewire — that rewire is only committed on this worktree's branch. Diffing worktree vs main-repo compose file confirms the only engramd changes are: `-extract-url` → `http://host.docker.internal:8088`, `extra_hosts: host.docker.internal:host-gateway`, and the dropped `stub-llm` healthcheck dependency. No network/port/volume changes — safe to overlay onto the main-repo file and recreate just `engramd`.

**Direct OpenSearch queries against the live stack** (`localhost:9201`, confirmed via `memory_status` too):
- `engram-episodic-000001`: 183 docs with `tenant_id: rtd`, **all 183 have `processed_at` set**, 0 dead-lettered.
- `engram-ledger-000001`: 183 entries for tenant rtd, **all `extractor_version: v1`, all `phase: complete`**.
- `engram-semantic-000001`: 3 docs total (cluster-wide, not rtd — almost certainly the Phase 1 live-agy-smoke-test event's output; tenant rtd's semantic_count is 0 per `memory_status`).
- `memory_status` (tenant rtd, live call): `episodic_count: 183, semantic_count: 0` — confirms the plan's stated baseline exactly.

## Gaps

None against the plan's assumptions — the plan anticipated exactly this fork ("version bump alone" vs "fallback processed_at reset") and told me to confirm against source before executing. Confirmed below.

## Code Standards

No `docs/code-standards.md` found in this repo. Followed the existing style in `scripts/` (none exist yet) and `cmd/engram-extract-shim` for the new backfill script: plain bash, commented, fails loudly (`set -euo pipefail`), no dependencies beyond `curl` and `jq` (already used elsewhere in this repo's tooling per Makefile).

## Test Infrastructure

This phase is operationally verified per the plan (no unit tests to write) — DW items are proven with live command output (`podman`/`curl`/MCP tool calls), not a test suite. No test framework changes.

## DW Verification

| DW-ID | Done-When Item | Status | Evidence Plan |
|-------|---------------|--------|----------------|
| DW-2.1 | Worker re-extracts rtd events through the shim, observed in engramd logs (extraction calls firing, not ErrNoFacts for prose-bearing events) | COVERED | `podman logs -f local-engramd-1` tailed during the sweep; grep for extraction/shim-call log lines vs `ErrNoFacts` |
| DW-2.2 | `memory_status` reports `semantic_count > 0` for tenant rtd | COVERED | `memory_status` MCP call before/after |
| DW-2.3 | `memory_search` over ≥3 known ported facts returns them from the semantic tier with faithful `{subject,predicate,object}` content | COVERED | `memory_search` with `k=3` against 3+ specific known rtd facts, spot-checked verbatim |
| DW-2.4 | Reconciliation completes with no dead-lettered events and no duplicate-fact explosion | COVERED | Post-sweep OpenSearch aggregation: `dead_lettered:true` count on `engram-episodic-000001` (must stay 0), and a `content_key` cardinality/duplicate check on `engram-semantic-000001` for tenant rtd |

**All items COVERED:** YES

## Design Decision: version bump vs processed_at reset

**Question:** does bumping `-extractor-version` (v1→v2) alone re-claim the 183 already-`processed_at`-stamped rtd events?

**Answer, confirmed against source: NO — both mechanisms are required together.**

Two independent gates exist, and each event must clear both:

1. **Outbox claim gate** (`internal/store/outbox.go:20-41`, `ClaimBatch`): the OpenSearch query is
   ```
   must_not: [ exists(processed_at), term(dead_lettered=true) ]
   ```
   This is the ONLY gate that determines whether `Worker.Tick` ever sees an event again. It has **no knowledge of `extractor_version` at all** — that field isn't even in the query. Since all 183 rtd episodic docs already carry `processed_at` (confirmed by direct aggregation query above), `ClaimBatch` will never return them again, no matter what `-extractor-version` is set to. **A version bump alone is a no-op for already-processed events.**

2. **Ledger claim gate** (`internal/store/ledger.go:56-89`, `ClaimLedger`, keyed by `internal/memory/ids.go:42` `LedgerKey.DocID() = sha256(tenant·event_id·extractor_version)`): *if* an event reaches `ProcessEvent` (i.e., if it clears gate 1), the ledger key changes when `extractor_version` changes, so `ClaimLedger` gets a **fresh doc-create (201)** instead of hitting the existing v1 `LedgerComplete` entry. This matters because `ProcessEvent`'s switch (worker.go:254-297) treats an existing `LedgerComplete` entry as a **replay short-circuit** — it calls `w.complete()` and returns WITHOUT calling the extractor at all. So if we reset `processed_at` but leave `extractor_version` at `v1`, the event re-enters the outbox, gets claimed, hits `ClaimLedger` with the same v1 key, finds `Phase: complete`, and just re-stamps `processed_at` again — **no re-extraction happens.**

**Conclusion:** the mechanisms are complementary, not alternatives — the fallback isn't a fallback, it's a required second step:
- Bump `-extractor-version` to `v2` on `engramd` (ensures gate 2 doesn't short-circuit).
- Scoped `processed_at` reset for tenant `rtd` on `engram-episodic-000001` (clears gate 1 so the events re-enter the outbox at all).

This is a stronger, source-verified version of the plan's framing ("if version bump alone does NOT re-claim... fall back to...") — the two are not alternatives to try in sequence, they are both required from the start given the confirmed state (183/183 already `processed_at`-stamped).

**Rollback safety:** the reset only clears `processed_at` (and implicitly re-enables the outbox claim); it does not touch `attempts`, ledger entries, or any semantic data. The v1 ledger entries and any v1 semantic writes (none exist for rtd currently) are untouched — re-extraction lands under fresh v2-keyed ledger entries and content-addressed fact doc ids, so it is purely additive. Event ids are snapshotted to a file before the reset per the plan's rollback note.

## Prerequisites

- [x] Phase 1 shim exists and builds (`cmd/engram-extract-shim`)
- [x] Worktree's `deploy/local/docker-compose.yml` already rewired (host.docker.internal:8088, extra_hosts, relaxed stub-llm dependency)
- [x] Live stack running under compose project `local`, reachable (opensearch :9201, engramd :7071)
- [x] `agy` CLI present on host (`/Users/r/.local/bin/agy`)
- [ ] Shim not yet running (must start before any engramd recreate/reset)
- [ ] engramd not yet recreated with the rewired config + v2 extractor version
- [ ] processed_at not yet reset

## Recommendation

**BUILD.** Plan:
1. Add `-extractor-version` / `v2` to the worktree's `deploy/local/docker-compose.yml` engramd command.
2. Build and start `cmd/engram-extract-shim` on the host (`:8088`, backend `agy`), confirm `/health`.
3. Copy the worktree's rewired compose file over the main-repo path the running project reads from (`/Users/r/repos/engram/deploy/local/docker-compose.yml`), then recreate ONLY `engramd` in place under project `local` (`podman compose -p local -f <path> up -d --no-deps engramd`). Do not touch opensearch/embed/stub-llm.
4. Confirm engramd is healthy and can reach the shim (watch startup logs; no immediate crash-loop).
5. Snapshot the 183 rtd event ids to a file (rollback evidence), then run the scoped `processed_at` reset (`scripts/backfill-reextract-rtd.sh`) against `engram-episodic-000001` for `tenant_id: rtd`.
6. Watch engramd logs during the ~11-minute sweep; poll `memory_status` periodically.
7. Verify DW-2.2 through DW-2.4 once the sweep converges (outbox drains, no dead-letters, semantic_count stable).
