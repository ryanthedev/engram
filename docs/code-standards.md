<!-- base-commit: dece1dba83a314841d990f9582b9204e38964170 -->
<!-- generated: 2026-10-10 -->
# Code Standards

Engram is a Go / gRPC / OpenSearch memory-and-knowledge server. Business logic sits in
`internal/<concern>` behind small seam interfaces; OpenSearch is one implementation reached
over raw `net/http`. The rules below are what deviates from mainstream Go — the parts a
generator gets wrong.

## 1. Forbidden Patterns

**Never import a transport/framework package into a business package.** `google.golang.org/grpc`
and the generated `api/engrampb` are allowed only at five named edges, enforced by a hermetic
tree-walk checker (`go test ./internal/importlint/...`, `_test.go` files exempt), not convention.
```go
// internal/importlint/importlint.go:48-62 — DefaultConfig().AllowedTransportDirs.
AllowedTransportDirs: []string{
    "internal/server", "internal/authgrpc", "internal/engramclient",
    "internal/mcp", "internal/telemetrygrpc",
}
```
`internal/cli` is *not* allowlisted, so gRPC status classification for CLI callers lives at the
edge as an `engramclient.IsXxx` predicate:
```go
// internal/engramclient/knowledge.go:144,149 — status.Code() lives at the edge, not in cli.
func IsAlreadyExists(err error) bool    { return status.Code(err) == codes.AlreadyExists }
func IsPermissionDenied(err error) bool { return status.Code(err) == codes.PermissionDenied }
// BAD: internal/cli calling status.Code(err) directly — importlint fails the build.
```

**Never hard-delete or blind-overwrite a semantic/episodic fact on the normal write path.**
`memory.SemanticFact` is append-only with a guarded close; an optimistic-concurrency loss is
`store.ErrConflict` and the caller re-reads, never retries blind. There are exactly **two
documented exceptions**, each announced in its own doc comment — do not "fix" either:
```go
// internal/store/knowledge.go:17-24 — knowledge tier: mutable harvested content, so
// upsert-by-id + real delete are correct. "Do not 'fix' this back to op_type=create".

// internal/store/purge.go:42-48 — memory tier: PurgeEvent is the operator escape hatch for a
// bad bulk ingest. Episodic + ledger rows are HARD-deleted; semantic facts are SOFT-deleted by
// stamping expired_at (once; already-expired facts are not re-stamped).
```
`retract:` (the reconciler) is the *normal* removal path: it ADDS a retracting fact and never
deletes. Purge is deliberately unreachable by an LLM: no MCP tool (`internal/server/purge.go:10-13`),
role-gated on `RoleMemoryAdmin` (distinct from knowledge `admin`), and `engram purge` is a dry
run unless `--confirm` is passed (`internal/cli/purge.go:47`).

**Never post-filter a kNN query by ACL/tenancy.** The filter goes *inside* the `knn` clause;
filtering after the fact collapses recall:
```go
// internal/retrieval/opensearch.go:723 — filter nested in the knn body.
inner["filter"] = map[string]any{"bool": map[string]any{"filter": opts.filters}}
```

**Never let an ACL denial leak whether a record exists.** `Read` fetches the full record,
authorizes, *then* projects ACL fields away; unknown id, cross-tenant, and ACL denial all
return one indistinguishable sentinel:
```go
// internal/server/read.go:68
var errReadNotFound = status.Error(codes.NotFound, "record not found")
```

**Never compare/dedupe user-derived filenames without NFC-normalizing first** — APFS
collision-folds NFC/NFD-distinct names (bug `995f33a`). `internal/cli/export.go`'s
`safeNoteName` folds first; trap test `TestSafeNoteName_NFCFoldPreventsSilentDrop`.

**Never use `fs.Parse` alone for CLI verbs with positionals.** Go's `flag` stops at the first
non-flag, silently dropping `engram search QUERY -k 3`'s `-k`. Use `parseInterleaved`:
```go
// internal/cli/args.go:21 — flags before AND after positionals; unknown trailing flag errors.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error)
```

## 2. Code Examples

### An OpenSearch store write method
```go
// DO — internal/store/opensearch.go:149 (Create). Normalize scope, run the write-guard
// barricade, then map OS status codes to domain errors — only a 409 becomes ErrConflict.
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

### A search tool handler: budget-pack, then spill on overflow
```go
// DO — internal/mcp/tools.go:366-372, shared by memory_search and knowledge_search.
// Oversized results are packed against a byte budget; omitted hits spill to disk and the
// tool returns overflow_path instead of truncating silently.
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
// DO — internal/server/server.go:137,180. Thin: derive identity, call the seam, map the
// seam's sentinel errors to gRPC codes. New destructive RPCs follow server/purge.go's order:
// authorize FIRST, then validate fields, then call the seam (unauthorized callers learn nothing).
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
// internal/engramclient/knowledge.go:43 and internal/store/purge.go:179 — same house shape.
return fmt.Errorf("engramclient: encoding fields of doc %q: %w", d.ID, err)
return 0, fmt.Errorf("store: purging %s tier: %w", tier, err)
```

Control-flow errors are exported sentinels documented "match with errors.Is"; the boundary maps
them. A guard denial is returned **unwrapped** so the sentinel survives to the gRPC edge.
```go
// internal/knowledge/knowledge.go:19,24
var ErrNotFound = errors.New("knowledge: collection not found")
var ErrConflict = errors.New("knowledge: collection conflict")
```

OpenSearch HTTP status is mapped explicitly, never assumed. Only `index_not_found_exception` is
translated to an empty/zero result; every other status stays a loud error. One predicate per
package, reused by every method (`internal/retrieval/knowledge.go:189,294` call
`isKnowledgeIndexNotFound`; `internal/store/purge.go` calls `isIndexNotFound` and returns `0, nil`).

`_delete_by_query` / `_update_by_query` need `conflicts=proceed` (stale snapshot after a bulk
upsert, `cd67814`; concurrent worker bumps). `conflicts=proceed` hides conflicts under
`version_conflicts`, so purge checks failures separately (`purgeFailures`,
`internal/store/purge.go:260`).

Fail-closed at trust boundaries: an ACL compile error returns zero results and logs a denial —
the query never runs unfiltered (`internal/retrieval/opensearch.go:331-335`).

## 4. Imports & Dependency Direction

Three `gofmt`/`goimports` groups: stdlib, external, then `github.com/ryanthedev/engram/...`
(aliased `yaml` import sits in the last group, `sources/markdown_dir.go:3-18`).

Dependency direction is enforced three ways: `internal/importlint` (a plain `go test`, gates CI
without any tool installed), `.golangci.yml` depguard (`internal-no-transport`, same allowlist
as globs), and revive. Keep the importlint allowlist and the depguard globs in sync.
- Business packages (`store`, `retrieval`, `memory`, `acl`, `knowledge`, `ingest`, `graph`,
  `experience`, `embed`, `enrich`, `harvester`, `cli`, ...) must not import `grpc` or `engrampb`.
- Transport lives only at the five allowlisted edges; `internal/cli` deliberately is not one.
- Allowlisted is not the same as actively using the exemption: `internal/mcp` has no production
  file that needs `engrampb`. Note `internal/mcp` also owns the neutral seam types
  (`mcp.KnowledgeDoc`, `mcp.Backend`) that `internal/harvester/sources` imports.
- **Seams are consumer-defined.** The interface lives where it is *used*; vendor types never
  appear in its signature, and the implementation is asserted at the satisfier:
```go
// internal/engramclient/client.go:28 — structural satisfaction, checked at compile time.
var _ mcp.Backend = (*Client)(nil)
```
  Others: `server.StatusProbe`, `knowledge.CollectionRegistry` (`internal/knowledge/knowledge.go:122`).

## 5. Testing Patterns

Standard-library `testing` only — **no testify** (not in `go.mod`). Unit tests are co-located in
an external `_test` package (`ingest_test`); white-box tests use `*_internal_test.go` /
`read_internal_test.go` style files when they must reach unexported names.

Table tests: named-case slice + `t.Run` + `got`/`want` `t.Errorf`:
```go
// internal/ingest/rule_test.go:14
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

Tests pinned to a planned decision cite its code (`TestDW_1_3_BuildQueryGoldenMatrix`,
`internal/retrieval/buildquery_golden_test.go:14` — 104 of 181 test files use `TestDW_`).
Regression/edge tests use plain descriptive names (`TestPurgeIsDryRunByDefault`,
`TestMemoryPurgeAuthorizesBeforeValidating`, `TestSafeNoteName_NFCFoldPreventsSilentDrop`);
both coexist by design. 
Integration tests: `//go:build integration` as the first line of `*_integration_test.go`,
excluded from `go test ./...`, run via `make integration` against scratch indices.

In-process gRPC stub testing: instead of mocking `mcp.Backend`, tests start a real
`grpc.NewServer` on a `net.Listener` with a stub embedding `engrampb.UnimplementedEngramServer`
and dial a real `engramclient.Client` (`internal/cli/seedknowledge_test.go:1-9`;
`internal/engramclient/knowledgehit_test.go`).

## 6. Naming Conventions

Constructors are `NewXxx` returning the concrete `*Type`, configured by functional options
(`Option func(*T)`, `WithXxx`):
```go
// internal/store/opensearch.go:25,60
func WithEpisodicIndex(name string) Option
func NewOpenSearchStore(client *http.Client, baseURL string, opts ...Option) *OpenSearchStore
```

Doc comments on **every** exported identifier, interfaces included, are enforced by revive
(`revive.toml`: `exported` with `checkPrivateReceivers`, `package-comments`,
`receiver-naming`, `error-strings`, `context-as-argument`; `make lint` = `go vet` + pinned revive,
generated `api/engrampb` excluded). A new exported name without a comment fails CI.

OpenSearch fields are `snake_case` in struct tags / query maps (`expired_at`, `text_embedding`).
Domain terms are load-bearing: **episodic**/**semantic**/**knowledge** tiers, **fact**, **scope**,
**outbox**, **ledger**, **tenant**/**owner_agent_id**, **overflow_path**, **brain** (a root of
local notes in the markdown-dir harvester), **purge** (memory removal) vs **retract** (adds a
retracting fact) vs **sweep** (knowledge mark-and-sweep). Comments cite decision codes
(`D12`, `DW-4.5`) that index the plan.

## 7. File Organization

```
cmd/<name>/              # one binary per entrypoint (16: engram, engramd, engram-mcp,
                         #   engram-harvester, engram-eval, ...); main.go wires deps only
internal/<concern>/
├── <concern>.go         # the seam interface + domain types (store.go, knowledge.go)
├── opensearch.go        # the OpenSearch implementation of that seam
├── <feature>.go         # cohesive extras (purge.go, enrich.go, ledger.go, outbox.go)
├── *_test.go            # unit tests, co-located, external `_test` package
└── *_integration_test.go # //go:build integration — live-cluster tests
api/engrampb/            # generated protobuf (buf); imported only by edges
```
One package per bounded concern is the default for new packages. Harvest sources are one file
each under `internal/harvester/sources/` (`github_repos.go`, `markdown_dir.go`, `arxiv_*.go`)
with a `_test.go` sibling.

**Known exceptions — don't "correct" them.** The whole vault-export feature (`export.go`,
`vault*.go`, `sanitize.go`, `seedknowledge.go`) lives inside `internal/cli/`, not a separate
package; the operator verbs (`purge.go`, `knowledge.go`) sit there too. Each RPC family gets
its own file next to `server.go` (`server/knowledge.go`, `server/purge.go`, `server/read.go`),
with the thin `engramclient` mirror in `engramclient/<family>.go`.

## 8. Technology Decisions

- **Raw `net/http` to the OpenSearch REST API — no OpenSearch Go client.** Queries are nested
  `map[string]any` + `json.Marshal`, including purge and knowledge aggregations. Do not add an SDK.
- **`go.yaml.in/yaml/v2` for YAML, two styles.** `yaml.UnmarshalStrict` for config/manifests
  (unknown fields rejected on purpose; `internal/harvester/manifest.go`, markdown-dir frontmatter
  in `sources/markdown_dir.go`); `yaml.MapSlice` where emitted key *order* matters
  (`internal/cli/vaultnotes.go`).
- **Optimistic concurrency for the memory tier** (`op_type=create`, `if_seq_no` +
  `if_primary_term`; 409 -> `ErrConflict`). Knowledge tier upserts by id and sweeps with
  `conflicts=proceed`. Purge is the only memory-tier delete (see Forbidden Patterns).
- **Embeddings are text-first.** Append writes text immediately (BM25 serves it); the
  background `enrich` job fills `text_embedding` (`store.FindUnembedded`, `internal/enrich`).
  The embedding model runs on the host GPU via `engram-embed-server`, not in the container.
- **RBAC by role on `auth.Identity`.** `internal/knowledgeauth` gates knowledge writes
  (`admin`/`harvester`); memory purge uses its own `memory-admin` role — roles never cross tiers.
- **Knowledge collections are runtime-mutable** via `knowledge.CollectionRegistry` (meta-index,
  provisioning, atomic alias swap); search supports fragments, offset paging, and exact totals.
- **The MCP server is hand-rolled** stdio JSON-RPC 2.0 (`internal/mcp/mcp.go`), all tools behind
  the `mcp.Backend` seam that `engramclient.Client` satisfies. Destructive ops are never tools.
- **gRPC + protobuf via buf** (`make proto`, `proto-check`); `context.Context` first arg on every
  I/O call; `log/slog` for logging; config via `flag` with `ENGRAM_*` env defaults.

## 9. Exemplar Files

**`internal/store/opensearch.go`** — append-only write seam: functional-option constructor,
write-guard barricade, `op_type=create`/`if_seq_no` writes, status -> `ErrConflict`.

**`internal/store/purge.go`** — the model for a sanctioned destructive path: exception stated in
the doc comment, per-tier mechanism with reasoning, dry-run via `_count`, `conflicts=proceed`,
failure inspection, path-traversal guarded index names.

**`internal/engramclient/client.go` + `knowledge.go` + `purge.go`** — the shared gRPC client edge:
token attachment, proto<->domain translation, `IsXxx` predicates for non-allowlisted callers.

**`internal/mcp/tools.go` + `budget.go` + `render.go`** — tool handlers, byte-budget packing,
overflow spill, one-line hit rendering.

**`internal/server/server.go`, `read.go`, `purge.go`** — thin handlers: sentinel -> gRPC code,
fetch-authorize-project, authorize-before-validate.

**`internal/cli/purge.go` + `args.go`** — operator verb shape: interleaved flags, dry-run default,
explicit `--confirm`, guardrails documented in the header comment.
