# Review: Phase 2 - Role identity + authz core (sample 2)

## Executed Results (Step 0)
- Build: `go build ./...` → success (exit 0)
- Unit suites: `go test ./internal/auth/... ./internal/knowledgeauth/... -v` → 34 passed, 0 failed (auth: 13, knowledgeauth: 21 incl. subtests)
- Full suite: `make test` → all packages ok (exit 0)
- Lint: `make lint` (go vet + revive) → clean (exit 0)
- Coverage: `go test -coverprofile` → knowledgeauth **100.0%** total; auth 91.4% total but every phase-2 function at 100% (`Identity()` auth.go:134, `normalizeRoles` auth.go:308, `ContextWithIdentity`, `IdentityFromContext`); sub-100% functions (Verify 86.7%, Issue 84.6%, Revoke 83.3%, generateRawToken 75%) are pre-existing store-error/entropy-error paths untouched by this phase (git diff --stat confirms scope).

## Requirement Fulfillment

### DW-2.1
PREMISE:  `auth.Identity` carries `Roles`, populated from verified token claims; a unit test proves client-supplied roles in a request are IGNORED (roles derive only from the verified token, never from request fields).
EVIDENCE: internal/auth/auth.go:44 (`Roles []string` on Identity), auth.go:134-136 (`TokenRecord.Identity()` builds Roles via `normalizeRoles(r.Roles)` — record only), auth.go:178-203 (`Verify` returns `rec.Identity()`; its only inputs are the raw token string and the store record — no request-field path exists); test internal/auth/roles_test.go:37-76.
TRACE:    Token issued with bound roles `["curator"]` → client forges `Identity{Roles:["admin"]}` into the request ctx via `ContextWithIdentity` → `Verify(ctx, raw)` hashes raw, loads the stored record, returns `rec.Identity()` = Roles `["curator"]`; ctx value is never read on the Verify path → forged "admin" absent from verified identity; barricade overwrite leaves `["curator"]`.
VERDICT:  PASS — `TestDW_2_1_ClientSuppliedRolesIgnored` and `TestDW_2_1_RolesFromVerifiedToken` both PASS (executed, raw log /tmp/p2-review-sample-2/dw2-raw.log).

### DW-2.2
PREMISE:  `AuthorizeRead(id, public, requiredRoles)` allows any authenticated caller when public; allows a caller holding a required role; denies otherwise with the `ErrForbidden` sentinel.
EVIDENCE: internal/knowledgeauth/knowledgeauth.go:45-58; test internal/knowledgeauth/knowledgeauth_test.go:20-52 (11-case table).
TRACE:    (a) valid id, public=true → `id.Valid()` true, `public` true → nil (allow). (b) valid id with Roles ["curator"], public=false, required ["admin","curator"] → loop hits "curator", `slices.Contains` true → nil. (c) valid id with Roles ["reader"], required ["admin","curator"] → no match → return `ErrForbidden` (line 57, unwrapped).
VERDICT:  PASS — `TestDW_2_2_AuthorizeRead` PASS with all 11 subtests (executed).

### DW-2.3
PREMISE:  `AuthorizeWrite(id, requiredRole)` denies a caller lacking the required role with `ErrForbidden`.
EVIDENCE: internal/knowledgeauth/knowledgeauth.go:65-71; test internal/knowledgeauth/knowledgeauth_test.go:57-84 (7-case table).
TRACE:    id Roles ["reader","curator"], requiredRole "harvester" → `id.Valid()` true, requiredRole non-empty, `slices.Contains(["reader","curator"], "harvester")` false → return `ErrForbidden` (line 68, unwrapped).
VERDICT:  PASS — `TestDW_2_3_AuthorizeWrite` PASS (executed).

**All requirements met:** YES

## Test-DW Coverage
- [x] DW-2.1 → `TestDW_2_1_RolesFromVerifiedToken`, `TestDW_2_1_ClientSuppliedRolesIgnored` (ran, PASS)
- [x] DW-2.2 → `TestDW_2_2_AuthorizeRead` (ran, PASS)
- [x] DW-2.3 → `TestDW_2_3_AuthorizeWrite` (ran, PASS)
- [x] Coverage matches stated 100% level: knowledgeauth package 100.0% statements; all phase-2 additions in auth 100%.
- Supporting: `TestErrForbiddenIsUnwrappedSentinel`, `TestIssueNormalizesRoles`, `TestTokenRecordIdentityNormalizesRoles`, `TestTokenRecordRolesJSONRoundTrip` all PASS.

## Edge Cases (prompt-listed)
| Edge case | Evidence | Result |
|---|---|---|
| Empty roles → deny, not error | knowledgeauth_test.go:33,68 — `caller()` denied with ErrForbidden via errors.Is | HANDLED |
| Unknown role in policy → deny, not error | knowledgeauth_test.go:35 — `no-such-role` policy denies with ErrForbidden; code path returns sentinel, never errors out | HANDLED |
| Public still requires authentication | knowledgeauth.go:46-48 checks `id.Valid()` BEFORE the public short-circuit; knowledgeauth_test.go:36 (`auth.Identity{}`, public=true → ErrForbidden) | HANDLED |
| Missing admin/harvester write role → ErrForbidden | knowledgeauth_test.go:67 (lacking harvester), :68 (empty roles vs admin) | HANDLED |
| ErrForbidden UNWRAPPED so errors.Is matches | knowledgeauth.go:57,68 return the sentinel directly; `TestErrForbiddenIsUnwrappedSentinel` asserts pointer equality (`err != ErrForbidden` fails the test), PASS | HANDLED |

## Dead Code
None found (no unused imports, no unreachable code, no debug statements, no commented-out blocks in the six reviewed files).

## Correctness Dimensions
| Dimension | Status | Evidence |
|-----------|--------|----------|
| Concurrency | PASS | `Authorizer` is stateless (zero-struct); `Identity` passed by value; `normalizeRoles` clones — `TestTokenRecordIdentityNormalizesRoles` proves no aliasing of the record's backing array. No shared mutable state introduced. |
| Error Handling | PASS | Probed the misconfiguration paths: nil/empty `requiredRoles` policy → deny (loop body never grants); empty `requiredRole` → trimmed then denied at knowledgeauth.go:67. All denials collapse to one sentinel — no role oracle. |
| Resources | N/A | Pure in-memory decision functions; no handles, connections, or goroutines. |
| Boundaries | PASS | Adversarial traces: nil `id.Roles` → `slices.Contains` false → deny; `""` in both claim and policy → policy entry trimmed to "" and skipped before Contains (knowledgeauth.go:53), so blank-matches-blank cannot grant — test cases :38-:39 confirm; whitespace-padded requiredRole trimmed (:66). Untrimmed store-side role (" admin") is normalized by `TokenRecord.Identity()` before reaching authz. |
| Security | PASS | Fail-closed order verified: `id.Valid()` precedes the `public` grant, so an unauthenticated caller is denied even for public. Role smuggling probed: `Verify`'s claims come solely from the stored record (auth.go:202); forged ctx identity demonstrated inert (`TestDW_2_1_ClientSuppliedRolesIgnored`). Claims sanitized at mint (auth.go:241) AND at read (auth.go:135) — defense in depth. |

## Loaded-Skill Criteria
N/A — no skills loaded (dispatch had no `## Additional Skills` block).

## Notes (non-blocking)
- internal/auth/auth_test.go:65 — comment on `records()` says "rawOf returns…", a stale name from an earlier draft. Cosmetic only.
- auth-tokens.json adds `roles` as keyword under `dynamic: strict`; a role-carrying record written to an old, pre-roles strict index would be rejected until that index is re-provisioned. The `omitempty` on `TokenRecord.Roles` (auth.go:121) keeps role-less records compatible, and the code comment acknowledges the re-provisioning need — operational note, not a Phase-2 defect.
- DW-2.1's "client-supplied roles ignored" test simulates the transport barricade in-package (forged ctx identity + Verify). The structural guarantee — `Verify` has no request-field input other than the opaque token — makes this sound; end-to-end interceptor enforcement is Phase 6 scope.

## Issues
None.

**Verdict: PASS**
