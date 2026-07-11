<!-- base-commit: 8ed5258 -->
<!-- generated: 2026-07-10 -->
# Code Standards

Engram is a Go / gRPC / OpenSearch memory server. Business logic sits in `internal/<concern>`
behind small seam interfaces; OpenSearch is one implementation reached over raw `net/http`.
The rules below are what deviates from mainstream Go — the parts a generator gets wrong.

## 1. Forbidden Patterns

**Never import a transport/framework package into a business package.** `google.golang.org/grpc`
and the generated `api/engrampb` are allowed only at the named edges (`internal/server`,
`internal/authgrpc`, `internal/telemetrygrpc`, `cmd/`). `internal/importlint` fails CI on any
other `internal/` package that imports them.
```go
// BAD — internal/retrieval importing the proto API couples the read path to the wire format.
import "github.com/ryanthedev/engram/api/engrampb"
// GOOD — the server (an edge) translates; retrieval speaks its own retrieval.Hit type.
// internal/server/server.go:144 marshals retrieval.Hit -> engrampb.Hit at the boundary.
```

**Never hard-delete or blind-overwrite a semantic fact.** The store is append-only with a guarded
close (`invalid_at` stamp). Every optimistic-concurrency loss surfaces as `store.ErrConflict`; the
caller re-reads and re-reconciles — it never retries a blind write.
```go
// store.go:70 — the Store contract states it directly:
// "must surface every optimistic-concurrency loss as ErrConflict ... and must never
//  hard-delete a semantic fact."
```

**Never trust client-supplied tenancy or scope.** When the auth barricade injected a verified
Identity, it is authoritative; request fields are a fallback only for in-process/test callers.
```go
// BAD — trusting req.GetTenantId() lets a token write another tenant's data.
// GOOD — server/server.go:92: the verified Identity overrides the request.
tenantID, ownerAgentID := req.GetTenantId(), req.GetOwnerAgentId()
if id, ok := authgrpc.IdentityFrom(ctx); ok {
    tenantID, ownerAgentID = id.TenantID, id.AgentID // Identity wins
}
```

**Never post-filter a kNN query by ACL/tenancy.** The filter goes *inside* the `knn` clause;
filtering after the fact collapses recall (a Phase-0 spike finding).
```go
// retrieval/opensearch.go:509 — filter nested in the knn body, not applied to results.
inner := map[string]any{"vector": vec, "k": k}
if len(filters) > 0 {
    inner["filter"] = map[string]any{"bool": map[string]any{"filter": filters}}
}
```

## 2. Code Examples

### An OpenSearch store write method
```go
// DO — from internal/store/opensearch.go:149 (Create).
// Normalize scope, run the write-guard barricade, then map OS status codes to
// domain errors — a 409 create-collision becomes ErrConflict, nothing else does.
func (s *OpenSearchStore) Create(ctx context.Context, id string, f memory.SemanticFact) error {
    f.Scope = normalizeScope(f.Scope)
    if err := s.authorizeWrite(ctx, acl.Record{ /* tenancy from f */ }); err != nil {
        return err // guard denial returned verbatim so errors.Is finds the sentinel
    }
    body, err := json.Marshal(f)
    if err != nil {
        return fmt.Errorf("store: encoding semantic fact: %w", err)
    }
    url := fmt.Sprintf("%s/%s/_create/%s", s.baseURL, s.semanticIndex, id)
    status, decoded, err := doJSON(ctx, s.client, http.MethodPut, url, body)
    if err != nil {
        return fmt.Errorf("store: creating semantic fact %s: %w", id, err)
    }
    switch status {
    case http.StatusCreated, http.StatusOK:
        return nil
    case http.StatusConflict:
        return fmt.Errorf("store: creating semantic fact %s: %w", id, ErrConflict)
    default:
        return fmt.Errorf("store: creating semantic fact %s: unexpected status %d: %v", id, status, decoded)
    }
}
```

### A gRPC handler translating domain errors to status codes
```go
// DO — from internal/server/server.go:108. The handler is thin: derive identity,
// call the seam, translate the seam's sentinel errors to gRPC codes. No policy detail leaks.
id, err := s.Store.Append(ctx, rec)
if err != nil {
    if errors.Is(err, acl.ErrScopeDenied) || errors.Is(err, acl.ErrUnknownScope) {
        return nil, status.Error(codes.PermissionDenied, "not authorized to write this scope")
    }
    return nil, status.Errorf(codes.Internal, "appending episodic event: %v", err)
}
```

## 3. Error Handling

Errors are returned and wrapped, never panicked in library code. Every wrap is prefixed with the
package name and states the operation: `fmt.Errorf("store: creating semantic fact %s: %w", id, err)`.
```go
// retrieval/opensearch.go:428 — "pkg: verb-ing noun: %w" is the house shape everywhere.
return nil, fmt.Errorf("retrieval: searching %s: %w", t.index, err)
```

Control-flow errors are exported sentinels checked with `errors.Is`; the boundary maps them.
```go
// store/store.go:20 — one sentinel for every optimistic-write loss.
var ErrConflict = errors.New("store: version or create conflict")
// A guard denial is returned UNWRAPPED so the typed acl.ErrScopeDenied survives errors.Is
// up at the gRPC edge (opensearch.go:99). Don't fmt.Errorf-wrap sentinels you need to match.
```

OpenSearch HTTP status is mapped explicitly, never assumed. Exactly one 404 shape —
`index_not_found_exception` — is translated to an empty result (a not-yet-created index is empty,
not broken); every other status stays a loud error.
```go
// store/opensearch.go:208 — isIndexNotFound matches ONLY that one error.type; all else surfaces.
```

Fail-closed at trust boundaries: an ACL compile error returns zero results and logs a denial —
the query never runs unfiltered (`retrieval/opensearch.go:211`).

## 4. Imports & Dependency Direction

Three `gofmt`/`goimports` groups: stdlib, external, then `github.com/ryanthedev/engram/...`.
```go
import (
    "context"
    "net/http"

    "google.golang.org/grpc/codes"

    "github.com/ryanthedev/engram/internal/acl"
    "github.com/ryanthedev/engram/internal/store"
)
```

Dependency direction is enforced, not conventional:
- Business packages (`store`, `retrieval`, `memory`, `acl`, `ingest`, `graph`, `experience`, `embed`)
  must not import `grpc` or `engrampb`. Checked by `internal/importlint` (`DefaultConfig`) and revive.
- Transport lives only at edges: `internal/server`, `internal/authgrpc`, `internal/telemetrygrpc`, `cmd/`.
- **Seams are consumer-defined.** The interface lives where it is *used*, and OpenSearch/vendor types
  never appear in its signatures. `server.StatusProbe` and `mcp.Backend` are declared by the consumer;
  `*store.OpenSearchStore` and a gRPC adapter satisfy them from the other side.

## 5. Testing Patterns

Standard-library `testing` only — **there is no testify** (`go.mod` has no `stretchr/testify`).
Table tests use a named-case slice + `t.Run` + `got`/`want` `t.Errorf`.
```go
// internal/ingest/rule_test.go:14 — the canonical shape. External test package (ingest_test).
tests := []struct {
    name string
    line string
    want bool
}{
    {"fact prefix", "fact: alice | likes | tea", true},
    {"plain prose", "alice really likes her tea", false},
}
for _, tt := range tests {
    t.Run(tt.name, func(t *testing.T) {
        if got := ingest.IsFactDirectiveLine(tt.line); got != tt.want {
            t.Errorf("IsFactDirectiveLine(%q) = %v, want %v", tt.line, got, tt.want)
        }
    })
}
```

Tests are named after the decision/requirement they pin (`TestDW_3_7_GreenOnCleanTree`,
"DW-4.3" in case names) so a failure names the contract it broke.

Integration tests hit a live OpenSearch behind a build tag and use a scratch index (via the
`WithEpisodicIndex`/`WithIndices` options) so they can't collide with production or each other.
```go
//go:build integration    // first line of *_integration_test.go; excluded from the default `go test`.
// Helpers pull the module root with testutil.RepoRoot(t); unit tests never touch the network.
```

## 6. Naming Conventions

Constructors are `NewXxx` returning the concrete `*Type`, configured by functional options —
`Option func(*T)` (or `func(*config)`) with `WithXxx` builders and package-level defaults.
```go
// store/opensearch.go:19,25,60 — the pattern repeated across store, retrieval, graph.
type Option func(*OpenSearchStore)
func WithSemanticIndex(name string) Option { return func(s *OpenSearchStore) { s.semanticIndex = name } }
func NewOpenSearchStore(client *http.Client, baseURL string, opts ...Option) *OpenSearchStore { ... }
```

Compile-time interface satisfaction is asserted next to the type:
```go
var _ Store = (*OpenSearchStore)(nil)     // store/opensearch.go:55
var _ Retriever = (*MultiRetriever)(nil)  // retrieval/opensearch.go:171
```

Receivers are short and consistent per type (`s` store/server, `t` tierRetriever, `m` MultiRetriever,
`b` backend) — revive's `receiver-naming` enforces consistency. OpenSearch document/JSON fields are
`snake_case` in struct tags (`owner_agent_id`, `tenant_id`, `valid_at`, `if_seq_no`). Domain terms
are load-bearing: **episodic**/**semantic** tiers, **fact**, **scope**, **outbox**, **ledger**,
**tenant**/**owner_agent_id**. Comments cite decision codes (`D12`, `DW-4.5`) that index the plan.

## 7. File Organization

```
cmd/engram-<name>/       # one binary per entrypoint (engram-server, engram-mcp, ...); main.go wires deps
internal/<concern>/
├── <concern>.go         # the seam interface + domain types (store.go, retriever.go)
├── opensearch.go        # the OpenSearch implementation of that concern's seam
├── <feature>.go         # cohesive extras (facts.go, ledger.go, outbox.go, budget.go)
├── *_test.go            # unit tests, co-located, external `_test` package
└── *_integration_test.go # //go:build integration — live-cluster tests
api/engrampb/            # generated protobuf (buf); imported only by edges
```
One package per bounded concern. A concern's OpenSearch code always lives in that package's
`opensearch.go`; the seam and types it implements live in `<concern>.go`.

## 8. Technology Decisions

- **Raw `net/http` to the OpenSearch REST API — no official OpenSearch Go client.** Each package
  carries its own thin JSON helper (`doJSON` in store, `osDo`/`osJSON` in graph). Queries are built as
  `map[string]any` and `json.Marshal`ed. Do not add an OpenSearch SDK dependency.
- **Writes use OpenSearch optimistic concurrency.** New docs: `op_type=create` (`_create/{id}`);
  updates: `if_seq_no`+`if_primary_term` guards. A 409 is `ErrConflict`, retried by re-read (never
  blind overwrite). The episodic log is the append-only outbox (D12); extraction is claim-first via a
  ledger index (D13).
- **Hybrid search = BM25 + kNN fused server-side by an RRF search pipeline** (`store.RRFPipelineName`
  = `engram-rrf`), attached as `?search_pipeline=` only for hybrid mode with a usable vector. Query
  embeddings are bounded (`DefaultEmbedTimeout` 50ms); on timeout the tier degrades to BM25-only, it
  does not fail. `K` from external callers is clamped to `[1, MaxK]`.
- **The MCP server is hand-rolled** (stdio JSON-RPC 2.0, `internal/mcp/mcp.go`), not the official SDK,
  behind the `mcp.Backend` seam so the SDK can replace it later without touching callers. Tool-level
  failures return an MCP result with `isError=true`; only protocol misuse is a JSON-RPC error.
- **gRPC + protobuf via buf**; `context.Context` is the first arg on every I/O and RPC call.
- **Structured logging with `log/slog`** (`slog.Default()`, `WarnContext`); config via `flag` with
  `ENGRAM_*` env-var defaults (`cmd/engram-server/main.go:49`).

## 9. Exemplar Files

**`internal/store/opensearch.go`** — the write seam over OpenSearch: functional-option constructor,
the write-guard barricade, `op_type=create` / `if_seq_no` guarded writes, status→`ErrConflict`
mapping, and the single-shape `index_not_found` translation. The template for any new store method.

**`internal/retrieval/opensearch.go`** — the read seam: concurrent per-tier fan-out, RRF hybrid query
construction, ACL-inside-the-knn-clause filtering, fail-closed authorization ordered *before*
truncation, and response-field projection to a display allowlist. The template for a new retriever/tier.

**`internal/server/server.go`** — the gRPC edge: thin handlers that resolve the verified Identity,
call the `Store`/`Retriever` seams, and translate domain sentinels to gRPC status codes. The template
for wiring a new RPC without leaking transport into business packages.
