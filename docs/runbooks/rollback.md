# Reference: Rollback procedures

This is a consolidated reference for the three rollback mechanisms Phase 7 built — the five incident runbooks (`01`–`05`) link here rather than repeating the mechanics. Not one of the "5 incident runbooks" itself (DW-7.7); it's the shared how-to they point at.

## 1. Blue/green ECS task-definition revert

**When:** a bad deploy is running in staging or prod and needs to be undone quickly, without waiting for a forward fix.

**How:** `cmd/engram-deploy -env <staging|prod> -rollback <engramd|worker|embed>`, or trigger the `rollback` job in `.github/workflows/deploy.yml` (`workflow_dispatch` with `rollback_service` set — validated against an allowlist before use).

**Mechanics:** `awsapi.Rollback` (deploy/aws/awsapi/converge.go) reads the service's current `PreviousTaskDefinitionARN` (set by the last `Converge`-driven `UpdateService` call — ECS task definitions are immutable, so the prior revision ARN always still resolves) and points the service back at it. It fails loudly, not silently, if there is no prior revision to revert to.

**Proof:** `deploy/aws/awsapi/converge_test.go`'s `TestRollback_RevertsTaskDefinition` (unit test against the fake Provisioner) proves the exact revert mechanics; `TestRollback_NoPriorRevisionFails` and `TestRollback_UnknownServiceFails` prove the loud-failure paths. Real-AWS execution is a documented manual step (no AWS credentials in this build environment).

## 2. OpenSearch restore-from-snapshot

**When:** data loss or corruption at the index/domain level (see `docs/runbooks/05-restore-from-snapshot.md` for the full procedure, RPO/RTO, and drill evidence).

**Mechanics:** snapshot to a registered repository (every 6h, RPO ≤24h) → restore into a NEW index name → verify → cut over. Never restores over a live index in place.

## 3. Versioned indices + alias flip (the "point of no return" mitigation)

**When:** a breaking index-template/mapping change is needed (e.g. adding a field that requires a different type than the existing mapping, or any change OpenSearch won't accept as a live mapping update). This is the plan's explicitly named point of no return — an in-place mapping mutation that goes wrong cannot be undone the way a task-definition or a snapshot restore can.

**Mechanics:** `deploy/aws/reindex` (`EnsureAliasedIndex`, `FlipAlias`) creates a new versioned concrete index (`<alias>-000002`) under the new template, backfills it, and atomically repoints a search alias from the old index to the new one via OpenSearch's multi-action `_aliases` API — there is never a moment with zero or two authoritative indices, and the old index's mapping is never touched.

**Procedure:**
1. Author the new template (episodic/semantic/etc. in `internal/store/templates/`).
2. `reindex.EnsureAliasedIndex(ctx, client, baseURL, alias, nextVersion, newTemplateJSON)` — creates the new index, does NOT move the alias yet.
3. Backfill the new index (reindex existing data, or let it fill naturally going forward, depending on the change's nature).
4. Verify the new index serves correctly (a few known-good queries, or a full e2e pass against it directly).
5. `reindex.FlipAlias(ctx, client, baseURL, alias, oldIndex, newIndex)` — the atomic cutover.
6. To revert: `FlipAlias` back to the old index — it is exactly as safe as the forward flip (same atomic mechanism), which is the whole point of never mutating in place.

**Proof:** `deploy/aws/reindex/alias_integration_test.go`'s `TestDW_7_Rollback_VersionedIndexAliasFlip` runs this exact sequence for real against the local pinned-3.1 cluster: creates v1, writes a doc, creates v2 under a DIFFERENT (breaking) mapping, asserts v1's mapping bytes are unchanged, flips the alias, asserts v1's mapping is STILL unchanged, and confirms a search through the alias sees only v2's data post-flip.

**Note:** this mechanism is deploy-time tooling; wiring the live retrieval/write path to resolve indices through an alias instead of the current fixed concrete names (`store.EpisodicIndex`/`store.SemanticIndex`) is a follow-on migration this package enables but does not itself perform.
