<!-- base-commit: fea066992980a8fc93b7aa9923cbd1cf63d75ff6 -->
<!-- generated: 2026-07-23 -->
# Code Standards

Engram is a Go / gRPC / OpenSearch memory-and-knowledge server. Business logic sits in
`internal/<concern>` behind small seam interfaces; OpenSearch is one implementation reached
over raw `net/http`. The rules below are what deviates from mainstream Go — the parts a
generator gets wrong.

## 1. Forbidden Patterns

**Never import a transport/framework package into a business package.** `google.golang.org/grpc`
and the generated `api/engrampb` are allowed only at five named edges, enforced by a hermetic
tree-walk checker, not just convention.
```go
// internal/importlint/importlint.go:47-58 — AllowedTransportDirs. Anything else under
// internal/ that imports grpc or engrampb fails `go test ./internal/importlint/...` in CI.
AllowedTransportDirs: []string{
    "internal/server", "internal/authgrpc", "internal/engramclient",
    "internal/mcp", "internal/telemetrygrpc",
}
```
`internal/engramclient` is the newest edge — the shared gRPC client the MCP server and CLI use
to reach `engramd`. Because `internal/cli` is *not* allowlisted, gRPC status-code classification
for CLI callers can't live in `internal/cli` — call an `engramclient.IsXxx` predicate instead:
```go
// internal/engramclient/knowledge.go:129-141 — status.Code() lives at the edge, not in cli.
func IsAlreadyExists(err error) bool      { return status.Code(err) == codes.AlreadyExists }
func IsPermissionDenied(err error) bool   { return status.Code(err) == codes.PermissionDenied }
// BAD: internal/cli checking status.Code(err) directly — importlint fails the build.
```

**Never hard-delete or blind-overwrite a semantic/episodic fact.** `memory.SemanticFact` is
append-only with a guarded close (`invalid_at` stamp); every optimistic-concurrency loss
surfaces as `store.ErrConflict`, and the caller re-reads and re-reconciles, never retries blind.
**This rule does NOT apply to the knowledge tier** — `KnowledgeStore` is the one documented
exception, and the code says so explicitly:
```go
// internal/store/knowledge.go:17-24
// KnowledgeStore ... is Engram's ONE intentional, documented deviation from
// append-only memory writes (docs/code-standards.md's "never hard-delete or
// blind-overwrite" rule governs memory.SemanticFact only — see store.go).
// Knowledge documents are mutable harvested content, not reconciled facts,
// so upsert + real delete are correct here. Do not "fix" this back to
// op_type=create / invalid_at.
```

**Never post-filter a kNN query by ACL/tenancy.** The filter goes *inside* the `knn` clause;
filtering after the fact collapses recall (a Phase-0 spike finding), still true today:
```go
// internal/retrieval/opensearch.go:677-679 — filter nested in the knn body.
inner["filter"] = map[string]any{"bool": map[string]any{"filter": filters}}
```

**Never let an ACL denial leak whether a record exists.** `Read` fetches the full record
(including ACL fields), authorizes, *then* projects ACL fields away — every denial reason
collapses to one indistinguishable sentinel:
```go
// internal/server/read.go:61,79,84,95 — unknown id, cross-tenant, and ACL denial
// all return the SAME error so existence never leaks through the message.
errReadNotFound = status.Error(codes.NotFound, "record not found")
```

**Never compare/dedupe user-derived filenames without NFC-normalizing first** — APFS silently
collision-folds NFC/NFD-distinct Unicode names; a real bug (`995f33a`) lost vault notes this way.
`internal/cli/export.go`'s `safeNoteName` NFC-folds before writing; see the trap test
`export_test.go: TestSafeNoteName_NFCFoldPreventsSilentDrop`.

## 2. Code Examples

### An OpenSearch store write method
```go
// DO — from internal/store/opensearch.go:149 (Create). Normalize scope, run the
// write-guard barricade, then map OS status codes to domain errors — a 409
// create-collision becomes ErrConflict, nothing else does.
func (s *OpenSearchStore) Create(ctx context.Context, id string, f memory.SemanticFact) error {
    f.Scope = normalizeScope(f.Scope)
    if err := s.authorizeWrite(ctx, acl.Record{ /* tenancy from f */ }); err != nil {
        return err // guard denial returned verbatim so errors.Is finds the sentinel
    }
    status, decoded, err := doJSON(ctx, s.client, http.MethodPut, url, body)
    switch status {
    case http.StatusCreated, http.StatusOK:
        return nil
    case http.StatusConflict:
        return fmt.Errorf("store: creating semantic fact %s: %w", id, ErrConflict)
    default:
        return fmt.Errorf("store: unexpected status %d: %v", status, decoded)
    }
}
```

### A knowledge tool handler: budget-pack, then spill on overflow
```go
// DO — from internal/mcp/tools.go:317-340, shared by memory_search and knowledge_search.
// Oversized result sets are packed against a byte budget; anything omitted spills to
// disk and the tool returns overflow_path instead of truncating silently.
result := packSearchResult(hits, searchByteBudget(), facetFields)
if result.Omitted > 0 {
    if path, spillErr := spillFullResult(hits); spillErr != nil {
        result.Hint = refineHint(...) // spill failed — fall back to a narrower hint
    } else {
        result.OverflowPath = path
    }
}
```

### A gRPC handler translating domain errors to status codes
```go
// DO — from internal/server/server.go:132-135,175-178. The handler is thin: derive
// identity, call the seam, translate the seam's sentinel errors to gRPC codes.
if errors.Is(err, acl.ErrScopeDenied) || errors.Is(err, acl.ErrUnknownScope) {
    return nil, status.Error(codes.PermissionDenied, "not authorized to write this scope")
}
if errors.Is(err, retrieval.ErrInvalidFilter) {
    return nil, status.Error(codes.InvalidArgument, err.Error())
}
```

## 3. Error Handling

Errors are returned and wrapped, never panicked in library code. Every wrap is prefixed with
the package name and states the operation: `"pkg: verb-ing noun: %w"`.
```go
// internal/engramclient/knowledge.go:43 — the house shape holds in the newest package too.
return fmt.Errorf("engramclient: encoding fields of doc %q: %w", id, err)
```

Control-flow errors are exported sentinels checked with `errors.Is`; the boundary maps them.
A guard denial is returned **unwrapped** so the typed sentinel survives `errors.Is` up at the
gRPC edge — don't `fmt.Errorf`-wrap sentinels you need to match.
```go
// internal/knowledge/knowledge.go:17-24 — same doc-comment pattern as store.ErrConflict:
// "Returned wrapped with %w — match with errors.Is."
var ErrNotFound = errors.New("knowledge: not found")
var ErrConflict = errors.New("knowledge: version conflict")
```

OpenSearch HTTP status is mapped explicitly, never assumed. Exactly one 404 shape —
`index_not_found_exception` — is translated to an empty result; every other status stays a
loud error. The same predicate now exists per-retriever, not just for the original store:
```go
// internal/retrieval/knowledge.go:360-364
func isKnowledgeIndexNotFound(status int, decoded map[string]any) bool { ... }
```

**Production gotcha worth knowing:** `_delete_by_query` needs `conflicts=proceed`. A
mark-and-sweep running right after a bulk upsert sees a stale point-in-time snapshot and hits
`version_conflict` on rows that were legitimately just re-touched (`cd67814`, found in e2e).

Fail-closed at trust boundaries: an ACL compile error returns zero results and logs a denial —
the query never runs unfiltered (`internal/retrieval/opensearch.go:211`).

## 4. Imports & Dependency Direction

Three `gofmt`/`goimports` groups: stdlib, external, then `github.com/ryanthedev/engram/...`.

Dependency direction is enforced, not conventional (`internal/importlint`, mirrored in
`.golangci.yml` depguard):
- Business packages (`store`, `retrieval`, `memory`, `acl`, `knowledge`, `ingest`, `graph`,
  `experience`, `embed`, `harvester`, `cli`, ...) must not import `grpc` or `engrampb`.
- Transport lives only at the five allowlisted edges: `internal/server`, `internal/authgrpc`,
  `internal/engramclient`, `internal/mcp`, `internal/telemetrygrpc`.
- `internal/mcp` is allowlisted but currently has **zero** production files that need it
  (only `render_test.go` imports `engrampb`) — allowlisted ≠ actively using the exemption.
- `internal/cli` is deliberately **not** allowlisted. gRPC status-code checks for CLI code
  must go through an `engramclient.IsXxx` predicate (see Forbidden Patterns).
- **Seams are consumer-defined.** The interface lives where it is *used*; vendor types never
  appear in its signature. `server.StatusProbe`, `mcp.Backend`, and the newer
  `knowledge.CollectionRegistry` (`internal/knowledge/knowledge.go:85-101`) are all declared by
  the consumer and satisfied structurally from the other side (e.g. `*store.OpenSearchStore`,
  `*engramclient.Client`).

## 5. Testing Patterns

Standard-library `testing` only — **there is no testify** (`go.mod` has no `stretchr/testify`).

Table tests use a named-case slice + `t.Run` + `got`/`want` `t.Errorf`:
```go
// internal/ingest/rule_test.go:14 — the canonical shape. External test package (ingest_test).
tests := []struct{ name, line string; want bool }{
    {"fact prefix", "fact: alice | likes | tea", true},
}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        if got := ingest.IsFactDirectiveLine(tt.line); got != tt.want {
            t.Errorf("IsFactDirectiveLine(%q) = %v, want %v", tt.line, got, tt.want)
        }
    })
}
```

Tests tied to a specific planned decision cite its code in the name
(`TestDW_3_1_SeedIssuesCreateThenIngest`) — 49 of 71 test files touched since base still use
this. Auxiliary/edge-case/regression tests use plain descriptive names in the same files
(`TestBulkIndexRejectsPathTraversalIndex`, `TestSafeNoteName_NFCFoldPreventsSilentDrop`) — both
coexist by design; a descriptive name is correct when the test isn't pinning one decision.

Integration tests hit a live OpenSearch behind `//go:build integration` (first line of
`*_integration_test.go`, excluded from default `go test`) on scratch indices.

**New shared convention: in-process gRPC stub testing.** Rather than mocking at `mcp.Backend`,
white-box tests spin up a real `grpc.NewServer` on a `net.Listener` backed by a stub embedding
`engrampb.UnimplementedEngramServer`, then dial a real `engramclient.Client` against it —
`internal/cli/seedknowledge_test.go:1-9` names this "the same convention `export_test.go` uses."

## 6. Naming Conventions

Constructors are `NewXxx` returning the concrete `*Type`, configured by functional options —
`Option func(*T)` with `WithXxx` builders. Compile-time interface satisfaction is asserted next
to the type:
```go
// internal/engramclient/client.go:27
var _ mcp.Backend = (*Client)(nil)
```

Receivers are short and consistent per type, enforced by revive's `receiver-naming`.
OpenSearch document/JSON fields are `snake_case` in struct tags. Domain terms are load-bearing:
**episodic**/**semantic**/**knowledge** tiers, **fact**, **scope**, **outbox**, **ledger**,
**tenant**/**owner_agent_id**, **overflow_path** (search spill), **honest k** (graph expansions
never count against `k`, and are `nil` — not empty — when absent, `internal/mcp/mcp.go:42-58`).
Comments cite decision codes (`D12`, `DW-4.5`) that index the plan.

**Single-choke-point normalization** is a naming/hygiene principle worth internalizing:
`internal/cli`'s `parseRoles` deliberately does *not* re-sanitize role strings — normalization
happens once, at mint time, in `auth.TokenIssuer.Issue`. Comment: *"doing it again here would
just be the same sanitization rule implemented twice with room to drift."*

## 7. File Organization

```
cmd/engram-<name>/       # one binary per entrypoint (14 as of HEAD); main.go wires deps
internal/<concern>/
├── <concern>.go         # the seam interface + domain types (store.go, knowledge.go)
├── opensearch.go         # the OpenSearch implementation of that concern's seam
├── <feature>.go          # cohesive extras (facts.go, ledger.go, outbox.go, seed.go)
├── *_test.go             # unit tests, co-located, external `_test` package
└── *_integration_test.go # //go:build integration — live-cluster tests
api/engrampb/             # generated protobuf (buf); imported only by edges
```
One package per bounded concern, still the default for new packages (`internal/knowledge/`,
`internal/harvester/` with a `sources/` subdir per source implementation).

**Known exception — don't assume every feature gets its own `internal/<concern>/`.** The whole
"dual-primary vault export" feature (`export.go`, `vault.go`, `vaultmodel.go`, `vaultnotes.go`,
`vaultmaps.go`, `vaultknowledge.go`, `sanitize.go`, `seedknowledge.go`) lives inside
`internal/cli/`, not a separate `internal/export/`. If extending vault/export behavior, follow
the existing location rather than "correcting" it into a new package.

## 8. Technology Decisions

- **Raw `net/http` to the OpenSearch REST API — no official OpenSearch Go client.** Queries are
  nested `map[string]any` + `json.Marshal`, including the newer `internal/retrieval/knowledge.go`
  filter/aggregation code. Do not add an OpenSearch SDK.
- **`go.yaml.in/yaml/v2` (direct dep) for YAML, two styles.** `yaml.UnmarshalStrict` for
  config/manifests (`internal/harvester/manifest.go`, `internal/knowledge/seed.go`) — strict
  rejects unknown fields on purpose. `yaml.MapSlice`/`MapItem` for Obsidian frontmatter
  (`internal/cli/vaultnotes.go:88-114`) where emitted key *order* matters and a struct can't say it.
- **Optimistic concurrency for the memory tier only.** `op_type=create` /
  `if_seq_no`+`if_primary_term`; 409 → `ErrConflict`, retried by re-read. Knowledge tier is the
  exception: `KnowledgeStore.BulkIndex` upserts by id, `DeleteByQuery` really deletes with
  `conflicts=proceed` (survives concurrent sweep-vs-upsert races).
- **RBAC via a role dimension on `auth.Identity`.** A dedicated `internal/knowledgeauth`
  authorizer gates knowledge writes by role (`admin`/`harvester`), separate from the
  tenant/scope ACL that governs memory.
- **`CollectionRegistry` makes knowledge collections runtime-mutable** (meta-index +
  provisioning, live reindex + atomic alias swap) — no server restart to add a collection.
- **The MCP server is hand-rolled** (stdio JSON-RPC 2.0, `internal/mcp/mcp.go`), now 10 tools (up
  from 3), all behind the `mcp.Backend` seam that `internal/engramclient.Client` satisfies.
- **Injection barricade for re-used ingested content.** `internal/cli/sanitize.go`'s
  `sanitizeBody`/`quoteBlock` transform (not reject) hostile tokens before untrusted prose is
  written into Obsidian notes — a second-use injection surface, not just the API input.
- **gRPC + protobuf via buf**; `context.Context` first arg on every I/O and RPC call.
- **Structured logging with `log/slog`**; config via `flag` with `ENGRAM_*` env-var defaults.

## 9. Exemplar Files

**`internal/store/opensearch.go`** — append-only write seam: functional-option constructor,
write-guard barricade, `op_type=create`/`if_seq_no` writes, status→`ErrConflict`. Template for
a new *memory-tier* store method.

**`internal/store/knowledge.go`** — the documented exception to append-only: upsert-by-id bulk
writes, mark-and-sweep delete with `conflicts=proceed`. Read its header comment before writing
another delete path.

**`internal/retrieval/knowledge.go`** — newest read seam: per-tier filterable-field registry,
predicate-to-`map[string]any` translation, aggregations, its own `index_not_found` handling.
Template for a new filterable retriever.

**`internal/engramclient/client.go` + `knowledge.go`** — the gRPC edge shared by MCP and CLI:
token attachment, proto↔domain translation, `IsAlreadyExists`/`IsPermissionDenied` predicates
for non-allowlisted callers. Template for a new client-side RPC call.

**`internal/mcp/tools.go`** — tool-handler edge: arg parsing, `toolResult`/`toolResultWithText`,
the shared `packAndSpill` budget-then-overflow pattern. Template for a new MCP tool.

**`internal/server/server.go` + `read.go`** — gRPC edge: thin handlers translating sentinels to
gRPC codes, plus fetch-then-authorize-then-project to keep denials indistinguishable from
not-found. Template for a new RPC.
