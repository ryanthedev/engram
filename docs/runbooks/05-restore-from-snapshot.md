# Runbook: Restore from snapshot (data loss / corruption recovery)

**Trigger:** an OpenSearch domain/index is lost, corrupted, or a bad write (e.g. a botched manual `_update_by_query`) needs to be rolled back to a known-good point in time.

**RPO (Recovery Point Objective): ≤ 24 hours.** Snapshot policy takes an automated snapshot every 6 hours (4x/day) to a registered repository, giving an actual worst-case data loss window of 6 hours — comfortably inside the 24h contract with headroom for a missed/delayed snapshot.

**RTO (Recovery Time Objective): ≤ 1 hour**, for the common case (restoring into an already-running domain — the domain itself did not need to be re-provisioned). A full domain loss (the domain itself is gone, not just an index) additionally requires `cmd/engram-deploy`'s Converge to recreate the domain (OpenSearch Service domain creation is itself typically 15–30 minutes) before the snapshot restore can even begin — this is the scenario most likely to threaten the 1h RTO and is called out explicitly below.

## Snapshot policy

- Repository: a registered snapshot repository (`fs` locally for the drill; `s3` for AWS — OpenSearch Service supports a manual S3 repository registration).
- Schedule: every 6 hours, retaining the last 28 snapshots (7 days) — old snapshots are pruned automatically so the repository doesn't grow unbounded.
- Scope: the semantic (T2), episodic (T1), experience (T3), and graph (T4) indices, plus acl-edges and auth-tokens (identity/authorization state must restore consistently with the data it gates — restoring data without its ACL edges would be a silent security regression).

## Detection

- The trigger for this runbook is usually runbook 03 (OpenSearch node failure) escalating to "the domain/index is unrecoverable," or a manual discovery of bad data (an operator or an eval-gate regression flags corrupted/incorrect content that a targeted fix can't cleanly undo).

## Procedure

1. **Confirm the restore target.** Pick the most recent snapshot at or before the known-good point in time. List available snapshots:
   ```
   GET /_snapshot/<repo>/_all
   ```
2. **If the domain itself is intact** (only data is bad/lost): restore into a **new, differently-named index** — never restore over a live index in place (that would risk a partial in-place mutation if the restore is interrupted, the same "never in-place" discipline as the index-template migration path, `deploy/aws/reindex`):
   ```
   POST /_snapshot/<repo>/<snapshot>/_restore
   {
     "indices": "engram-semantic-000001",
     "rename_pattern": "engram-semantic-000001",
     "rename_replacement": "engram-semantic-restored"
   }
   ```
3. **Verify the restored index** (doc counts, a few known-good queries against `engram-semantic-restored`) before it takes traffic.
4. **Cut over** using the SAME alias-flip mechanism the index-template migration path uses (`deploy/aws/reindex.FlipAlias`) if the live path is alias-mediated, or update `engram-server`'s `-url`/index-name flags and redeploy if not — either way, the cutover is atomic, never a window with both the bad and restored index partially live.
5. **If the domain itself is gone:** run `cmd/engram-deploy`'s Converge for the environment first (recreates the OpenSearch Service domain per the environment's `DomainSpec`), wait for it to report `active`, THEN perform steps 1–4 against the new domain.
6. **Confirm e2e green** against the restored data: run the e2e suite (or its cloud profile against staging) end-to-end through MCP/CLI/gRPC — the DW-7.4 contract is "restore drill passes... e2e green," not just "the index exists."

## No silent data loss beyond the RPO window

Because the outbox/repair-sweep protocol (D10–D13) is itself durable and idempotent, restoring T1/T2 to a snapshot taken up to 6 hours ago and then **replaying episodic events newer than the snapshot** (they are still in the outbox/durable store if the loss was index-level, not domain-level) can shrink the effective data loss window below the raw RPO in many failure modes — worth checking before accepting the full 6-hour loss as unavoidable.

## Tabletop walkthrough

**Walked:** 2026-07-04, against the real local pinned-3.1 OpenSearch cluster with a local `fs` snapshot repository (`e2e/cloud`'s `TestRestoreDrill`, passing): registered the repo, took a snapshot of a scratch semantic index seeded with two known fixture facts, deleted the index (simulating loss, confirmed gone via a HEAD check), restored it under a new name via the snapshot's `rename_pattern`/`rename_replacement`, and verified via a direct search against the restored index that both fixture facts came back byte-for-byte.

**Timing observed:** snapshot registration + restore + verification completed in ~280ms against the local single-node cluster — comfortably inside the 1h RTO for the "domain intact, index lost" case. The "domain also gone" case's ~15–30 minute domain-creation floor is not exercisable locally (no real AWS) and is flagged as the RTO's real risk margin — a real-AWS restore drill is the documented manual-verification step for that specific sub-case.

**Gap found:** none in the mechanism during this walkthrough — the drill and this runbook were both authored from the start around restoring under a new name and cutting over explicitly (never in place), per the phase's rollback contract, so there was no "wrong first draft" to catch here. The one real gap the walkthrough did surface: the local `path.repo` setting the drill depends on (`scripts/dev-cluster.sh`) was not originally set on the dev cluster at all — a fresh `make dev-cluster` before this phase would have failed the drill with a repository-registration error. Fixed by adding `path.repo` to the dev-cluster script and re-verified against a freshly recreated container.

**Not yet exercised:** the "domain also gone" sub-case (full OpenSearch Service domain loss, requiring `cmd/engram-deploy`'s Converge to recreate the domain before any restore can begin) — no real AWS account is available in this build environment; this remains a real-AWS manual-verification step.
