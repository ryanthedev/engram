# Review: Phase 2 - Backfill re-extraction + verification

## Executed Results (Step 0)
- Go test suite: `make test` → all packages `ok` (internal/worker, internal/store, internal/enrich, internal/graph, etc.); no failures.
- `go vet ./...` → no issues found.
- `gofmt -l .` → 3 pre-existing files flagged (`internal/eval/halu_test.go`, `internal/retrieval/acl_test.go`, `internal/worker/worker_integration_test.go`), none touched by this phase's diff (`git status` shows only `deploy/local/docker-compose.yml` modified and `scripts/backfill-reextract-rtd.sh` / discovery doc added) — pre-existing, non-blocking.
- Live system state queried directly: `memory_status` MCP call, `memory_search` MCP calls (4 known facts, k=3–5), direct OpenSearch aggregations against `engram-episodic-000001` and `engram-semantic-000001` on `localhost:9201`, `podman inspect`/`podman logs` on `local-engramd-1`. No mutating commands were run (no processed_at reset, no re-extraction sweep triggered by this review — the sweep had already converged before this review started).

## Requirement Fulfillment

### DW-2.1
PREMISE:  Worker re-extracted the tenant rtd events through the shim — extraction actually fired (not silently skipped, not ErrNoFacts for prose-bearing events).
EVIDENCE: `podman logs local-engramd-1 --since 30m` — repeated `INFO extraction cost model=gpt-4o-mini events=N` lines climbing monotonically 14→43→74→106→141→174→185 between 02:51:10 and 03:00:10, plus interleaved `INFO graph dedup decision name="<extracted fact name>"` lines naming real extracted content (e.g. "deleting duplicated content rather than extracting it into additional reference files", "retries silently forever with no error surfaced causing subagent to hang"). Zero `ERROR`, `panic`, or `fatal` lines in the same window (`grep -iE "level=ERROR|panic|fatal"` → empty).
TRACE:    processed_at reset (scoped, see edge case below) re-queues 184 rtd episodic docs → `ClaimBatch` (outbox.go:20) claims them → `ProcessEvent` (worker.go:242) builds `LedgerKey{..., ExtractorVersion:"v2"}` → no existing v2 ledger entry, so the `default` branch (worker.go:270-297) calls `w.extractor.Extract` → cost-log line emitted per batch → facts land under `extractor_version:"v2"` in `engram-semantic-000001` (confirmed: sampled facts show `"extractor_version":"v2"`).
VERDICT:  PASS

### DW-2.2
PREMISE:  memory_status reports semantic_count > 0 for tenant rtd.
EVIDENCE: Live `memory_status` MCP call (this review, this session): `{"healthy":true,"tenant_id":"rtd","user_id":"ryan","agent_id":"claude-code","episodic_count":185,"semantic_count":367,"opensearch_version":"3.1.0"}`.
TRACE:    MCP call → engramd gRPC status handler → OpenSearch count queries scoped to tenant rtd → `semantic_count: 367`.
VERDICT:  PASS

### DW-2.3
PREMISE:  A search over ≥3 known facts returns them from the semantic tier with faithful {subject,predicate,object} content.
EVIDENCE: Sampled 5 semantic docs directly from `engram-semantic-000001` (tenant rtd) to get known ground truth, then ran `memory_search` (k=3–5) on 4 of them, this session:
1. Query "Caddy reverse proxy proxies Bun port 3001" → semantic hit `{"subject":"Caddy reverse proxy target","predicate":"proxies to Bun on port","object":"3001","statement":"Caddy proxies traffic to Bun on port 3001."}` (source doc matches sampled ground truth exactly).
2. Query "engram is append-only..." (broad) → incidentally surfaced semantic hit `{"subject":"test-first development","predicate":"is more turn-expensive for agents than","object":"test-after development",...}` matching ground truth.
3. Query "Open-ended work should be given heuristics instead of rigid steps." → semantic hit `{"subject":"open-ended work","predicate":"should be given","object":"heuristics instead of rigid steps"}` — exact match to sampled ground truth.
4. Query "When designing skill instructions, the degrees of freedom should match the task fragility." → semantic hit `{"subject":"skill instruction design","predicate":"matches degrees of freedom to","object":"task fragility"}` — exact match to sampled ground truth.
TRACE:    OpenSearch sample (ground truth) → `memory_search` query in natural language paraphrase/verbatim of the statement → hybrid BM25+vector fused result → semantic-tier hit returned with `subject`/`predicate`/`object`/`statement` identical to the sampled ground truth, for 4/4 attempted facts (exceeds the ≥3 requirement).
VERDICT:  PASS

### DW-2.4
PREMISE:  Reconciliation completed with no dead-lettered events and no duplicate-fact explosion (repair backlog converged).
EVIDENCE: Direct OpenSearch aggregation on `engram-episodic-000001` filtered to `tenant_id:rtd`: `{"hits":{"total":{"value":185}}, "aggregations":{"unprocessed":{"doc_count":0},"dead_lettered":{"doc_count":0}}}`. Direct OpenSearch cardinality aggregation on `engram-semantic-000001` filtered to `tenant_id:rtd`: total hits `367`, `distinct_content_keys` cardinality `367` — exact match, meaning zero duplicate `content_key` values (no fact indexed twice). `podman logs` shows the extraction-cost `events=` counter plateauing at 185 for three consecutive minutes (02:57:10, 02:58:10, 02:59:10, 03:00:10) — the outbox drained and stayed empty (convergence).
TRACE:    Every rtd episodic doc has `processed_at` set (unprocessed=0) and none carry `dead_lettered:true` → outbox fully drained with no parked events. `semantic` doc count (367) == distinct `content_key` count (367) → every fact was created exactly once, no dedup failure or explosion.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-2.1–DW-2.4 are operationally verified per the plan (no unit tests apply — this is a live-system operational phase). Each item is covered by observed behavior from live MCP calls, live OpenSearch queries, and live container logs, captured above with exact commands/queries and their outputs.
- [x] Coverage matches the stated level (live system state, not a test suite) as specified in the dispatch prompt's "How to verify" section.
- No gaps: all 4 DW items have direct observed-behavior evidence gathered independently by this review (not copied from the discovery doc).

## Dead Code
None found. `scripts/backfill-reextract-rtd.sh` has no unused code paths; `bash -n` syntax check passed. The `deploy/local/docker-compose.yml` diff is a single flag addition with an explanatory comment — no dead config.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `ClaimBatch`/`claimOne` (outbox.go:20-104) use guarded `if_seq_no`/`if_primary_term` updates — a losing claim returns 409→`won=false`, not an error; `ProcessEvent`'s ledger claim (`ClaimLedger`) and reconcile-attempt loop (`maxReconcileAttempts=3`, worker.go:372) handle concurrent claims. Traced: two workers racing the same batch — one wins the guarded update, the other sees 409 and `continue`s past it (outbox.go:71-73). No defect found. |
| Error Handling | PASS | `ProcessEvent`'s extract branch (worker.go:274-287): `errors.Is(err, ingest.ErrNoFacts)` completes cleanly with `Extraction: []byte("[]")` and no semantic write (edge case 2, verified against source); any other extractor error returns unwrapped, leaving the event unprocessed for the outbox's lease-expiry retry (bounded by `MaxAttempts`, then dead-lettered). Traced: a malformed-output error from the shim → `ProcessEvent` returns error → `Tick` logs it and does not call `Complete` → event stays claimed until lease expires → retried. No silent failure found. |
| Resources | PASS | The backfill script's `_update_by_query` sets `refresh=true&conflicts=proceed`, tolerating concurrent version bumps without leaking a stuck update. No file handles/connections opened outside the standard `curl`/OpenSearch HTTP request-response cycle. |
| Boundaries | PASS | Snapshot query traced: `if total != len(hits): sys.exit(1)` (backfill script lines 37-40) — guards against OpenSearch returning fewer hits than `total` (e.g., if the true count exceeds `size:1000`), which would otherwise silently under-snapshot. At the observed scale (184 rtd events), well under 1000, this guard did not trigger, but it is present and correct for the boundary case. |
| Security | PASS | The scoped `processed_at` reset (edge case 1) is the security-sensitive operation in this phase — see the dedicated edge-case row below. No other untrusted input is processed in this phase's diff. |

## Loaded-Skill Criteria
N/A — no `## Additional Skills` block beyond the two invoked skills (`cc-debugging`, `engram-memory`), which are process/domain-knowledge skills for this review's own methodology rather than skills whose criteria apply as extra pass/fail gates on the reviewed code. `cc-debugging`'s scientific method was followed to ground every verdict in observed live-system output before concluding; `engram-memory`'s recall/ingest discipline informed how `memory_search`/`memory_status` evidence was gathered and interpreted (hit `source` field distinguishes episodic/semantic/graph tiers; `fields_json` read as evidence, not gospel, and cross-checked against direct OpenSearch queries rather than trusted at face value).

| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-debugging | Ground every verdict in an observed run's actual output, not inferred/"looks correct" | PASS | Every DW verdict above cites a live command/query executed in this review session, not the discovery doc's prior claims. |
| engram-memory | Treat a search hit as evidence, not gospel; corroborate | PASS | DW-2.3's semantic hits were corroborated against directly-sampled OpenSearch ground truth (not accepted from `memory_search` alone). |

## Edge Cases

**Scoped processed_at reset — tenant isolation and snapshot-before-mutate (safety-critical):**
- `scripts/backfill-reextract-rtd.sh` (read in full): both the snapshot query (lines 26-33) and the mutating `_update_by_query` (lines 49-59) filter with `{"term": {"tenant_id": "'"${TENANT}"'"}}` — an exact-match term filter, not a wildcard/prefix/regex, so no cross-tenant bleed is structurally possible regardless of what other tenant ids exist in the index.
- Snapshot-before-mutate confirmed: the script writes affected event ids to a file *before* running the destructive update (lines 25-45 precede lines 47-59), and asserts `total == len(hits)` before writing (lines 37-40), refusing to proceed on a truncated snapshot.
- Confirmed this actually ran, and ran scoped to `rtd`: found the produced snapshot file at `/private/tmp/claude-501/.../scratchpad/backfill-rtd-20260709T025019Z-event-ids.txt` (184 lines, e.g. `engram-port:self/user/prod-ops-handle-it-dont-ask#2`) — the filename embeds `TENANT=rtd` (script line 23: `SNAPSHOT_FILE="${SNAPSHOT_DIR}/backfill-${TENANT}-...`), confirming the script was invoked with `rtd` and not some other tenant.
- `bash -n scripts/backfill-reextract-rtd.sh` → syntax OK.
- **Verdict: PASS** — tenant-scoped, snapshot-before-mutate confirmed both in the script's logic and in its actual executed output.

**Legitimate no-fact extraction ([]) is not a failure:**
- Verified against source (worker.go:274-282): `ErrNoFacts` is handled as a clean completion path (`LedgerComplete`, `Extraction: []byte("[]")`), never conflated with an extraction error. No evidence in this review contradicts this — not directly falsifiable from live state alone (there's no negative-space signal for "this event correctly produced zero facts" vs "this event was never claimed"), but the source-level handling is correct and the aggregate results (185/185 processed, 0 dead-lettered) are consistent with it.
- **Verdict: PASS** (source-verified, no observed violation).

**Re-extraction must not duplicate existing facts:**
- OpenSearch cardinality check: `distinct_content_keys` = 367 == total semantic doc count (367) for tenant rtd. Zero duplicate `content_key` values.
- **Verdict: PASS**.

## Notes (non-blocking)
- `episodic_count` grew from the discovery doc's stated baseline of 183 to 185 between discovery and this review (185 confirmed live, snapshot file shows 184 events reset) — consistent with 1-2 new episodic events ingested by other sessions/tools during the intervening window (e.g. this review's own MCP tool usage, or other agent activity), not a defect in this phase's work.
- `gofmt` flags 3 pre-existing test files unrelated to this phase's diff; not chased per this phase's scope (no Go code was added).
- The compose bump was verified live, not just in the file: `podman inspect local-engramd-1 --format '{{.Config.Cmd}}'` shows the running container's actual command line ends in `-extractor-version v2` and `-extract-url http://host.docker.internal:8088`, and the main-repo compose file (`/Users/r/repos/engram/deploy/local/docker-compose.yml`, which is what the live `local` compose project actually reads) is byte-identical to the worktree's copy — confirming the deployed config matches the diff under review, not just the source file.

**Verdict: PASS**
