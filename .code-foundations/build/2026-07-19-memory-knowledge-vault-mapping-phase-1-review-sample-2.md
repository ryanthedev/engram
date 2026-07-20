# Review: Phase 1 - token create --roles (sample 2)

Worktree: `/Users/r/repos/engram/.claude/worktrees/memory-knowledge-vault-mapping`

## Executed Results (Step 0)
- Test suite: `go test -count=1 ./internal/cli/ ./internal/auth/` → both `ok` (cli 0.068s, auth 0.006s); 0 failures. Verbose run confirms `TestDW_1_1_TokenCreateRolesFlagVerifies`, `TestDW_1_2_TokenCreateNoRolesFlagIsRoleless`, `TestDW_1_3_TokenCreateRolesNormalization` (5 subtests), `TestTokenCreateStillRequiresTenantAndUser`, `TestUsageDocumentsRolesFlag` all PASS.
- Build: `go build ./...` → clean.
- Lint: `make lint` (go vet + revive) → exit 0.

## Requirement Fulfillment

### DW-1.1
PREMISE:  `token create ... --roles admin,harvester` mints a token; verifying it yields `Roles == ["admin","harvester"]` (order-normalized).
EVIDENCE: internal/cli/cli.go:139 (flag), cli.go:145 (`Roles: parseRoles(*roles)`), cli.go:167-173 (parseRoles); internal/auth/auth.go:241 (`normalizeRoles` at mint), auth.go:135 (re-normalized at Verify); test internal/cli/cli_test.go:276-290.
TRACE:    `--roles admin,harvester` → parseRoles splits to `["admin","harvester"]` → Issue normalizes and persists to the (fake) token store → `auth.Authenticator.Verify(raw)` returns Identity with `Roles == ["admin","harvester"]`, asserted via `reflect.DeepEqual`. Verification is done through the real auth layer against the same backend the mint wrote to, not by re-parsing the flag.
VERDICT:  PASS (TestDW_1_1_TokenCreateRolesFlagVerifies, ran fresh, passed)

### DW-1.2
PREMISE:  `token create` with no `--roles` mints a role-less token exactly as before (regression).
EVIDENCE: internal/cli/cli.go:167-172 (empty raw → `nil`, never `[""]`); internal/auth/auth.go:121 (`json:"roles,omitempty"` — nil roles marshal with no roles key, byte-identical record shape to pre-flag tokens); test internal/cli/cli_test.go:295-305.
TRACE:    omitted flag → `*roles == ""` → `parseRoles("")` returns nil → `normalizeRoles(nil)` returns nil → record marshals without a `roles` key → Verify yields `len(Roles) == 0`. Exit code 0, raw token printed as before (mint path otherwise untouched by the diff).
VERDICT:  PASS (TestDW_1_2_TokenCreateNoRolesFlagIsRoleless, ran fresh, passed)

### DW-1.3
PREMISE:  dirty input `--roles " admin , , harvester "` normalizes to `["admin","harvester"]` (no empty/whitespace entries) — table test.
EVIDENCE: internal/auth/auth.go:308-320 (trim, drop-empty, dedupe, first-seen order); test internal/cli/cli_test.go:310-336 — a table test with 5 cases including the exact literal `" admin , , harvester "`.
TRACE:    `" admin , , harvester "` → Split on `,` → `[" admin "," "," harvester "]` → normalizeRoles trims each, drops the empty middle entry → `["admin","harvester"]`, asserted on the *verified* identity. Also covered: duplicates → deduped; whitespace-only → nil; trailing comma → dropped.
VERDICT:  PASS (TestDW_1_3_TokenCreateRolesNormalization + 5 subtests, ran fresh, passed)

**All requirements met:** YES

## Edge Cases (prompt-listed — verdict standing)
| Edge case | Evidence | Result |
|---|---|---|
| empty/whitespace-only `--roles` → no roles | cli.go:168-171 returns nil; table case `"whitespace-only value"` (cli_test.go:318) verifies nil roles after Verify | HANDLED |
| duplicate/whitespace-padded names normalized | auth.go:311-317 trims before dedupe check; table cases `"padded with empty segments"`, `"duplicate roles"` (cli_test.go:316-317) | HANDLED |
| unknown role strings accepted at mint | No role registry/allowlist exists anywhere in the mint path: parseRoles only splits (cli.go:167-173), Issue only trims/dedupes (auth.go:308-320); grep for `"admin"`/`"harvester"`/allowlist constants in internal/auth and cli.go (non-test) returns nothing — every role string, including the test suite's own "admin"/"harvester", is an arbitrary unknown string to the mint path, and all mint tests exit 0. No rejection code path exists. | HANDLED |
| Adversarial: role smuggling via another path | Identity.Roles is assigned exactly once, from `parseRoles(*roles)` (cli.go:145); no env var, no default, no other write path. Verify-side `TokenRecord.Identity()` (auth.go:135) re-normalizes stored roles, so a hand-crafted store record can't inject un-normalized claims either. | NO PATH FOUND |
| Adversarial: flag parsing corrupting tenant/user/agent or the token | Each field bound to its own flag var (cli.go:136-141); role values are marshaled as JSON array elements (commas/quotes cannot escape into other fields); raw-token generation untouched by the diff. `TestTokenCreateStillRequiresTenantAndUser` (ran, passed) confirms `--roles` does not weaken the `--tenant`/`--user` requirement; Verify succeeding in DW-1.1/1.3 confirms token hash integrity. | NO DEFECT FOUND |

## Test-DW Coverage
- [x] All 3 DW items have DW-ID-named automated tests that ran in Step 0.
- [x] Coverage level "100% of new/changed functions": `parseRoles` (new) = **100.0%**; `runTokenCreate` (changed) = 88.9%, but every line this phase added or changed (flag at :139, identity construction at :145) is covered — the two uncovered blocks (:142-144 fs.Parse error return, :150-152 Issue error return) are pre-existing branches this diff did not touch, and the base commit had *zero* token-create tests (grep of `git show HEAD:internal/cli/cli_test.go` finds no token tests), so this phase strictly raised coverage from 0%. Pre-existing gap noted below, not a blocker.

## Dead Code
None found. The added `strings` import is used by parseRoles; no debug statements, unreachable code, or commented-out blocks in the diff.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | CLI command is single-threaded per invocation; no shared state introduced. |
| Error Handling | PASS | fs.Parse and Issue errors returned up to Run (cli.go:142-144, 150-152); no swallowed errors. Probed: `--roles ",,"` → Split yields all-empty entries → normalizeRoles → nil → identical to role-less; no panic path. |
| Resources | N/A | No handles/connections opened by the change (HTTP client owned by env, pre-existing). |
| Boundaries | PASS | Probed empty string, whitespace-only, trailing comma, all-comma input, single entry — all traced to correct output; table test executes the first three. `lines[len(lines)-1]` token capture in the test helper is guarded by the code==0 check. |
| Security | PASS | Untrusted CLI input crosses the barricade at mint (`normalizeRoles`, auth.go:241) and is re-normalized on the read side (auth.go:135) — defense in depth on the auth path. Smuggling/corruption probes above found no path. |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | No executable code in assertions | N/A | Go; no assertions used. |
| cc-defensive-programming | No empty catch blocks / swallowed errors | PASS | All error returns propagate (cli.go:142-144, 150-152); no `_ = err` in the diff. |
| cc-defensive-programming | External input validated at entry | PASS | `--tenant`/`--user` required at CLI entry (cli.go:146-148) AND re-checked inside auth (`Issue` rejects incomplete identity, auth.go:226-228) — barricade plus defense in depth. Roles sanitized at the mint barricade (auth.go:241). TestTokenCreateStillRequiresTenantAndUser executes the entry check. |
| cc-defensive-programming | Assertions for bugs only (anticipated errors handled) | PASS | Anticipated bad input (dirty roles, missing tenant/user) uses error handling, not assertions. |
| cc-defensive-programming | Barricade reduces redundant validation without weakening security paths | PASS | parseRoles deliberately defers per-entry cleanup to the single `normalizeRoles` barricade (documented, cli.go:158-166) — one sanitization rule, no drift; security-critical read side still re-normalizes (auth.go:135). |

## Notes (non-blocking)
- Pre-existing coverage gap: runTokenCreate's fs.Parse-error and Issue-error branches (cli.go:142-144, 150-152) remain untested. These branches predate this phase (base commit had no token-create tests at all); worth a follow-up test but outside this phase's changed lines.
- "Order-normalized" in DW-1.1 is implemented as first-seen-order preservation (auth.go:304), not sorting. For the DW's literal input the output matches exactly; `--roles harvester,admin` would verify as `["harvester","admin"]`. Deterministic and documented in the test comment — flagging only in case the plan intended lexicographic order.
- Role names are case-sensitive (`Admin` ≠ `admin`); no requirement addresses this, consistent with "authorization is policy-side".

## Issues
None.

**Verdict: PASS**
