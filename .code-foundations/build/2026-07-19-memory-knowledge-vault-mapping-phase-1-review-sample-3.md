# Review: Phase 1 - token create --roles (role-bearing token minting)

Sample 3. Reviewed files: `internal/cli/cli.go`, `internal/cli/cli_test.go` (worktree `.claude/worktrees/memory-knowledge-vault-mapping`).

## Executed Results (Step 0)
- Test suite: `go test -count=1 ./internal/cli/ ./internal/auth/` → both `ok` (cli: 108 top-level tests, 0 failures; auth: pass)
- Build/typecheck: `go build ./...` → clean, no output
- Lint: `make lint` (`go vet` + `revive`) → clean, exit 0

## Requirement Fulfillment

### DW-1.1
PREMISE:  `token create ... --roles admin,harvester` mints a token; verifying it yields `Roles == ["admin","harvester"]` (order-normalized).
EVIDENCE: cli.go:139 (flag), cli.go:145 (`Roles: parseRoles(*roles)`), cli.go:167-173 (`parseRoles`), auth/auth.go:241 (`normalizeRoles` at mint), auth/auth.go:135 (`normalizeRoles` at verify). Test: cli_test.go:276-290 `TestDW_1_1_TokenCreateRolesFlagVerifies` — PASS in Step 0.
TRACE:    `--roles admin,harvester` → parseRoles → `["admin","harvester"]` → Issue normalizes (trim/dedupe/first-seen order) → Put to fake OS → real `auth.Authenticator.Verify` against same store → `Identity.Roles == ["admin","harvester"]` asserted via `reflect.DeepEqual`. Order is deterministic (first-seen preserved by `normalizeRoles`, auth/auth.go:308-320).
VERDICT:  PASS

### DW-1.2
PREMISE:  `token create` with no `--roles` mints a role-less token exactly as before (regression).
EVIDENCE: cli.go:167-171 (empty/omitted → `nil`, never `[""]`); auth/auth.go:121 (`Roles []string json:"roles,omitempty"` — role-less record marshals with no roles key, byte-identical to pre-roles records). Test: cli_test.go:295-305 `TestDW_1_2_TokenCreateNoRolesFlagIsRoleless` — PASS in Step 0.
TRACE:    omitted flag → `*roles == ""` → `parseRoles("")` → `nil` → `normalizeRoles(nil)` → `nil` → record persisted without a `roles` field → Verify → `len(id.Roles) == 0` asserted.
VERDICT:  PASS

### DW-1.3
PREMISE:  dirty input `--roles " admin , , harvester "` normalizes to `["admin","harvester"]` (no empty/whitespace entries) — table test.
EVIDENCE: cli_test.go:310-336 `TestDW_1_3_TokenCreateRolesNormalization` — table test with 5 cases including the exact dirty input `" admin , , harvester "`, duplicates, whitespace-only, trailing comma, clean single role — all subtests PASS in Step 0. Normalization implemented once in auth/auth.go:308-320 (`normalizeRoles`), applied at mint (auth.go:241) and verify (auth.go:135).
TRACE:    `" admin , , harvester "` → `parseRoles` (TrimSpace, Split on ",") → `["admin"," "," harvester"]`-ish raw segments → `Issue` → `normalizeRoles`: trim each, drop empty, dedupe → `["admin","harvester"]` persisted → Verify → asserted `["admin","harvester"]`. Persistence-side normalization independently proven by `internal/auth` `TestIssueNormalizesRoles` (PASS), so the CLI test's verify-path assertion is not masking a dirty stored record.
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-1.1 → `TestDW_1_1_TokenCreateRolesFlagVerifies` (ran, PASS)
- [x] DW-1.2 → `TestDW_1_2_TokenCreateNoRolesFlagIsRoleless` (ran, PASS)
- [x] DW-1.3 → `TestDW_1_3_TokenCreateRolesNormalization` table test (ran, all 5 subtests PASS)
- [x] Coverage level "100% of new/changed functions": `go tool cover -func` → `parseRoles` 100.0%; `runTokenCreate` 88.9% where the uncovered branches (cli.go:142-144 flag-parse error return, cli.go:150-152 Issue error return) are pre-existing lines untouched by this diff (confirmed via `git diff HEAD -- internal/cli/cli.go`); every added/changed line is exercised. Usage-banner change covered by `TestUsageDocumentsRolesFlag` (PASS).

## Edge Cases (prompt-listed)
| Edge case | Evidence | Verdict |
|---|---|---|
| empty/whitespace-only `--roles` → no roles | `parseRoles` cli.go:168-171 returns `nil`; DW-1.3 subtest `whitespace-only value` (`"   "` → `nil`) PASS; DW-1.2 covers omitted flag | PASS |
| duplicate/whitespace-padded names normalized | DW-1.3 subtests `padded with empty segments`, `duplicate roles`, `trailing comma` all PASS | PASS |
| unknown role strings accepted at mint | No allowlist exists anywhere in the mint path (auth.go:225-251 — only `id.Valid()` tenant/user check; `normalizeRoles` never rejects content). Demonstrated by reviewer-authored overlay test `TestReview_UnknownRolesAcceptedAtMint` (`--roles frobnicator,totally-made-up` → exit 0, verified Roles `["frobnicator","totally-made-up"]`) — ran via `go test -overlay`, PASS; worktree not modified | PASS |

**Adversarial checks:**
- Role smuggling by any path other than `--roles`: `Identity.Roles` at mint derives solely from `parseRoles(*roles)` (cli.go:145); no env var, no positional arg, no other flag feeds it. At verify, roles come only from the stored record (auth.go:135); `TestDW_2_1_ClientSuppliedRolesIgnored` (auth package, PASS) proves caller-supplied roles on requests are ignored. No smuggling path found.
- Flag parsing corrupting tenant/user/agent or the token: stdlib `flag` gives each flag an independent pointer; commas inside the `--roles` value are opaque to the parser and cannot terminate or redefine other flags. `TestTokenCreateStillRequiresTenantAndUser` (PASS) proves `--roles` does not weaken the tenant/user requirement. The raw token itself is generated from `crypto/rand` entropy (auth.go:229) after parsing — flag input cannot influence it.

## Dead Code
None found in the changed code. New `strings` import is used; `parseRoles` is called; no debug statements, no commented-out blocks, no unreachable code.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | CLI invocation is single-threaded; no shared state introduced. (Test fake uses a mutex — test code.) |
| Error Handling | PASS | Flag-parse errors returned (cli.go:142-144); Issue/store errors returned and wrapped (cli.go:150-152, auth.go:247-249). Probed `--roles ","` and `--roles "--tenant"` shapes via trace: both flow through Split → normalizeRoles without panic or misparse. |
| Resources | N/A | No new handles/connections in the diff; HTTP client lifecycle unchanged. |
| Boundaries | PASS | Empty string, whitespace-only, trailing comma (empty tail segment), duplicates, single element — all traced and test-proven. `Split` on a comma-only value yields all-empty segments → `nil` after normalization. |
| Security | PASS | Roles claim sanitized at mint (auth.go:241) AND re-normalized at verify (auth.go:135) — defense-in-depth on the security-critical claim path. Raw token never persisted (hash only); roles cannot influence token entropy or handle. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | No executable code in assertions | N/A | No assertions in the diff (Go, no assert usage). |
| cc-defensive-programming | No empty catch blocks / swallowed errors | PASS | Every error in changed production code is returned; discarded writes (`_ = json.NewEncoder...`) are in test fakes, exempt per skill. |
| cc-defensive-programming | External input validated at entry | PASS | CLI args are external input: tenant/user required-check at entry (cli.go:146-147, proven by `TestTokenCreateStillRequiresTenantAndUser`); roles normalized inside the auth barricade before persistence (auth.go:241). Single validation implementation (no drift), matching the skill's barricade rule. |
| cc-defensive-programming | Barricade reduces redundancy but security-critical paths validate again (defense-in-depth) | PASS | Claim barricade is two-halved: mint-time `normalizeRoles` (auth.go:241) + read-time re-normalization in `TokenRecord.Identity()` (auth.go:135), each with passing dedicated tests (`TestIssueNormalizesRoles`, `TestTokenRecordIdentityNormalizesRoles`). |
| cc-defensive-programming | Assertions for bugs only / anticipated errors handled | PASS | Anticipated bad input (dirty roles, missing flags) uses error handling, not assertions. |

## Notes (non-blocking)
- `TestDW_1_1` asserts only `Identity.Roles` on the verified token, not that TenantID/UserID survived alongside the new flag; the trace (independent flag pointers, cli.go:136-145) shows no corruption path, and my overlay run confirmed a clean mint+verify, so this is a test-thoroughness observation only.
- "Order-normalized" in DW-1.1 resolves to first-seen order preservation (auth.go:304), not sorting. For the specified input the two coincide; the behavior is deterministic and documented, so no ambiguity in practice.
- `parseRoles` deliberately leaves per-entry cleanup to `normalizeRoles` (comment at cli.go:158-166). Defensible single-implementation choice; noted only because a future non-Issue consumer of `parseRoles` output would receive untrimmed segments.

## Issues
None.

**Verdict: PASS**
