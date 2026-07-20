# Review: Phase 1 - token create --roles (role-bearing token minting)

## Executed Results (Step 0)
- Test suite: `go test -count=1 ./internal/cli/ ./internal/auth/` → both `ok`, 0 failures. Verbose run of `./internal/cli/` shows every test PASS, including `TestDW_1_1_TokenCreateRolesFlagVerifies`, `TestDW_1_2_TokenCreateNoRolesFlagIsRoleless`, `TestDW_1_3_TokenCreateRolesNormalization` (5 subtests), `TestTokenCreateStillRequiresTenantAndUser`, `TestUsageDocumentsRolesFlag`.
- Build: `go build ./...` → clean.
- Lint: `make lint` (go vet + revive) → exit 0.

## Requirement Fulfillment

### DW-1.1
PREMISE:  `token create ... --roles admin,harvester` mints a token; verifying it yields `Roles == ["admin","harvester"]` (order-normalized).
EVIDENCE: internal/cli/cli.go:139,145,167-173; internal/auth/auth.go:241,308-320; test internal/cli/cli_test.go:276-290
TRACE:    `--roles "admin,harvester"` → `parseRoles` → `["admin","harvester"]` → `Identity.Roles` → `Issue` → `normalizeRoles` (trim/dedupe/first-seen order) persisted in `TokenRecord.Roles` → `Authenticator.Verify` → `TokenRecord.Identity()` re-normalizes → `["admin","harvester"]`.
VERDICT:  PASS — `TestDW_1_1_TokenCreateRolesFlagVerifies` verifies through the real mint+verify round trip against a fake OpenSearch store, asserting `reflect.DeepEqual(got, []string{"admin","harvester"})`; ran and passed.

### DW-1.2
PREMISE:  `token create` with no `--roles` mints a role-less token exactly as before (regression).
EVIDENCE: internal/cli/cli.go:145,167-171; diff shows the pre-change literal was `auth.Identity{TenantID, UserID, AgentID}` (Roles zero-value nil)
TRACE:    omitted flag → `*roles == ""` → `parseRoles("")` → `TrimSpace` → `""` → returns `nil` → `Identity.Roles = nil`, byte-identical to the previous struct literal → `normalizeRoles(nil)` → `nil` → record marshals with no `roles` key (`json:"roles,omitempty"`, auth.go:121) → verified `Roles` empty.
VERDICT:  PASS — `TestDW_1_2_TokenCreateNoRolesFlagIsRoleless` mints without the flag and asserts verified `len(Roles) == 0`; ran and passed.

### DW-1.3
PREMISE:  dirty input `--roles " admin , , harvester "` normalizes to `["admin","harvester"]` (no empty/whitespace entries) — table test.
EVIDENCE: internal/cli/cli_test.go:310-336 (table test, 5 cases); normalization at internal/auth/auth.go:308-320
TRACE:    `" admin , , harvester "` → `parseRoles`: TrimSpace → `"admin , , harvester"` → Split → `["admin ", " ", " harvester"]` → `Issue` → `normalizeRoles`: per-entry trim → `"admin"`, `""` (dropped), `"harvester"` → `["admin","harvester"]` → verified round-trip equals expected.
VERDICT:  PASS — `TestDW_1_3_TokenCreateRolesNormalization` is a table test (padded+empty segments, duplicates, whitespace-only, trailing comma, single role); all 5 subtests ran and passed.

**All requirements met:** YES

## Edge cases (prompt-listed)
| Edge case | Status | Evidence |
|---|---|---|
| empty/whitespace-only `--roles` → no roles | HANDLED | `parseRoles` returns `nil` for `""`/whitespace (cli.go:168-171); subtest `whitespace-only_value` (want `nil`) and `TestDW_1_2` both passed |
| duplicate/whitespace-padded role names normalized | HANDLED | `normalizeRoles` (auth.go:308-320) trims, drops empties, dedupes; subtests `duplicate_roles`, `padded_with_empty_segments`, `trailing_comma` passed |
| unknown role strings accepted at mint | HANDLED | No role allowlist/registry exists anywhere in `internal/` (grep for allowedRoles/knownRoles/validRoles/ErrUnknownRole: no hits). `Issue` (auth.go:225-251) validates only `id.Valid()` (tenant+user non-empty) and normalizes shape, never membership — the passing DW tests mint arbitrary strings ("admin","harvester") with no registry backing them; mint cannot reject an unknown role by construction |

**Adversarial checks:**
- *Role smuggling by any other path:* `Identity.Roles` is set exclusively from `parseRoles(*roles)` (cli.go:145). `env` feeds only the OpenSearch URL (cli.go:128); no `ENGRAM_ROLES` fallback exists. Go `flag` values are single argv entries, so a tenant/user value containing `--roles admin` stays an opaque string in that field — traced: it cannot re-enter flag parsing. No other write path to `Roles` in the mint flow.
- *Identity/token corruption via flag parsing:* tenant/user/agent come from distinct flags untouched by `parseRoles`; the raw token is generated from `crypto/rand` (auth.go:274-280) independent of roles; roles live only in the stored record, not in the token string. `TestTokenCreateStillRequiresTenantAndUser` confirms `--roles` does not weaken the `--tenant`/`--user` requirement (passed). `parseRoles(",")` → `["",""]` → `normalizeRoles` → `nil` (traced; benign).

## Test-DW Coverage
- [x] All DW items have corresponding tests, ran in Step 0 (test names carry DW IDs: `TestDW_1_1_...`, `TestDW_1_2_...`, `TestDW_1_3_...`)
- [x] Coverage level "100% of new/changed functions" met: `parseRoles` (new) — exercised by all three DW tests; `runTokenCreate` (changed) — DW tests + `TestTokenCreateStillRequiresTenantAndUser`; `usage` banner (changed) — `TestUsageDocumentsRolesFlag`
- Tests verify through the real `auth.Authenticator.Verify` path against a fake OpenSearch backend, not by inspecting the parsed flag — the assertion lands on the actual auth-layer result.

## Dead Code
None found. The new `strings` import is used by `parseRoles`; the diff (+23 lines in cli.go) contains no debug statements, commented-out blocks, or unreachable code.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | N/A | CLI mint path is single-threaded per invocation; no shared state introduced |
| Error Handling | PASS | `fs.Parse` error returned (cli.go:142-144); `Issue` error returned (cli.go:149-152); traced dirty inputs (`,`, whitespace-only, trailing comma) all yield defined results, never panic |
| Resources | N/A | No new handles/connections; reuses existing `http.Client{Timeout}` issuer construction |
| Boundaries | PASS | Empty string, whitespace-only, `","`, trailing comma, duplicates all traced through `parseRoles`+`normalizeRoles` to correct results; whitespace-only and trailing-comma cases executed in the table test |
| Security | PASS | Roles sourced only from the explicit flag; normalization at mint (auth.go:241) AND re-normalization at verify (auth.go:135) — adversarial store-tamper path still normalized before authorization |

## Loaded-Skill Criteria
| Skill | Criterion | Status | Evidence |
|-------|-----------|--------|----------|
| cc-defensive-programming | External input validated at entry | PASS | CLI flags (external) hit the barricade: tenant/user required via `id.Valid()`; roles shape-normalized before persistence; parse errors rejected |
| cc-defensive-programming | No empty catch blocks / swallowed errors | PASS | Every error in the changed path is returned and surfaces as a non-zero exit through `cli.Run` |
| cc-defensive-programming | No executable code in assertions / assertions for bugs only | N/A | No assertions in the changed Go code; runtime conditions use error handling |
| cc-defensive-programming | Security-critical path gets defense-in-depth (barricade does not replace re-validation) | PASS | Auth path normalizes twice: mint-time (`Issue`, auth.go:241) and read-time (`TokenRecord.Identity()`, auth.go:135) — records written by any other means get the same treatment before authorization |
| cc-defensive-programming | Barricade reduces redundant validation (no drift-prone duplication) | PASS | `parseRoles` deliberately does splitting only; per-entry cleanup lives once in `normalizeRoles`, documented at cli.go:158-166; end-to-end tests prove the barricade actually fires |

## Notes (non-blocking)
- `parseRoles` returns un-trimmed segments (e.g. `["admin ", " "]`) by design, relying on `Issue` to normalize. Correct in every executed path (and Verify re-normalizes), but a future caller printing or using `Identity.Roles` before `Issue` would see raw segments. The comment documents the contract; no demonstrated defect.
- `runTokenCreate` ignores trailing positional args after flags (`fs.Args()` unchecked) — pre-existing behavior shared by all subcommands, not introduced by this change, and traced to have no effect on roles or identity.
- A role name with interior whitespace (e.g. `"ad min"`) survives normalization as a single role — consistent with "unknown role strings accepted at mint; authorization is policy-side."

## Issues (if FAIL)
None.

**Verdict: PASS**
