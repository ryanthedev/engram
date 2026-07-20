# Discovery + Design: Phase 1 - Role-bearing token minting

## Files Found
- `internal/cli/cli.go` (547 lines) — `runTokenCreate` at lines 132-154 mints a role-less token today.
- `internal/cli/cli_test.go` (174 lines) — external test package `cli_test`; existing tests cover `ingest` advisory behavior and `help` text only. No `token create` tests exist yet.
- `internal/auth/auth.go` (320 lines) — **out of file scope, already implements everything downstream of the CLI**: `Identity.Roles` field (line 44), `TokenIssuer.Issue` already sets `Roles: normalizeRoles(id.Roles)` at line 241, `normalizeRoles` (line 308) trims whitespace, drops empties, dedupes preserving first-seen order, returns `nil` for an effectively-empty set. `TokenRecord.Identity()` re-normalizes on read (defense-in-depth). This was landed in a prior commit (`7a714ee feat(auth): role dimension on identity + knowledge authorizer`), not part of this build.
- `internal/auth/roles_test.go` — proves `normalizeRoles` behavior end-to-end (`TestIssueNormalizesRoles`, `TestTokenRecordIdentityNormalizesRoles`) and the DW-2.1 barricade (client-supplied roles never leak in). Confirms normalization is already fully verified at the auth layer.
- `internal/store/auth.go` — `AuthTokenStore` (OpenSearch-backed `auth.TokenStore`): `Put` does `PUT {baseURL}/{index}/_doc/{hash}?refresh=true`, `GetByHash` does `GET {baseURL}/{index}/_doc/{hash}`. No mocking helper currently exported for CLI-level tests.
- `internal/store/opensearch_test.go` — shows the project's convention for a minimal in-package `httptest.Server`-backed fake OpenSearch cluster (`fakeOS`) used to unit-test store-layer code without a live cluster.

## Current State
`runTokenCreate` (cli.go:132) parses `--tenant`, `--user`, `--agent`, `--ttl`, `--url` via `flag.FlagSet`, builds `auth.Identity{TenantID, UserID, AgentID}` (no `Roles`), validates with `id.Valid()`, and calls `tokenIssuer(env, *url).Issue(ctx, id, *ttl)`. There is no `--roles` flag, so every minted token today has `Roles == nil` regardless of what the caller might want. `auth.Identity.Roles` and the full mint→normalize→persist→verify chain are already implemented and tested at the `internal/auth` layer — this phase only needs to plumb a new flag into the `Identity` literal.

## Gaps
- No `--roles` flag on `token create`.
- No CLI-level test harness exists to drive `token create` end-to-end (mint via a fake OpenSearch backend, then verify via `auth.Authenticator`) — needs a small `httptest`-backed fake implementing just the two auth-token endpoints (`PUT/GET .../_doc/{hash}`) that `AuthTokenStore.Put`/`GetByHash` use. `token create` never calls `_search`, so the fake does not need that endpoint (unlike the more general `fakeOS` in `internal/store/opensearch_test.go`, which also serves `token list`'s `_search`).
- Usage banner (`const usage`, cli.go:74-105) does not document a `--roles` flag on `token create`.

## Code Standards
`docs/code-standards.md` conventions applicable here: raw `net/http` to OpenSearch (no client library) — the existing `store.NewAuthTokenStore` pattern is reused as-is; doc comments cite `DW-N.N` where a decision maps to a plan item (already the norm in `auth.go`); table-driven tests are the house style (`internal/auth/roles_test.go`, `internal/store/opensearch_test.go`).

## Test Infrastructure
`internal/cli/cli_test.go` is `package cli_test` (external), drives everything through `cli.Run(ctx, args, env, out, errW)` — never calls unexported `cli` internals directly. Existing pattern: spin up a real in-process server (there: gRPC via `net.Listen` + `grpc.NewServer`; here: `httptest.NewServer` fronting a tiny fake OpenSearch), pass its address via a CLI flag (`-addr` there, `--url` here), then assert on captured stdout/stderr/exit code. `noopEnv` is the existing env stub (`func(string) string { return "" }`) — tests pass all flags explicitly rather than relying on env fallback, so `--url` must point at the fake server per-test.

To verify DW-1.1/1.2/1.3 (the *verified* `Identity.Roles`, not just what was requested), the test must round-trip: mint through `cli.Run(["token","create",...])` against the fake server, scrape the raw token out of stdout, then construct `auth.NewAuthenticator(store.NewAuthTokenStore(fakeSrv.Client(), fakeSrv.URL), nil).Verify(ctx, rawToken)` and assert on the returned `Identity.Roles`. This exercises the real `Issue` → `normalizeRoles` → persist → `GetByHash` → `Verify` chain, proving the flag actually reaches the auth layer (not just that a string got parsed).

## DW Verification

| DW-ID | Done-When Item | Status | Test Cases |
|-------|---------------|--------|------------|
| DW-1.1 | `token create ... --roles admin,harvester` mints a token; verifying it yields `Roles == ["admin","harvester"]` (order-normalized) | COVERED | `TestDW_1_1_TokenCreateRolesFlagVerifies` — round-trip mint+verify against fake OpenSearch, asserts `Identity.Roles == []string{"admin","harvester"}` |
| DW-1.2 | `token create` with no `--roles` mints a role-less token exactly as before (regression) | COVERED | `TestDW_1_2_TokenCreateNoRolesFlagIsRoleless` — same round-trip, no `--roles` flag, asserts `Identity.Roles` is empty (nil/len 0) |
| DW-1.3 | dirty input `--roles " admin , , harvester "` normalizes to `["admin","harvester"]` (no empty/whitespace entries) — table test | COVERED | `TestDW_1_3_TokenCreateRolesNormalization` — table test over dirty inputs (padded, duplicate, empty-segment, whitespace-only) round-tripped through mint+verify, each asserting the final normalized `Identity.Roles` |

**All items COVERED:** YES

## Design Decisions

**Flag parsing:** add `roles := fs.String("roles", "", "comma-separated role list (e.g. admin,harvester)")` to `runTokenCreate`'s `flag.FlagSet`, alongside the existing `tenant`/`user`/`agent`/`ttl`/`url` flags.

**Splitting/normalization split of responsibility:** `internal/auth.normalizeRoles` (already implemented, out of file scope) is the authoritative sanitizer — it trims each entry, drops empties, dedupes preserving first-seen order, and is applied unconditionally inside `TokenIssuer.Issue` before persistence. The CLI boundary therefore only needs to turn the raw flag string into a `[]string` for `Identity.Roles`; it does not need to duplicate per-entry trimming/dedup, since that would be redundant work superseded by the barricade's own normalization (cc-defensive-programming: barricade reduces redundant validation without removing defense-in-depth — the auth-layer barricade already re-normalizes on both mint and read paths).

A small `parseRoles(raw string) []string` helper is added to `cli.go`:
```go
func parseRoles(raw string) []string {
    raw = strings.TrimSpace(raw)
    if raw == "" {
        return nil
    }
    return strings.Split(raw, ",")
}
```
- Trims the *overall* flag value first so an empty/whitespace-only `--roles` (or the flag omitted, default `""`) returns `nil` — never a `[]string{""}` single-empty-element slice — matching DW-1.2's "exactly as before" requirement structurally, not just after auth-layer cleanup.
- Individual entries are left un-trimmed/undeduped; `normalizeRoles` inside `Issue` handles that. This keeps the CLI thin and avoids two independent implementations of the same sanitization rule (DRY, and less surface for the two to drift on edge cases like Unicode whitespace).

This satisfies the phase note ("decide whether to also trim at the CLI boundary or rely on Issue's normalization; either is fine") by relying on Issue's normalization for per-entry cleanup while still guarding the empty/whitespace-only case explicitly at the boundary, since that's the one case where *not* guarding it would change `Identity.Roles`'s shape (`[]string{""}` vs `nil`) before it ever reaches `Issue` — `normalizeRoles([]string{""})` also returns `nil`, so behaviorally both approaches converge, but the explicit guard reads more obviously as "no --roles means no roles" at the point of decision.

**Test harness:** a minimal fake OpenSearch `httptest.Server` local to `cli_test.go`, serving only `PUT .../_doc/{id}?refresh=true` and `GET .../_doc/{id}` (the two endpoints `AuthTokenStore.Put`/`GetByHash` issue) backed by an in-memory `map[string]map[string]any`. This is narrower than `internal/store/opensearch_test.go`'s `fakeOS` (which also serves `_search`, `_create`, `_update` for the broader store surface) because `token create` never lists or revokes.

**Usage banner:** update the `token create` usage line (cli.go:77) to include `[--roles R1,R2]` so `engram help` documents the new flag, consistent with the file's existing practice of keeping `const usage` in sync with actual flags (see the `ingest` line documenting `--scope`/`--team`).

## Prerequisites
- [x] Required files exist (`internal/cli/cli.go`, `internal/cli/cli_test.go`)
- [x] Dependencies available (`internal/auth`, `internal/store` already implement everything downstream)
- [x] No missing prerequisites

## Recommendation
BUILD. The phase is a small, additive flag-plumbing change entirely within `internal/cli/cli.go`'s existing `runTokenCreate` handler; the auth-layer normalization and persistence it depends on is already implemented and tested. Main implementation work: the `--roles` flag + `parseRoles` helper in `cli.go`, and a minimal fake-OpenSearch test harness in `cli_test.go` to prove the flag reaches a *verified* `Identity.Roles` end-to-end.
