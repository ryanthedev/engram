# Review: Phase 2 - Role identity + authz core (sample 1)

## Executed Results (Step 0)
- Test suite (scoped): `go test ./internal/auth/... ./internal/knowledgeauth/... -v` → 34 passed, 0 failed (13 top-level tests + 18 subtests; all `TestDW_2_*` present and PASS)
- Full suite: `make test` → exit 0, all packages ok, no FAIL lines
- Lint: `make lint` (go vet + revive) → exit 0
- Build: `go build ./...` → success
- Coverage: `go test -coverprofile` → knowledgeauth **100.0%**; auth 91.4% overall, but every uncovered block (auth.go 184-186, 193-195, 230-232, 247-249, 258-260, 276-278 — store/entropy error paths) is **pre-existing code untouched by this phase's diff**; all phase-changed lines (Identity.Roles, TokenRecord.Roles, Identity(), Issue roles, normalizeRoles) are 100% covered. Profile at /tmp/p2-review-sample-1/cover.out.

## Requirement Fulfillment

### DW-2.1
PREMISE:  `auth.Identity` carries `Roles`, populated from verified token claims; a unit test proves client-supplied roles in a request are IGNORED (roles derive only from the verified token, never from request fields).
EVIDENCE: internal/auth/auth.go:44 (`Roles []string` on Identity), auth.go:134-136 (`TokenRecord.Identity()` populates Roles via `normalizeRoles(r.Roles)` from the stored record), auth.go:178-203 (`Verify` reads only the raw token + store record; never a request/context field). Tests: internal/auth/roles_test.go:13-30 (`TestDW_2_1_RolesFromVerifiedToken`), roles_test.go:37-76 (`TestDW_2_1_ClientSuppliedRolesIgnored`).
TRACE:    Token minted with Roles=["curator"]; client forges Identity{Roles:["admin"]} into the incoming ctx → `Verify(ctx, raw)` hashes raw → `GetByHash` → returns `rec.Identity()` = Roles=["curator"]; barricade overwrite of ctx then carries no "admin". Test asserts exactly this and passed.
VERDICT:  PASS

### DW-2.2
PREMISE:  `AuthorizeRead(id, public, requiredRoles)` allows any authenticated caller when public; allows a caller holding a required role; denies otherwise with the `ErrForbidden` sentinel.
EVIDENCE: internal/knowledgeauth/knowledgeauth.go:45-58. Test: internal/knowledgeauth/knowledgeauth_test.go:20-52 (`TestDW_2_2_AuthorizeRead`, 11 subtests, all passed).
TRACE:    (a) valid id + public=true → nil (line 49-51). (b) valid id Roles=["curator"], public=false, required=["admin","curator"] → `slices.Contains` hit → nil. (c) Roles=["reader"], required=["admin","curator"] → no match → `ErrForbidden` (line 57, bare sentinel).
VERDICT:  PASS

### DW-2.3
PREMISE:  `AuthorizeWrite(id, requiredRole)` denies a caller lacking the required role with `ErrForbidden`.
EVIDENCE: internal/knowledgeauth/knowledgeauth.go:65-71. Test: internal/knowledgeauth/knowledgeauth_test.go:57-84 (`TestDW_2_3_AuthorizeWrite`, 7 subtests, all passed).
TRACE:    id Roles=["reader","curator"], requiredRole="harvester" → Valid=true, required non-empty, `slices.Contains`=false → `ErrForbidden` (line 68, bare sentinel).
VERDICT:  PASS

**All requirements met:** YES

## Test-DW Coverage
- [x] All DW items have DW-named tests that ran in Step 0 (`TestDW_2_1_*` x2, `TestDW_2_2_AuthorizeRead`, `TestDW_2_3_AuthorizeWrite`)
- [x] Coverage matches the stated 100% level: knowledgeauth.go 100.0%; every line this phase added/changed in auth.go is covered (uncovered blocks are pre-existing error paths outside the phase diff)
- [x] Supporting tests: `TestIssueNormalizesRoles`, `TestTokenRecordIdentityNormalizesRoles`, `TestTokenRecordRolesJSONRoundTrip`, `TestErrForbiddenIsUnwrappedSentinel` — all passed

## Edge cases (prompt-listed)
| Edge case | Handling | Evidence |
|---|---|---|
| Empty roles → deny, not error | `slices.Contains(nil, ...)`=false → ErrForbidden | subtests "gated denies a caller with empty roles", "caller with empty roles is denied" — passed |
| Unknown role in policy → deny, not error | no match path returns ErrForbidden, never an error type | subtest "unknown role in policy denies, not errors" — passed |
| Public still requires authentication | `!id.Valid()` check (knowledgeauth.go:46) precedes the public check (line 49) | subtest "public still requires authentication" — passed |
| Admin/harvester write role missing → ErrForbidden | knowledgeauth.go:67-68 | subtests "caller lacking the harvester role is denied", "caller with empty roles is denied" (admin) — passed |
| ErrForbidden UNWRAPPED, errors.Is matches | all four denial returns are the bare sentinel; no fmt.Errorf in the package | `TestErrForbiddenIsUnwrappedSentinel` asserts with `!=` identity — passed |

## Dead Code
None found. `normalizeRoles` is used at both mint (auth.go:241) and read (auth.go:135) sites; all imports used; no debug statements or commented-out blocks in the phase diff.

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | Authorizer is stateless (zero-size struct); Identity is a value type; `normalizeRoles` never mutates its input and returns a fresh slice, so `Verify`-returned identities never alias the store record (proven by `TestTokenRecordIdentityNormalizesRoles` mutation check) |
| Error Handling | PASS | Every denial branch traced returns the bare `ErrForbidden` — misconfiguration (empty/blank policy) denies rather than errors; probed blank-role and nil-slice inputs, all fail closed |
| Resources | N/A | No files, connections, locks, or goroutines in the phase code |
| Boundaries | PASS | Probed nil roles, empty strings, whitespace-only roles on both claim and policy sides — `slices.Contains` on nil is safe, trimmed-empty policy entries are skipped (knowledgeauth.go:53), `normalizeRoles(nil)` → nil keeps omitempty (JSON round-trip test passed) |
| Security | PASS | Fail-closed traced on every branch: unauthenticated-even-if-public (line 46 before line 49), empty write role grants nobody (line 67), untrimmed claim vs trimmed policy misses exact match → deny (fail-closed direction); forged context roles cannot reach `Verify` output (roles_test.go:37-76); no oracle — single sentinel for all denial reasons |

## Loaded-Skill Criteria
N/A — no skills loaded (dispatch had no `## Additional Skills` block).

## Notes (non-blocking)
- Role matching is exact and case-sensitive by documented design (knowledgeauth.go:44); a case-mismatched role denies — fail-closed, so not a defect, but worth a mention if policies will be human-authored.
- Pre-existing uncovered error paths in auth.go (store-error and entropy-error branches, incl. the constant-time-mismatch defense at 193-195) predate this phase; not chased per scope.
- internal/store/templates/auth-tokens.json adds `"roles": {"type":"keyword"}` under `dynamic: strict`; the `omitempty` on TokenRecord.Roles keeps role-less records writable to un-reprovisioned indices — coherent design, verified by the JSON round-trip test.

## Issues
None.

**Verdict: PASS**
