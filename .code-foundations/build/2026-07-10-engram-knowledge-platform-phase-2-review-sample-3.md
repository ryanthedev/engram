# Review: Phase 2 - Role identity + authz core (sample 3)

## Executed Results (Step 0)
- Build: `go build ./...` → success
- Test suite: `go test ./internal/auth/... ./internal/knowledgeauth/... -v -coverprofile` → 34 tests, all PASS, 0 failures (raw log: /tmp/p2-review-sample-3/test-verbose.txt)
- Full suite: `make test` → all packages `ok`, no failures
- Lint: `make lint` (go vet + revive) → exit 0, no findings
- Coverage: internal/knowledgeauth **100.0%** of statements; internal/auth 91.4% overall, but every uncovered block (auth.go 184, 193, 230, 247, 258, 276 — store-error/entropy-error branches) pre-dates this phase: `git diff HEAD -- internal/auth/auth.go` shows the phase added no error branches. **All Phase-2-added code is 100% covered** (`normalizeRoles` 100%, `TokenRecord.Identity` 100%, `Valid` 100%, all of knowledgeauth 100%).

## Requirement Fulfillment

### DW-2.1
PREMISE:  "`auth.Identity` carries `Roles`, populated from verified token claims; a unit test proves client-supplied roles in a request are IGNORED (roles derive only from the verified token, never from request fields)."
EVIDENCE: internal/auth/auth.go:44 (`Roles []string` on Identity), auth.go:121 (persisted `TokenRecord.Roles`), auth.go:134-136 (`Identity()` builds Roles from the stored record via `normalizeRoles`), auth.go:202 (Verify returns `rec.Identity()`); tests internal/auth/roles_test.go:13-30, 37-76.
TRACE:    Issue(id{Roles:["curator"]}) → record stored with normalized roles → Verify(raw) hashes token, loads record by hash, returns `rec.Identity()` — the ONLY input to Verify is the raw token string, so no request field can reach Roles. In `TestDW_2_1_ClientSuppliedRolesIgnored` a forged `Identity{Roles:["admin"]}` is planted in the incoming context; Verify still returns `["curator"]`, and after barricade overwrite `IdentityFromContext` carries no "admin".
VERDICT:  **PASS** — `TestDW_2_1_RolesFromVerifiedToken` and `TestDW_2_1_ClientSuppliedRolesIgnored` both ran and passed (Step 0 log).

### DW-2.2
PREMISE:  "`AuthorizeRead(id, public, requiredRoles)` allows any authenticated caller when public; allows a caller holding a required role; denies otherwise with the `ErrForbidden` sentinel."
EVIDENCE: internal/knowledgeauth/knowledgeauth.go:45-58; tests internal/knowledgeauth/knowledgeauth_test.go:20-52.
TRACE:    (valid id, public=true, any policy) → `!id.Valid()` false → `public` → nil (allow). (valid id{Roles:["curator"]}, public=false, ["admin","curator"]) → loop trims "admin" (no match), "curator" → `slices.Contains` true → nil. (valid id{Roles:["reader"]}, public=false, ["admin","curator"]) → no match → `ErrForbidden`.
VERDICT:  **PASS** — 11/11 `TestDW_2_2_AuthorizeRead` subtests passed, each denial asserted via `errors.Is(err, ErrForbidden)`.

### DW-2.3
PREMISE:  "`AuthorizeWrite(id, requiredRole)` denies a caller lacking the required role with `ErrForbidden`."
EVIDENCE: internal/knowledgeauth/knowledgeauth.go:65-71; tests internal/knowledgeauth/knowledgeauth_test.go:57-84.
TRACE:    (id{Roles:["reader","curator"]}, "harvester") → Valid true, requiredRole non-empty, `slices.Contains(["reader","curator"],"harvester")` false → `ErrForbidden`. (id{Roles:["reader","admin"]}, "admin") → Contains true → nil.
VERDICT:  **PASS** — 7/7 `TestDW_2_3_AuthorizeWrite` subtests passed.

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-2.1 → `TestDW_2_1_RolesFromVerifiedToken`, `TestDW_2_1_ClientSuppliedRolesIgnored` (ran, passed)
- [x] DW-2.2 → `TestDW_2_2_AuthorizeRead` (11 subtests, ran, passed)
- [x] DW-2.3 → `TestDW_2_3_AuthorizeWrite` (7 subtests, ran, passed)
- [x] Coverage matches the stated 100% level for phase code: knowledgeauth 100.0% of statements; all Phase-2-added auth.go statements covered (uncovered blocks verified pre-existing via git diff).
- Supporting tests also ran: `TestIssueNormalizesRoles`, `TestTokenRecordIdentityNormalizesRoles` (no aliasing of record's backing array), `TestTokenRecordRolesJSONRoundTrip` (omitempty keeps pre-roles strict indices writable), `TestErrForbiddenIsUnwrappedSentinel`.

## Edge Cases (prompt-listed)
| Edge case | Handling | Evidence |
|---|---|---|
| Empty roles → deny, not error | `caller()` (nil Roles) gated → loop matches nothing → `ErrForbidden` | subtest "gated denies a caller with empty roles" PASS; also "gated with empty policy denies everyone (fail closed)" |
| Unknown role in policy → deny, not error | policy ["no-such-role"] → no claim matches → `ErrForbidden`, never a distinct error | subtest "unknown role in policy denies, not errors" PASS |
| Public still requires authentication | knowledgeauth.go:46 checks `!id.Valid()` BEFORE the `public` short-circuit | subtest "public still requires authentication" (zero Identity, public=true → deny) PASS; also "gated denies unauthenticated caller even with matching roles" |
| Admin/harvester write role missing → ErrForbidden | knowledgeauth.go:67 `!slices.Contains(id.Roles, requiredRole)` → `ErrForbidden` | subtests "caller lacking the harvester role is denied", "caller with empty roles is denied" (required "admin") PASS |
| ErrForbidden UNWRAPPED | Both funcs `return ErrForbidden` bare (knowledgeauth.go:47,57,68) — no fmt.Errorf on the return path | `TestErrForbiddenIsUnwrappedSentinel` asserts pointer equality `err != ErrForbidden` fails the test — PASS |

## Dead Code
None found blocking. See Notes for one stale comment name.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `Authorizer` is stateless (zero-size struct); both methods are pure functions of their args. `normalizeRoles` returns a fresh slice — `TestTokenRecordIdentityNormalizesRoles` proves mutating `Identity().Roles` cannot corrupt the shared record. No shared mutable state introduced. |
| Error Handling | PASS | Probed: store error in Verify wraps with %w (auth.go:185) — a store failure is an error, never an allow. All authz denials collapse to one sentinel (no role oracle). Adversarial trace: unknown role / empty policy / blank strings all reach `ErrForbidden`, never nil and never a different error. |
| Resources | N/A | No I/O, handles, locks, or caches in the authz path; TokenStore is a seam owned by callers. |
| Boundaries | PASS | Probed nil Roles, empty policy slice, empty-string and whitespace-only roles on BOTH sides: trimmed `want` must be non-empty (knowledgeauth.go:53), trimmed `requiredRole` empty → deny (66-67); claim `""` can only match policy `""` which is excluded. Untrimmed claim " admin" vs policy "admin" → deny (fail-closed direction, and Verify-produced claims are normalized at auth.go:135/241 anyway). |
| Security | PASS | Fail-closed probes: (1) forged context identity cannot influence Verify — `TestDW_2_1_ClientSuppliedRolesIgnored`; (2) `!id.Valid()` gate precedes the `public` allow, so unauthenticated-public is denied; (3) misconfigured empty write role grants nobody (traced: `requiredRole == ""` → ErrForbidden even for a caller claiming ""); (4) private ctx key type prevents cross-package forgery of the context identity; (5) full-digest constant-time compare retained in Verify (auth.go:193). |

## Loaded-Skill Criteria
N/A — no skills loaded (dispatch had no `## Additional Skills` block).

## Notes (non-blocking)
- internal/auth/auth_test.go:65-66: doc comment on `records()` starts with "rawOf returns..." — stale name from an earlier revision. Cosmetic.
- `Identity.Valid()` (auth.go:50-52) accepts whitespace-only TenantID/UserID ("` `" != ""). Not attacker-reachable in this phase — identities enter only via Issue (operator-side) and the verified-token path — so this is a hardening suggestion, not a demonstrated defect.
- `internal/knowledgeauth` has no production caller yet; per the package doc the enforcement site is Phase 6. Expected for this phase, not dead code.
- internal/store/templates/auth-tokens.json:17 adds `"roles": {"type": "keyword"}` to the strict mapping, matching the record's `roles` JSON key; `TestTokenRecordRolesJSONRoundTrip` proves omitempty keeps role-less records compatible with pre-roles strict indices. Integration-tagged store tests were not run per dispatch instructions (no mutating commands).

## Issues
None.

**Verdict: PASS**
