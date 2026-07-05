# Runbook: Index-template / mapping migration (breaking change)

The operator procedure `deploy/aws/reindex/alias.go` points to. Not one of the five DW-7.7 incident runbooks — a referenced operational procedure for the plan's explicitly named **point of no return**: a breaking index-template/mapping change.

A change OpenSearch will accept as a live mapping update (e.g. adding a new field of a new type) can be applied in place. A **breaking** change — changing an existing field's type, or anything OpenSearch rejects on a live index — cannot be undone in place if it goes wrong. This procedure avoids ever mutating a live mapping: it builds a new versioned index under the new template and atomically flips a search alias onto it. This is the same discipline `docs/runbooks/rollback.md` section 3 summarizes; this file is the step-by-step.

## Mechanism

`deploy/aws/reindex` (`EnsureAliasedIndex`, `FlipAlias`, `CurrentAliasTarget`) creates a new concrete index `<alias>-000002` from the new template, backfills it, and flips the alias from the old index to the new one via OpenSearch's atomic multi-action `_aliases` API — never a moment with zero or two authoritative indices behind the alias, and the old index's mapping is never touched.

## Procedure

1. **Author the new template** in `internal/store/templates/` (episodic/semantic/etc.).
2. **Create the new versioned index** — this does NOT move the alias yet:
   ```go
   idx, created, err := reindex.EnsureAliasedIndex(ctx, client, baseURL, alias, nextVersion, newTemplateJSON)
   ```
3. **Backfill** the new index. Depending on the change:
   - Reindex existing data via OpenSearch `_reindex` from the old index into the new one (for a change that transforms existing docs), or
   - Let it fill going forward (for a change that only affects new writes).
4. **Verify** the new index serves correctly — run a few known-good queries against `<alias>-000002` directly, or a full e2e pass against it, BEFORE cutting over.
5. **Cut over atomically:**
   ```go
   err := reindex.FlipAlias(ctx, client, baseURL, alias, oldIndex, newIndex)
   ```
6. **To revert:** `FlipAlias` back to the old index — exactly as safe as the forward flip (same atomic mechanism). This is the whole reason for never mutating in place: the rollback is a one-call alias flip, not an unrecoverable mapping repair.

## Point of no return — mitigated, not eliminated

The only genuinely irreversible step is discarding the old index after a successful cutover. Keep the old versioned index (and a snapshot — `docs/runbooks/05-restore-from-snapshot.md`) until the new one is confirmed healthy in production for long enough that a revert is no longer plausible.

## Proof

`deploy/aws/reindex/alias_integration_test.go`'s `TestDW_7_Rollback_VersionedIndexAliasFlip` runs this exact sequence for real against the local pinned-3.1 cluster: creates v1, writes a doc, creates v2 under a DIFFERENT (breaking) mapping, asserts v1's mapping bytes are unchanged, flips the alias, asserts v1's mapping is STILL unchanged, and confirms a search through the alias sees only v2's data post-flip.

## Follow-on note

The live retrieval/write path currently resolves fixed concrete index names (`store.EpisodicIndex` / `store.SemanticIndex`), not an alias. Wiring the runtime path to resolve through an alias — so this migration mechanism applies to the live indices without a code change — is a follow-on this package enables but does not itself perform.
