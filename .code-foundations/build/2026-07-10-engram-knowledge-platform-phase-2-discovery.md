# Discovery + Design: Phase 2 - Role identity + authorization core

## Files Found
- `internal/auth/auth.go` — Identity (line 34), TokenRecord, TokenIssuer.Issue, Authenticator.Verify, ContextWithIdentity/IdentityFromContext.
- `internal/auth/auth_test.go` — internal-package tests over a `memStore` fake; DW-3.x named tests.
- `internal/store/auth.go` — OpenSearch `AuthTokenStore` (JSON round-trips `auth.TokenRecord`, so a new struct field flows through with no store-code change).
- `internal/store/templates/auth-tokens.json` — `dynamic: "strict"` mapping; needs a `roles` keyword field.
- `internal/store/apply.go` — Apply upserts templates; existing indices keep their old mapping (see Gaps).
- `internal/authgrpc/interceptor.go:102` — the barricade injects the *verified* Identity via `auth.ContextWithIdentity`, overwriting anything already in ctx. This is the real code path DW-2.1's "client-supplied roles ignored" test mirrors.
- `internal/knowledgeauth/` — does not exist (net-new package, as planned).

## Current State
Tokens are opaque 256-bit bearer strings; the ONLY persisted representation is a `TokenRecord`
(hash-keyed doc in `engram-auth-tokens-000001`). **The TokenRecord IS the server-side claim set** —
identity claims (tenant/user/agent) are read from it at Verify time, never from the client.

## Assumption Verification (plan uncertainty, MED confidence)
**"Tokens are minted/verified in a place a roles claim can be added" — VERIFIED TRUE.**
- Mint: `TokenIssuer.Issue(ctx, id, ttl)` (auth.go:208) builds the `TokenRecord` from the operator-supplied Identity. Adding `Roles` to Identity + TokenRecord makes issuance persist the claim.
- Verify: `Authenticator.Verify` (auth.go:161) returns `rec.Identity()` — roles ride the same verified read.
- Persistence: `store.AuthTokenStore` marshals/unmarshals the whole `TokenRecord` via encoding/json (store/auth.go:49,199), so no store-code change is needed; only the strict index template needs the `roles` field (explicitly in scope).
No UPDATE_PLAN needed.

## Gaps
1. **`Identity` becomes non-comparable.** Adding `Roles []string` breaks `got != testIdentity` at `internal/auth/auth_test.go:150` (the only `==`/`!=` on Identity in the repo; the one map use holds Identity as a *value*, which is fine). Fix in-scope: compare with `reflect.DeepEqual`.
2. **Existing-deployment mapping migration.** `Apply` (store/apply.go:63-75) upserts the index *template*, but an already-created `engram-auth-tokens-000001` keeps its old strict mapping — writing a roles-carrying record there would raise `strict_dynamic_mapping_exception`. Mitigation inside this phase: `json:"roles,omitempty"` means role-less tokens (all current ones) serialize no `roles` key, so existing deployments are unaffected until roles are actually used; fresh deployments inherit the updated template. A live `PUT mapping` migration belongs to the Phase 3 provisioning machinery / ops, and `apply.go` is outside this phase's file scope. Noted for the plan orchestrator.
3. **No operator surface mints role-carrying tokens yet.** `internal/cli` (`token create`, cli.go:131) has no `--roles` flag and is outside this phase's file scope; no later phase lists it either. The phase contract ("role-carrying tokens") is met via the Go API (`Issue` with `Identity.Roles` set), which Phase 6 wiring can use — but the CLI flag is a one-line follow-up the plan should pick up. Not absorbing it silently.

## Code Standards
Read `docs/code-standards.md`. Load-bearing for this phase:
- Sentinels are exported, checked with `errors.Is`, and returned **unwrapped** so they survive to the gRPC edge (§3) — `ErrForbidden` follows `acl.ErrScopeDenied`'s pattern.
- Never trust client-supplied tenancy/scope; the verified Identity is authoritative (§1) — extends verbatim to roles.
- stdlib `testing` only, table tests with named cases, tests named after the DW they pin (§5).
- Package layout: one bounded concern per package; seam + types in `<concern>.go` (§7).
- Fail-closed at trust boundaries (§3).

## Test Infrastructure
- `internal/auth/auth_test.go`: internal test package with `memStore` fake + fixed-clock injection — reused for the roles tests.
- knowledgeauth is pure (no I/O): plain external-package table tests, no fake needed.
- Unit only; no integration tag required for this phase (store/auth.go is untouched).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-2.1 | `auth.Identity` carries `Roles`, populated from verified token claims; unit test proves client-supplied roles are ignored | COVERED | `TestDW_2_1_RolesFromVerifiedToken` (clean), `TestDW_2_1_ClientSuppliedRolesIgnored` (dirty — forged ctx identity + interceptor-mirror overwrite) in `internal/auth/roles_test.go` |
| DW-2.2 | `AuthorizeRead` allows public, allows a required-role holder, denies otherwise with `ErrForbidden` | COVERED | `TestDW_2_2_AuthorizeRead` table in `internal/knowledgeauth/knowledgeauth_test.go`: public+any-authenticated allow, role-holder allow, no-role deny, empty-roles deny, empty-policy deny, unknown-policy-role deny-not-error, unauthenticated-public deny |
| DW-2.3 | `AuthorizeWrite` denies a caller lacking the required role with `ErrForbidden` | COVERED | `TestDW_2_3_AuthorizeWrite` table: role-holder allow, lacking-role deny, empty-roles deny, empty-requiredRole fails closed, unauthenticated deny |

**All items COVERED:** YES (3/3 — matches the dispatch prompt's 3 DW-IDs)

## Design Decisions

### Design: knowledgeauth authorizer (aposd-designing-deep-modules, design-it-twice)

#### Approaches Considered
1. **A — package-level functions**: `knowledgeauth.AuthorizeRead(...)` / `AuthorizeWrite(...)` as free funcs + `ErrForbidden`. Zero state, smallest surface.
2. **B — zero-value `Authorizer` struct** with the two methods. Same purity, but gives Phase 6 an injectable *value* (a `Server`/`Backend` field), leaves room for future policy state (audit hook, role hierarchy) without call-site churn, and matches the plan's Produces line literally: `knowledgeauth.Authorizer{AuthorizeRead, AuthorizeWrite}`.
3. **C — consumer-defined interface in Phase 6 + impl here**: violates the house "seams are consumer-defined" placement (the consumer doesn't exist yet) and adds ceremony for two pure functions.

#### Comparison
| Criterion | A | B | C |
|-----------|---|---|---|
| Interface simplicity | 2 funcs | 2 methods on a zero-value type | 2 methods + interface decl |
| Information hiding | equal | equal (decision logic hidden either way) | equal |
| Caller ease of use | call directly | `var az knowledgeauth.Authorizer` — injectable, mockable | must define seam prematurely |
| Matches frozen plan contract | partially (funcs, not `Authorizer{...}`) | **exactly** | no |
| Future policy state | needs signature change | absorbed by the receiver | absorbed |

#### Choice: B
Rationale: identical depth to A, literal match to the plan's frozen `Produces` contract, and Phase 6 gets an injectable dependency. Sacrifice: one (empty) type of ceremony.

#### Depth Check
- Interface methods: 2 (+1 sentinel var)
- Hidden details: role-match semantics (exact, case-sensitive, empty-string roles grant nothing), fail-closed ordering (authn before public before roles), deny reason collapse (one sentinel — no oracle)
- Common case complexity: simple — `if err := az.AuthorizeRead(id, spec.Public, spec.Roles); err != nil { ... }`

### Authorization semantics (cc-defensive-programming: fail closed at every branch)
- `AuthorizeRead(id, public, requiredRoles)`: (1) `!id.Valid()` → `ErrForbidden` — a public collection **still requires authentication**; (2) `public` → allow; (3) allow iff some non-empty required role ∈ `id.Roles`; (4) else `ErrForbidden`. Corollaries: empty `requiredRoles` + `public=false` → deny; a policy naming a role nobody holds (unknown role) → deny, **not** an error.
- `AuthorizeWrite(id, requiredRole)`: `!id.Valid()` → deny; `requiredRole == ""` → deny (a misconfigured empty policy must never grant everyone write); allow iff `requiredRole ∈ id.Roles`.
- `ErrForbidden` = `errors.New("knowledgeauth: forbidden")`, always returned **unwrapped** (code-standards §3) so `errors.Is` survives to the Phase 6 gRPC edge; a single sentinel gives the caller no role-oracle.
- Matching is exact + case-sensitive; empty-string roles are inert on both sides.

### Roles-as-claims in internal/auth
- `Identity.Roles []string` + `TokenRecord.Roles []string` `json:"roles,omitempty"` (snake_case store field per §6; omitempty keeps old strict indices writable — Gap 2).
- **Barricade both halves**: `Issue` normalizes roles at the write half (trim, drop empties, dedupe, preserve order); `TokenRecord.Identity()` re-normalizes at the read half (defense-in-depth — security-critical path, records may be written by other means). One shared `normalizeRoles` helper.
- Client-supplied roles are structurally unreachable: `Verify` takes only the raw token and reads roles from the stored record; the authgrpc barricade then *overwrites* any ctx identity (interceptor.go:102). The DW-2.1 dirty test mirrors that exact flow.
- `Identity()` returns a cloned slice so no caller aliases the record's backing array.
- Template: add `"roles": { "type": "keyword" }` to `auth-tokens.json`.

## Prerequisites
- [x] Phase 1 merged (Provenance.roles proto field exists; not needed by this phase)
- [x] `internal/auth` + store + template exist as the plan assumes
- [x] Go 1.25 (`slices` stdlib available)
- [x] No missing dependencies — pure unit phase, no OpenSearch needed

## Recommendation
**BUILD.** Plan fits reality; the roles claim slots into the existing TokenRecord claim set exactly as assumed. Three noted gaps: the auth_test.go:150 comparability fix (in scope), the existing-index mapping migration (deferred to Phase 3 machinery/ops — flagged), and the missing CLI `--roles` operator flag (flagged for the orchestrator, not absorbed).
